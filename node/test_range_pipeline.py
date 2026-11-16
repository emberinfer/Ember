from test_support import targeted_query
"""FLR-4: signed single-range replay against real Qwen2.5-1.5B weights."""
import copy
import gc
import hashlib
import math
from pathlib import Path

import pytest
import torch
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

from ember_core import digest, node_id, sign
from ember_pb2 import (ChainGenesis, ModelManifest, NumericAuditPolicy, RangeBandParams,
    RangeDeviceBase, PipelineExecutionReceipt, ResourceProfile, ShardSpec,
    ShardForwardRequest, ShardForwardResponse)
from engine import Step
from engine.shard import Qwen2ShardEngine
from evidence import canonical, opening, encode_step
from shard_rpc import tensor_bytes, sign_stage_response
from sharding import range_parameters
from auditor.pipeline import audit_pipeline, load_pipeline_policy, pipeline_offense, reconstruct

MODEL = Path('/root/models/Qwen2.5-1.5B-Instruct')


@pytest.fixture(scope='module')
def ranges():
    if not MODEL.is_dir():
        pytest.skip('real 1.5B weights unavailable')
    torch.set_num_threads(2)
    torch.manual_seed(684)
    keys = [Ed25519PrivateKey.generate() for _ in range(2)]
    manifest = ModelManifest(chain_id='ember-range-test', total_layers=28, dynamic_ranges=True)
    sign(manifest, keys[0])
    bases = [RangeDeviceBase(producer_device='cpu', reference_device='cpu', base=.00001)]
    genesis = ChainGenesis(chain_id=manifest.chain_id, model_manifest_root=digest(manifest),
        execution_profile_ver='transformers-qwen2.5-pipeline-range-v2',
        numeric_audit_policy=NumericAuditPolicy(top_k=16, calibration_ref=b't'*32,
            range_band=RangeBandParams(metric='relative-l2-v1', base=bases, head_base=bases,
                                       depth_coef=.5, hard_multiplier=3)))
    sign(genesis, keys[0])
    specs = [ShardSpec(index=0, start_layer=0, end_layer=26, has_embedding=True, unit_kind='range',
                parameters_hash=range_parameters(digest(manifest), 0, 26)),
             ShardSpec(index=1, start_layer=26, end_layer=28, has_lm_head=True, unit_kind='range',
                parameters_hash=range_parameters(digest(manifest), 26, 28))]
    profiles = []
    for key, spec in zip(keys, specs):
        p = ResourceProfile(chain_id=manifest.chain_id, manifest_root=digest(manifest),
            execution_profile_ver=genesis.execution_profile_ver, actual_device='cpu',
            shard_dynamic_range=True, shard_unit_kind='range', shard_index=spec.index,
            shard_start_layer=spec.start_layer, shard_end_layer=spec.end_layer,
            shard_total_layers=28, shard_has_embedding=spec.has_embedding,
            shard_has_lm_head=spec.has_lm_head, shard_parameters_hash=spec.parameters_hash)
        sign(p, key); p.node_id = node_id(p.signer); sign(p, key)
        profiles.append(p)
    first = Qwen2ShardEngine(str(MODEL), 0, 26, True, False, 'cpu')
    first.range_evidence = True
    ids = [1, 2, 3]
    with torch.inference_mode():
        hidden = first.forward(input_ids=torch.tensor([ids]))
    raw, shape = tensor_bytes(hidden)
    first_activations = dict(first.last_activations)
    first.close(); del first; gc.collect()
    producer = Qwen2ShardEngine(str(MODEL), 26, 28, False, True, 'cpu')
    reference = Qwen2ShardEngine(str(MODEL), 26, 28, False, True, 'cpu')
    producer.range_evidence = True

    def make_job():
        with torch.inference_mode():
            logits = producer.forward(hidden_states=hidden)
            top = torch.topk(torch.log_softmax(logits[0, -1].float(), dim=-1), 16)
        token = int(top.indices[0])
        reqs = [ShardForwardRequest(request_id='range', step=0, input_ids=ids,
                    dynamic_range=True, executed_start_layer=0, executed_end_layer=26),
                ShardForwardRequest(request_id='range', step=0, hidden_bf16=raw, hidden_shape=shape,
                    dynamic_range=True, executed_start_layer=26, executed_end_layer=28)]
        responses = [ShardForwardResponse(hidden_bf16=raw, hidden_shape=shape,
                        activations=first_activations, shard_index=0, shard_parameters_hash=specs[0].parameters_hash),
                     ShardForwardResponse(token_id=token, topk_ids=top.indices.tolist(),
                        topk_logits=top.values.tolist(), activations=producer.last_activations,
                        shard_index=1, shard_parameters_hash=specs[1].parameters_hash)]
        invocations = []
        for i, (req, resp) in enumerate(zip(reqs, responses)):
            sign_stage_response(req, resp, keys[i], genesis, manifest, specs[i])
            invocations.append(dict(request=req.SerializeToString().hex(), response=resp.SerializeToString().hex(),
                                    profile=profiles[i].SerializeToString().hex()))
        step = Step(token, '', list(zip(top.indices.tolist(), top.values.tolist())),
                    dict(first_activations, **producer.last_activations), 'logprobs')
        receipt = PipelineExecutionReceipt(request_id='range', input_tokens=len(ids), output_tokens=1,
            elapsed_ms=10, stage_receipts=[r.receipt for r in responses],
            output_commitment=hashlib.sha256(canonical([encode_step(step)])).digest())
        return dict(kind='pipeline', genesis=genesis.SerializeToString().hex(),
            policy_ref=hashlib.sha256(genesis.SerializeToString()).hexdigest(),
            pipeline_receipt=receipt.SerializeToString().hex(), invocations=invocations,
            openings=[opening(step)], drawn_stage=dict(index=1, start_layer=26, end_layer=28,
                has_embedding=False, has_lm_head=True, node_id=profiles[1].node_id))
    yield producer, reference, make_job
    producer.close(); reference.close(); gc.collect()


def audit(job, engine):
    metric, thresholds, _ = load_pipeline_policy(job, engine)
    return audit_pipeline(job, engine, thresholds, metric)


def test_honest_single_range(ranges):
    _, reference, make_job = ranges
    job = make_job()
    assert reconstruct(job)[0].output_tokens == 1
    verdict, distance, reason, faults = audit(job, reference)
    assert verdict == 'accept', reason
    assert distance < 1e-7 and not faults
    assert set(job['_range_result']['distances']) == {'layer', 'head'}


def test_only_lm_head_cheat_is_caught_by_head_route(ranges):
    producer, reference, make_job = ranges
    # lm_head and embedding weights are independent tensors in the loaded shard;
    # no layer or input activation changes in this attack.
    saved = producer.lm_head.weight.detach().clone()
    try:
        with torch.no_grad():
            producer.lm_head.weight.add_(torch.randn_like(producer.lm_head.weight)*.05)
        job = make_job()
    finally:
        with torch.no_grad():
            producer.lm_head.weight.copy_(saved)
    verdict, distance, reason, faults = audit(job, reference)
    distances = job['_range_result']['distances']
    assert verdict == 'dispute', reason
    assert distances['layer'] < 1e-7  # stays at honest baseline
    assert distances['head'] > .0001
    assert faults == [job['drawn_stage']['node_id']]
    offense = pipeline_offense('reject', distance, job)
    assert offense.band == 'pipeline-range-v2' and offense.length_class == 'head:26-28'


def test_only_layer_weight_cheat_is_caught_by_layer_route(ranges):
    producer, reference, make_job = ranges
    weight = producer.layers[0].mlp.down_proj.weight
    saved = weight.detach().clone()
    try:
        with torch.no_grad():
            weight.add_(torch.randn_like(weight)*.05)
        job = make_job()
    finally:
        with torch.no_grad():
            weight.copy_(saved)
    verdict, distance, reason, faults = audit(job, reference)
    distances = job['_range_result']['distances']
    assert verdict == 'dispute', reason
    assert distances['layer'] > .0001
    # Head sees exactly the producer's own changed boundary, isolating head weights.
    assert distances['head'] < 1e-7
    assert faults == [job['drawn_stage']['node_id']]
    assert pipeline_offense('reject', distance, job).length_class == 'layer:26-28'


def test_missing_or_misaligned_input_is_invalid_not_whole_replay(ranges):
    _, reference, make_job = ranges
    job = make_job()
    job['openings'] = []
    result = audit_pipeline(job, reference, {'layer': (.01, .03), 'head': (.01, .03)})
    assert result[0] == 'reject' and math.isinf(result[1]) and result[3] == []
    assert pipeline_offense('reject', result[1], job) is None
    job = make_job()
    req = ShardForwardRequest.FromString(bytes.fromhex(job['invocations'][1]['request']))
    req.ClearField('hidden_bf16')
    job['invocations'][1]['request'] = req.SerializeToString().hex()
    result = audit_pipeline(job, reference, {'layer': (.01, .03), 'head': (.01, .03)})
    assert result[0] == 'reject' and math.isinf(result[1]) and result[3] == []


def test_registered_worker_binds_finalized_draw_and_signs_range_verdict(ranges, monkeypatch):
    import base64
    import json
    from types import SimpleNamespace
    from auditor.worker import execute
    from ember_pb2 import AuditVerdict, AuditChallenge
    _, reference, make_job = ranges
    job = make_job()
    _, thresholds, _ = load_pipeline_policy(job, reference)
    key = Ed25519PrivateKey.generate()
    marker = ResourceProfile(); sign(marker, key)
    auditor_id = node_id(marker.signer)
    d = job['drawn_stage']
    receipt_raw = bytes.fromhex(job['pipeline_receipt'])
    receipt_ref = hashlib.sha256(b'ember/pipeline-receipt/v1\x00'+receipt_raw).hexdigest()
    authorizations = []
    for item in job['invocations']:
        p = ResourceProfile.FromString(bytes.fromhex(item['profile']))
        authorizations.append(dict(node_id=p.node_id, start_layer=p.shard_start_layer,
            end_layer=p.shard_end_layer, profile=base64.b64encode(bytes.fromhex(item['profile'])).decode()))
    hold = dict(state='dispute', auditor=auditor_id, audit_proof='ab'*32,
        model_id='ember-range-test', receipt_ref=receipt_ref,
        pipeline=dict(receipt=base64.b64encode(receipt_raw).decode(), authorizations=authorizations),
        assignments=[dict(drawn_stage=d)], misjudge_thresholds=[dict(model='ember-range-test',
            band='pipeline-range-v2', length_class=f'{route}:26-28',
            theta_soft=pair[0], theta_hard=pair[1]) for route, pair in thresholds.items()])
    state = dict(holds={'range': hold}, accounts={auditor_id: dict(nonce=7)})
    monkeypatch.setattr('agent.join._chain_json', targeted_query(state))
    agent = SimpleNamespace(key=key, node_id=auditor_id, engine=reference,
        genesis_bytes=bytes.fromhex(job['genesis']), chain_args=SimpleNamespace(
            audit_role=True, chain_rpc='test', chain_cli='test', ledger_chain_id='test-ledger'))
    result = json.loads(execute(agent, SimpleNamespace(request_id='range'), job))
    assert result['verdict'] == 'accept'
    envelope = json.loads(base64.b64decode(result['envelope']))
    verdict = AuditVerdict.FromString(base64.b64decode(envelope['action']['verdict']))
    challenge = AuditChallenge.FromString(base64.b64decode(envelope['action']['challenge']))
    from ember_core import verify
    verify(challenge); verify(verdict)
    assert challenge.receipt_ref.hex() == receipt_ref
    assert verdict.n_out_billed == 1 and not verdict.HasField('offense')
    bad = copy.deepcopy(job)
    bad['drawn_stage']['index'] = 0
    with pytest.raises(ValueError, match='finalized assignment'):
        execute(agent, SimpleNamespace(request_id='range'), bad)
    bad = copy.deepcopy(job)
    bad['hold']['misjudge_thresholds'][0]['theta_hard'] *= 2
    with pytest.raises(ValueError, match='frozen policy'):
        load_pipeline_policy(bad, reference)


@pytest.mark.parametrize('route', ['layer', 'head'])
@pytest.mark.parametrize('values,verdict', [([1.5, 0.0, 0.0], 'accept'),
                                           ([1.5, 1.2, 0.6], 'dispute')])
def test_step_distances_use_mean(monkeypatch, route, values, verdict):
    from types import SimpleNamespace
    from auditor import range_pipeline as rp

    # Exercise the replay loop without model weights or signed-evidence setup;
    # only the per-step metric is stubbed to control the outlier precisely.
    hidden = torch.ones((1, 1, 2), dtype=torch.bfloat16)
    shard = SimpleNamespace(torch=torch, device='cpu', start_layer=0, end_layer=1,
        has_embedding=False, has_lm_head=True,
        config=SimpleNamespace(hidden_size=2, max_position_embeddings=32),
        forward=lambda **kwargs: hidden, norm=lambda x: x,
        lm_head=lambda x: torch.zeros((1, 1, 16)))
    d = dict(start_layer=1 if route == 'head' else 0, end_layer=1,
             has_embedding=False, has_lm_head=route == 'head', node_id='producer')
    request = SimpleNamespace(hidden_bf16=b'x', hidden_shape=[1, 1, 2])
    response = SimpleNamespace(hidden_bf16=b'x', hidden_shape=[1, 1, 2],
                               topk_ids=list(range(16)), topk_logits=[-3.0]*16)
    monkeypatch.setattr('auditor.pipeline.reconstruct',
                        lambda job: (SimpleNamespace(output_tokens=len(values)), None))
    monkeypatch.setattr(rp, 'stage_invocations',
                        lambda job: [(request, None, response)]*len(values))
    monkeypatch.setattr(rp, '_reference_shard', lambda engine: shard)
    monkeypatch.setattr('shard_rpc.bytes_tensor', lambda *args: hidden)
    steps = iter(values)
    monkeypatch.setattr(rp, 'relative_distance', lambda *args: next(steps))
    monkeypatch.setattr(rp, 'union_distance', lambda *args: (next(steps), None, None))
    job = dict(drawn_stage=d)
    result = rp.audit_range(job, shard, {route: (1.0, 2.0)})
    mean = sum(values)/len(values)
    assert max(values) > 1.0
    assert result[0] == verdict
    assert result[1] == pytest.approx(mean)
    assert job['_range_result']['distances'][route] == pytest.approx(mean)
    assert result[3] == (['producer'] if verdict == 'dispute' else [])
    first = copy.deepcopy(job['_range_result'])
    confirmed = rp.confirm_range_results(job, first, {route: (1.0, 2.0)})
    if verdict == 'dispute':
        assert confirmed == pytest.approx(mean)
        assert rp.range_offense(job, 'reject', confirmed).score == pytest.approx(mean)
    else:
        assert confirmed is None
