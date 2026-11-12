"""Identity-less dynamic pipeline assembly client for the coordinator."""
import hashlib
import json
import sys
import time

import grpc
from transformers import AutoTokenizer

from ember_core import node_id, verify, digest, sign
from sharding import profile_span
from ember_pb2 import PipelineExecutionReceipt, ResourceProfile, StageLatencyWitness
from engine import Step
from evidence import canonical, encode_step, opening
from shard_rpc import StageClient, bytes_tensor, read_scheduler_key


def load_profile(raw):
    profile = ResourceProfile.FromString(bytes.fromhex(raw))
    verify(profile)
    if profile.node_id != node_id(profile.signer):
        raise ValueError('invalid shard profile identity')
    if (not profile.shard_endpoint or len(profile.shard_tls_fingerprint) != 32 or
            profile.shard_end_layer < profile.shard_start_layer):
        raise ValueError('incomplete shard endpoint profile')
    profile_span(profile)
    return profile


def emit(value):
    print(json.dumps(value, separators=(',', ':')), flush=True)


def main():
    job = json.load(sys.stdin)
    tokenizer = AutoTokenizer.from_pretrained(job['model_path'], local_files_only=True,
                                               trust_remote_code=False)
    stages = []
    clients = []
    scheduler_key = read_scheduler_key(job['scheduler_key_file'])
    witnesses=[]
    witness_key=read_scheduler_key(job['witness_key_file']) if job.get('witness_key_file') else None
    for expected_index, item in enumerate(job['stages']):
        profiles = [load_profile(raw) for raw in item['profiles']]
        if not profiles:
            raise ValueError('pipeline stage has no candidates')
        for profile in profiles:
            if profile.shard_dynamic_range:
                if not profile.shard_start_layer <= item['start_layer'] < item['end_layer'] <= profile.shard_end_layer:
                    raise ValueError('pipeline execution exceeds capability')
                continue
            if (profile.shard_index != expected_index or
                    profile.shard_start_layer != item['start_layer'] or
                    profile.shard_end_layer != item['end_layer']):
                raise ValueError('pipeline plan/profile mismatch')
        stage_clients = [StageClient(
            f'{profile.shard_endpoint}@{profile.shard_tls_fingerprint.hex()}',
            scheduler_key, profile=profile, executed_range=(item["start_layer"],item["end_layer"])) for profile in profiles]
        clients.extend(stage_clients)
        stages.append((profiles, stage_clients))

    ids = tokenizer(job['prompt'], return_tensors='pt').input_ids[0].tolist()
    input_tokens = len(ids)
    steps = []
    invocations = []
    failovers = []
    preferred = [0] * len(stages)
    started = time.monotonic()
    try:
        for step_index in range(job['max_tokens']):
            response = None
            activations = {}
            for stage_index, (profiles, stage_clients) in enumerate(stages):
                order = [preferred[stage_index], *[i for i in range(len(stage_clients))
                         if i != preferred[stage_index]]]
                if job.get('stream') and step_index > 0:
                    order = order[:1]
                last_error = None
                for candidate_index in order:
                    client = stage_clients[candidate_index]
                    profile = profiles[candidate_index]
                    call_started = time.monotonic()
                    observed_start = time.monotonic_ns()
                    emit({'type': 'stage_start'})
                    try:
                        if stage_index == 0:
                            response = client.forward(job['request_id'], step_index,
                                                      input_ids=ids, timeout=job['timeout'])
                        else:
                            hidden = bytes_tensor(response.hidden_bf16,
                                response.hidden_shape, 'cpu')
                            response = client.forward(job['request_id'], step_index,
                                                      hidden=hidden, timeout=job['timeout'])
                        observed_end = time.monotonic_ns()
                        emit({'type': 'stage_end', 'stage_receipt_ref': digest(response.receipt).hex()})
                        elapsed = (time.monotonic() - call_started) * 1000
                        if witness_key is not None:
                            witness=StageLatencyWitness(stage_receipt_ref=digest(response.receipt),
                                started_mono_ns=observed_start,completed_mono_ns=observed_end)
                            sign(witness,witness_key);witnesses.append(witness)
                        preferred[stage_index] = candidate_index
                        invocations.append({
                            'request': client.last_request.SerializeToString(
                                deterministic=True).hex(),
                            'response': response.SerializeToString(
                                deterministic=True).hex(),
                            'profile': profile.SerializeToString(
                                deterministic=True).hex(),
                            'stage_ms': elapsed,
                        })
                        activations.update(response.activations)
                        break
                    except (grpc.RpcError, OSError, ValueError) as exc:
                        emit({'type': 'stage_failed'})
                        last_error = exc
                        failovers.append({'step': step_index, 'stage': stage_index,
                            'node_id': profile.node_id, 'error': str(exc)})
                else:
                    raise RuntimeError(f'stage {stage_index} unavailable: {last_error}')
            topk = list(zip(response.topk_ids, response.topk_logits))
            if not topk or response.token_id != topk[0][0]:
                raise ValueError('invalid terminal shard response')
            text = '' if response.token_id == tokenizer.eos_token_id else tokenizer.decode(
                [response.token_id])
            value = Step(response.token_id, text, topk, activations)
            steps.append(value)
            emit({'type': 'token', 'text': text, 'token': response.token_id})
            ids.append(response.token_id)
            if response.token_id == tokenizer.eos_token_id:
                break
    finally:
        for client in clients:
            client.close()

    receipt = PipelineExecutionReceipt(request_id=job['request_id'],
        output_commitment=hashlib.sha256(canonical([encode_step(step) for step in steps])).digest(),
        input_tokens=input_tokens, output_tokens=len(steps),
        elapsed_ms=max(1, int((time.monotonic() - started) * 1000)))
    receipt.latency_witnesses.extend(witnesses)
    for invocation in invocations:
        from ember_pb2 import ShardForwardResponse
        receipt.stage_receipts.add().CopyFrom(ShardForwardResponse.FromString(
            bytes.fromhex(invocation['response'])).receipt)
    emit({'type': 'result', 'receipt': receipt.SerializeToString(
            deterministic=True).hex(), 'openings': [opening(step) for step in steps],
          'invocations': invocations, 'failovers': failovers})


if __name__ == '__main__':
    main()
