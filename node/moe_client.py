"""MoE assembly client: drives backbone stages frame by frame, collects
routing records + segment receipts + frame receipts, stores segment
evidence for the auditor, and builds the PipelineExecutionReceipt with
MoeExecutionData that the chain verifies.

Frame plan (shared with the chain): prefill is chunked at
layout.max_frame_positions, then one frame per decode step. A frame
produces an output token iff it is the last prefill chunk or a decode
frame. The output commitment binds, per output token, the token id, the
head stage's top-16, and the producing frame's per-layer A_att bytes
(MoE commitment convention v1).
"""
import hashlib
import json
import time
from pathlib import Path

import torch

from ember_core import digest
from ember_core import moe as moe_core
from ember_pb2 import (MoeExecutionData, PipelineExecutionReceipt)
from engine import Step
from evidence import canonical, encode_step
from moe_rpc import (MoeAttentionClient, ReceiptSigner, verify_attention_response,
                     verify_frame_segments, verify_segment_profile,
                     verify_bound_signature, require_binding)
from shard_rpc import bytes_tensor


class MoeAssembler:
    def __init__(self, stages, tokenizer, manifest, genesis, expert_node_clients=None,
                 receipt_v2=False):
        """stages: [(MoeAttentionClient, stage ResourceProfile)] in span order.
        expert_node_clients: {node_id: MoeExpertClient} used to seal each
        expert node's frame receipt once per frame, after all stages of the
        frame completed (a node may serve several stages in one frame)."""
        self.receipt_v2 = receipt_v2
        self.stages = stages
        self.expert_node_clients = expert_node_clients or {}
        self.tokenizer = tokenizer
        self.manifest = manifest
        self.genesis = genesis
        self.layout = manifest.moe_layout
        self.root = digest(manifest)

    def generate(self, request_id, prompt, max_tokens, on_token=None):
        layout = self.layout
        input_ids = self.tokenizer(prompt).input_ids
        frames = moe_core.frames(len(input_ids), max_tokens,
                                 layout.max_frame_positions)
        prefill_frames = len(frames) - (max_tokens - 1)
        records, seg_receipts, frame_receipts = [], [], []
        evidence = {'a_att': {}, 'expin': {}, 'expout': {},
                    'stage_in': {}, 'stage_out': {}, 'tokens': []}
        tokens = []
        steps = []
        started = time.time()
        last_token = None
        cursor = 0
        for frame, size in enumerate(frames):
            if len(tokens) >= max_tokens:
                break
            hidden = None
            frame_a_att = {}
            frame_expert_nodes = set()
            for stage_idx, (client, profile) in enumerate(self.stages):
                if stage_idx == 0:
                    if frame < prefill_frames:
                        chunk = input_ids[cursor:cursor + size]
                        cursor += size
                        response = client.call_stage(
                            request_id, frame, profile, input_ids=chunk)
                        evidence['stage_in'][(frame, stage_idx)] = ('ids', chunk)
                    else:
                        response = client.call_stage(
                            request_id, frame, profile, input_ids=[last_token])
                        evidence['stage_in'][(frame, stage_idx)] = ('ids', [last_token])
                else:
                    response = client.call_stage(
                        request_id, frame, profile, hidden=hidden)
                    evidence['stage_in'][(frame, stage_idx)] = ('hidden', hidden)
                verify_attention_response(response, request_id, frame, profile)
                for expert_receipt in response.expert_receipts:
                    expert_client = self.expert_node_clients.get(expert_receipt.node_id)
                    require_binding(expert_client is not None and
                                    expert_client.profile is not None, 'expert node')
                    verify_segment_profile(expert_receipt, expert_client.profile)
                hidden = bytes_tensor(response.hidden_bf16,
                                      response.hidden_shape, 'cpu')
                hidden = hidden.reshape(hidden.shape[1], hidden.shape[2])
                evidence['stage_out'][(frame, stage_idx)] = response.hidden_bf16
                records.extend(response.routing_records)
                seg_receipts.append(response.segment_receipt)
                seg_receipts.extend(response.expert_receipts)
                for expert_receipt in response.expert_receipts:
                    frame_expert_nodes.add(expert_receipt.node_id)
                frame_receipts.extend(response.frame_receipts)
                for key, raw in response.activations.items():
                    if key.startswith('expin:'):
                        _, layer, expert = key.split(':')
                        evidence['expin'][(frame, int(layer), int(expert))] = raw
                    elif key.startswith('expout:'):
                        _, layer, expert = key.split(':')
                        evidence['expout'][(frame, int(layer), int(expert))] = raw
                    else:
                        evidence['a_att'][(frame, int(key))] = raw
                        frame_a_att[int(key)] = raw
            for node_id in sorted(frame_expert_nodes):
                client = self.expert_node_clients.get(node_id)
                if client is None:
                    raise RuntimeError(f'no seal client for expert node {node_id}')
                seal = client.seal(request_id, frame)
                verify_frame_segments(seal, request_id, frame, node_id,
                                      [r for r in seg_receipts
                                       if r.frame == frame and r.node_id == node_id])
                frame_receipts.append(seal)
            produces = frame >= prefill_frames - 1
            if produces:
                token = int(response.token_id)
                topk = list(zip(response.topk_ids,
                                [float(v) for v in response.topk_logits]))
                tokens.append(token)
                evidence['tokens'].append(token)
                if on_token is not None:
                    on_token(list(tokens))
                steps.append(Step(token, '', topk,
                                  {str(k): v for k, v in frame_a_att.items()}))
                last_token = token
                if token == self.tokenizer.eos_token_id:
                    break
        commitment = hashlib.sha256(canonical(
            [encode_step(step) for step in steps])).digest()
        receipt = PipelineExecutionReceipt(
            request_id=request_id,
            moe=MoeExecutionData(routing_records=records,
                                 segment_receipts=seg_receipts,
                                 frame_receipts=frame_receipts),
            output_commitment=commitment,
            input_tokens=len(input_ids), output_tokens=len(tokens),
            elapsed_ms=max(1, int((time.time() - started) * 1000)))
        return ReceiptSigner.execution(receipt, self.receipt_v2), evidence

    def save_evidence(self, directory, receipt, evidence, prompt, input_ids):
        """Persist the segment evidence bundle the auditor consumes."""
        verify_execution_bindings(ReceiptSigner.execution(receipt))
        directory = Path(directory)
        directory.mkdir(parents=True, exist_ok=True)
        index = {'request_id': receipt.request_id, 'prompt': prompt,
                 'input_ids': list(input_ids), 'tokens': evidence['tokens'],
                 'receipt': receipt.SerializeToString(deterministic=True).hex(),
                 'a_att': {}, 'expin': {}, 'expout': {},
                 'stage_in': {}, 'stage_out': {}}
        def dump(section, key, raw):
            name = f"{section}-{'_'.join(str(p) for p in key)}.bin"
            (directory / name).write_bytes(raw)
            index[section][','.join(str(p) for p in key)] = name
        for key, raw in evidence['a_att'].items():
            dump('a_att', key, raw)
        for key, raw in evidence['expin'].items():
            dump('expin', key, raw)
        for key, raw in evidence['expout'].items():
            dump('expout', key, raw)
        for key, raw in evidence['stage_out'].items():
            dump('stage_out', key, raw)
        for key, value in evidence['stage_in'].items():
            if value[0] == 'hidden':
                raw, _shape = value[1], None
                from moe_rpc import rows_bytes
                dump('stage_in', key, rows_bytes(value[1])[0])
            else:
                index['stage_in'][','.join(str(p) for p in key)] = {'ids': value[1]}
        (directory / 'index.json').write_text(json.dumps(index))
        return directory


def verify_execution_bindings(receipt):
    """Offline final-bundle validation; no evidence is persisted before this."""
    groups = {}
    records = {(r.frame, r.layer): r for r in receipt.moe.routing_records}
    for segment in receipt.moe.segment_receipts:
        verify_bound_signature(segment)
        require_binding(segment.request_id == receipt.request_id, 'segment request_id')
        groups.setdefault((segment.node_id, segment.frame), []).append(segment)
        if segment.kind == 'expert':
            record = records.get((segment.frame, segment.layer))
            require_binding(record is not None and segment.expert in record.experts,
                            'segment frame/layer/expert')
            require_binding(segment.routing_record_hash == moe_core.routing_record_hash(record),
                            'segment routing_record_hash')
            require_binding(segment.input_hash == record.input_hash, 'segment input_hash')
        elif segment.kind == 'attention':
            subset = [r for r in receipt.moe.routing_records if
                      r.frame == segment.frame and
                      segment.start_layer <= r.layer < segment.end_layer]
            require_binding(segment.routing_record_hash == moe_core.routing_set_hash(subset),
                            'attention routing set')
        else:
            require_binding(False, 'segment kind')
    seen = set()
    for seal in receipt.moe.frame_receipts:
        key = (seal.node_id, seal.frame)
        require_binding(key in groups and key not in seen, 'seal set')
        seen.add(key)
        verify_frame_segments(seal, receipt.request_id, seal.frame, seal.node_id, groups[key])
    require_binding(seen == set(groups), 'missing seal')
