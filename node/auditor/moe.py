"""MoE segment auditor: replays only the drawn segments.

Expert segments: the committed routing record names the exact token rows;
the auditor slices them from the committed A_att evidence (hash-checked
against the record), replays its own expert unit, and compares against the
segment's output evidence.

Attention segments: the auditor rebuilds the stage's KV by replaying the
stage from frame 0 up to the drawn frame (the KV-rebuild path calibrated
in MOE-0). Expert branches inside the replay are NOT recomputed: their
outputs are taken from evidence after verifying each output against the
expert segment receipt's signed output hash — so a cheating expert never
incriminates the attention segment, and vice versa.

Objective faults (params hash differs from the auditor's own weights,
committed evidence does not hash-match, missing evidence) are graded
objective; pure numerical excess is graded numerical, band
"moe-segment-v1".
"""
import json
import math
from pathlib import Path

import torch

from ember_core import digest
from ember_core import moe as moe_core
from ember_pb2 import (OffenseGrade, PipelineExecutionReceipt, SegmentDistance)
from engine.moe import AttentionStage, MoeWeights, make_expert_unit
from moe_rpc import rows_bytes


def _tensor(raw, hidden):
    return torch.frombuffer(bytearray(raw), dtype=torch.uint16).view(
        torch.bfloat16).reshape(-1, hidden)


def _rel_l2(a, b):
    a = a.float()
    b = b.float()
    return float(torch.linalg.vector_norm(a - b) /
                 max(torch.linalg.vector_norm(b), 1e-12))


class MoeAuditor:
    def __init__(self, model_dir, manifest, genesis, device='cpu'):
        self.weights = MoeWeights(model_dir)
        self.manifest = manifest
        self.genesis = genesis
        self.layout = manifest.moe_layout
        self.root = digest(manifest)
        self.device = device
        self._units = {}
        self._stages = {}
        self._replay = None  # (frame, evidence, seg_by_key) during attention replay

    def unit(self, layer, expert):
        key = (layer, expert)
        if key not in self._units:
            # The replay caliber follows the profile's segment dtype:
            # FP8 profiles replay FP8-natively, never via bf16 dequant.
            self._units[key] = make_expert_unit(
                self.weights, self.layout, layer, expert, device=self.device)
        return self._units[key]

    def stage(self, start, end):
        key = (start, end)
        if key not in self._stages:
            stage = AttentionStage(
                self.weights, self.layout, start, end, device=self.device,
                expert_call=self._evidence_expert_call)
            self._stages[key] = stage
        return self._stages[key]

    def _evidence_expert_call(self, layer, expert, rows):
        frame, evidence, seg_by_key = self._replay
        seg = seg_by_key.get(('expert', frame, layer, expert))
        exp_in = evidence.get('expin', (frame, layer, expert))
        exp_out = evidence.get('expout', (frame, layer, expert))
        if seg is None or exp_in is None or exp_out is None:
            raise ValueError('missing expert evidence during attention replay')
        if exp_in != rows_bytes(rows)[0]:
            raise ValueError('expert input rows differ from evidence')
        if moe_core.activation_hash(self.root, layer, exp_out,
                                    [1, rows.shape[0], rows.shape[1]]) != seg.output_hash:
            raise ValueError('expert output evidence hash mismatch')
        return _tensor(exp_out, rows.shape[1]).to(rows.dtype)

    def band(self, kind, producer_device):
        for band in self.genesis.numeric_audit_policy.bands:
            if (band.segment_kind == kind and band.mode == 'enforce' and
                    band.producer_device == producer_device and
                    band.reference_device == self.device):
                return band.tolerance
        return None

    def audit(self, job):
        """job: {receipt (hex), profiles {node_id: ResourceProfile},
        evidence_dir, drawn [SegmentRef], input_ids, tokens,
        stage_index {(start, end): stage position}}.

        Returns verdict fields; the caller signs the AuditVerdict.
        """
        receipt = PipelineExecutionReceipt.FromString(bytes.fromhex(job['receipt']))
        profiles = job['profiles']
        evidence = _Evidence(Path(job['evidence_dir']))
        data = receipt.moe
        records = {(r.frame, r.layer): r for r in data.routing_records}
        seg_by_key = {}
        for seg in data.segment_receipts:
            seg_by_key[(seg.kind, seg.frame, seg.layer, seg.expert)] = seg
        distances = []
        faults = {}
        hidden = self.layout.hidden_size
        for ref in job['drawn']:
            if ref.kind == 'expert':
                distance, objective, owner = self._audit_expert(
                    ref, records, seg_by_key, evidence, hidden)
            else:
                distance, objective, owner = self._audit_attention(
                    ref, receipt, seg_by_key, evidence, job, hidden)
            producer = profiles.get(owner)
            producer_device = producer.actual_device if producer else self.device
            tolerance = self.band(ref.kind, producer_device)
            distances.append(SegmentDistance(
                ref=ref, distance=distance,
                band_key=f'moe/{ref.kind}/{producer_device}->{self.device}'))
            if tolerance is None or distance > tolerance:
                faults[owner] = 'objective' if objective else 'numerical'
        verdict = 'accept' if not faults else 'reject'
        offense = None
        if faults:
            if 'objective' in faults.values():
                offense = OffenseGrade(**{'class': 'objective', 'fault': 'hash'})
            else:
                finite = [d.distance for d in distances if math.isfinite(d.distance)]
                offense = OffenseGrade(
                    **{'class': 'numerical',
                       'score': min(max(finite), 1e6) if finite else 1e6,
                       'band': 'moe-segment-v1'})
        return {'verdict': verdict, 'distances': distances,
                'fault_node_ids': sorted(faults), 'offense': offense}

    def _audit_expert(self, ref, records, seg_by_key, evidence, hidden):
        seg = seg_by_key.get(('expert', ref.frame, ref.layer, ref.expert))
        if seg is None:
            return math.inf, True, ''
        owner = seg.node_id
        record = records.get((ref.frame, ref.layer))
        if record is None or seg.input_hash != record.input_hash:
            return math.inf, True, owner
        if moe_core.routing_record_hash(record) != seg.routing_record_hash:
            return math.inf, True, owner
        a_att_raw = evidence.get('a_att', (ref.frame, ref.layer))
        if a_att_raw is None:
            return math.inf, True, owner
        if moe_core.activation_hash(self.root, ref.layer, a_att_raw,
                                    [1, record.positions, hidden]) != record.input_hash:
            return math.inf, True, owner
        unit = self.unit(ref.layer, ref.expert)
        if unit.params_hash != seg.params_hash:
            return math.inf, True, owner
        a_att = _tensor(a_att_raw, hidden)
        k = self.layout.top_k
        rows_idx = [pos for pos in range(record.positions)
                    if ref.expert in record.experts[pos * k:(pos + 1) * k]]
        if len(rows_idx) != seg.token_rows:
            return math.inf, True, owner
        in_rows = a_att[rows_idx]
        exp_in = evidence.get('expin', (ref.frame, ref.layer, ref.expert))
        if exp_in is not None and exp_in != rows_bytes(in_rows)[0]:
            return math.inf, True, owner
        exp_out_raw = evidence.get('expout', (ref.frame, ref.layer, ref.expert))
        if exp_out_raw is None:
            return math.inf, True, owner
        if moe_core.activation_hash(self.root, ref.layer, exp_out_raw,
                                    [1, len(rows_idx), hidden]) != seg.output_hash:
            return math.inf, True, owner
        out = unit.forward(in_rows)
        expected = _tensor(exp_out_raw, hidden)
        if out.shape != expected.shape:
            return math.inf, True, owner
        return _rel_l2(out, expected), False, owner

    def _audit_attention(self, ref, receipt, seg_by_key, evidence, job, hidden):
        seg = None
        for candidate in receipt.moe.segment_receipts:
            if (candidate.kind == 'attention' and candidate.frame == ref.frame and
                    candidate.start_layer == ref.layer and
                    candidate.end_layer == ref.end_layer):
                seg = candidate
                break
        if seg is None:
            return math.inf, True, ''
        owner = seg.node_id
        stage = self.stage(ref.layer, ref.end_layer)
        if stage.params_hash != seg.params_hash:
            return math.inf, True, owner
        input_ids = job['input_ids']
        tokens = job['tokens']
        sizes = moe_core.frames(receipt.input_tokens, receipt.output_tokens,
                                self.layout.max_frame_positions)
        prefill = len(sizes) - (receipt.output_tokens - 1)
        stage_idx = job['stage_index'][(ref.layer, ref.end_layer)]
        request_id = f"audit-{receipt.request_id}-{ref.layer}-{ref.end_layer}"
        cursor = 0
        distance = 0.0
        try:
            for frame in range(ref.frame + 1):
                size = sizes[frame]
                self._replay = (frame, evidence, seg_by_key)
                if ref.layer == 0:
                    if frame < prefill:
                        chunk = input_ids[cursor:cursor + size]
                        cursor += size
                        args = {'input_ids': chunk}
                    else:
                        args = {'input_ids': [tokens[frame - prefill]]}
                else:
                    raw = evidence.get('stage_in', (frame, stage_idx))
                    if raw is None:
                        return math.inf, True, owner
                    args = {'hidden': _tensor(raw, hidden)}
                out, _records, a_att, _logits, _in = stage.forward_frame(
                    request_id, frame, **args)
                if frame == ref.frame:
                    for layer in range(ref.layer, ref.end_layer):
                        expected_raw = evidence.get('a_att', (frame, layer))
                        if expected_raw is None:
                            return math.inf, True, owner
                        distance = max(distance, _rel_l2(
                            a_att[layer], _tensor(expected_raw, hidden)))
                    out_raw = evidence.get('stage_out', (frame, stage_idx))
                    if out_raw is not None:
                        distance = max(distance, _rel_l2(
                            out, _tensor(out_raw, hidden)))
        except ValueError:
            return math.inf, True, owner
        finally:
            self._replay = None
            stage.drop_request(request_id)
        return distance, False, owner


class _Evidence:
    def __init__(self, directory):
        self.dir = directory
        self.index = json.loads((directory / 'index.json').read_text())

    def get(self, section, key):
        name = self.index.get(section, {}).get(','.join(str(p) for p in key))
        if name is None or isinstance(name, dict):
            return None
        return (self.dir / name).read_bytes()
