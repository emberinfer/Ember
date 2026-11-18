"""MoE segment RPC: expert units and backbone attention stages served over
the pinned-TLS ShardPipeline channel, dispatched by moe_call_kind.

Receipt discipline mirrors shard_rpc: requests are scheduler-signed with
monotonic nonces; every response carries a SegmentReceipt signed by the
serving node's identity key, bound to the manifest, the routing record (for
experts) and the activation boundary hashes. At the end of a frame the
attention service seals the frame: it signs its own NodeFrameReceipt and
collects one from every expert node it called (moe_call_kind "seal").
"""
from dataclasses import dataclass, field
import json
import os
import threading
import time
from pathlib import Path

import grpc
import torch

from ember_core import digest, node_id, sign, verify
from ember_core import moe as moe_core
from ember_pb2 import (NodeFrameReceipt, RoutingRecord, SegmentReceipt,
                       ShardForwardRequest, ShardForwardResponse)
from ember_pb2_grpc import ShardPipelineServicer, ShardPipelineStub
from pinned_tls import pinned_channel
from shard_rpc import bytes_tensor, tensor_bytes

MAX_TENSOR_BYTES = 64 * 1024 * 1024
# Stage responses aggregate many individually bounded activation tensors.
MAX_MESSAGE_BYTES = 320 * 1024 * 1024


def rows_bytes(tensor):
    """(S, hidden) tensor -> (bf16 bytes, [1, S, hidden])."""
    return tensor_bytes(tensor.reshape(1, *tensor.shape))


def activation_hash(root, layer, tensor):
    raw, shape = rows_bytes(tensor)
    return moe_core.activation_hash(root, layer, raw, shape)


class _SchedulerAuth:
    """Same nonce discipline as shard_rpc.StageService."""

    def __init__(self, authorized_schedulers, nonce_path=None):
        self.authorized = set(authorized_schedulers)
        self.lock = threading.Lock()
        self.nonce_path = Path(nonce_path) if nonce_path else None
        self.nonces = {}
        if self.nonce_path and self.nonce_path.exists():
            self.nonces = json.loads(self.nonce_path.read_text(encoding='ascii'))

    def check(self, request):
        verify(request)
        scheduler = node_id(request.signer)
        with self.lock:
            if (scheduler not in self.authorized or
                    request.nonce <= self.nonces.get(scheduler, 0)):
                raise PermissionError('unauthorized or replayed scheduler request')
            self.nonces[scheduler] = request.nonce
            if self.nonce_path:
                self.nonce_path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
                tmp = self.nonce_path.with_suffix('.tmp')
                fd = os.open(tmp, os.O_CREAT | os.O_TRUNC | os.O_WRONLY, 0o600)
                with os.fdopen(fd, 'w', encoding='ascii') as stream:
                    json.dump(self.nonces, stream, sort_keys=True)
                os.replace(tmp, self.nonce_path)
        return scheduler


class ReceiptSigner:
    def __init__(self, key, genesis, manifest):
        self.key = key
        self.genesis = genesis
        self.manifest = manifest
        self.root = digest(manifest)
        from cryptography.hazmat.primitives import serialization
        self.node = node_id(key.public_key().public_bytes(
            serialization.Encoding.Raw, serialization.PublicFormat.Raw))

    @staticmethod
    def execution(receipt, receipt_v2=False):
        """Emit transport v2 only after full v1 signatures and roots exist."""
        if receipt_v2:
            return moe_core.compact_moe_receipt(receipt)
        return moe_core.expand_moe_receipt(receipt)

    def segment(self, **fields):
        receipt = SegmentReceipt(
            node_id=self.node, chain_id=self.genesis.chain_id,
            manifest_root=self.root,
            execution_profile_ver=self.genesis.execution_profile_ver, **fields)
        sign(receipt, self.key)
        return receipt

    def frame(self, request_id, frame, segment_hashes):
        receipt = NodeFrameReceipt(
            node_id=self.node, request_id=request_id, frame=frame,
            segments_root=moe_core.frame_root(segment_hashes),
            segment_count=len(segment_hashes))
        sign(receipt, self.key)
        return receipt


class MoeExpertService(ShardPipelineServicer):
    """Serves a set of (layer, expert) units; seals frames on request.

    units is either a {(layer, expert): ExpertUnit} dict or a factory
    (layer, expert) -> ExpertUnit used lazily (large models hold too many
    units to preload); factory results are cached.
    """

    def __init__(self, units, signer, authorized_schedulers, nonce_path=None):
        self.units = units
        self._unit_cache = {}
        self.signer = signer
        self.auth = _SchedulerAuth(authorized_schedulers, nonce_path)
        self.issued = {}  # (request_id, frame) -> [segment receipt hashes]
        self.lock = threading.Lock()

    def unit(self, layer, expert):
        if isinstance(self.units, dict):
            return self.units.get((layer, expert))
        if (layer, expert) not in self._unit_cache:
            self._unit_cache[(layer, expert)] = self.units(layer, expert)
        return self._unit_cache[(layer, expert)]

    def Forward(self, request, context):
        try:
            self.auth.check(request)
            if request.moe_call_kind == 'seal':
                with self.lock:
                    hashes = self.issued.pop((request.request_id, request.moe_frame), [])
                frame = self.signer.frame(request.request_id, request.moe_frame, hashes)
                return ShardForwardResponse(frame_receipts=[frame])
            if request.moe_call_kind != 'expert':
                raise ValueError('expert service accepts expert/seal calls only')
            unit = self.unit(request.moe_layer, request.moe_expert)
            if unit is None:
                raise ValueError('unit not held by this node')
            rows = bytes_tensor(request.hidden_bf16, request.hidden_shape,
                                unit.device)
            rows = rows.reshape(rows.shape[1], rows.shape[2])
            out = unit.forward(rows)
            raw, shape = rows_bytes(out)
            receipt = self.signer.segment(
                request_id=request.request_id, kind='expert',
                layer=request.moe_layer, expert=request.moe_expert,
                frame=request.moe_frame, start_layer=request.moe_layer,
                end_layer=request.moe_layer + 1,
                input_hash=request.moe_input_hash,
                output_hash=moe_core.activation_hash(
                    self.signer.root, request.moe_layer, raw, shape),
                routing_record_hash=request.moe_routing_record_hash,
                params_hash=unit.params_hash, token_rows=int(rows.shape[0]))
            with self.lock:
                self.issued.setdefault((request.request_id, request.moe_frame),
                                       []).append(moe_core.segment_receipt_hash(receipt))
            return ShardForwardResponse(hidden_bf16=raw, hidden_shape=shape,
                                        segment_receipt=receipt)
        except PermissionError as exc:
            context.abort(grpc.StatusCode.UNAUTHENTICATED, str(exc))
        except (ValueError, RuntimeError, KeyError) as exc:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, str(exc))


@dataclass
class FrameContext:
    request_id: str
    frame: int
    expert_receipts: list = field(default_factory=list)
    pending_rows: dict = field(default_factory=dict)
    a_att: dict = field(default_factory=dict)
    routing: dict = field(default_factory=dict)


class MoeAttentionService(ShardPipelineServicer):
    """Serves one backbone stage; fans out to expert clients per frame."""

    def __init__(self, stage, signer, expert_router, authorized_schedulers,
                 nonce_path=None):
        """expert_router: {(layer, expert): client} dict or a callable
        (layer, expert) -> MoeExpertClient (per-node routing for large
        models, where per-unit clients would mean hundreds of channels)."""
        self.stage = stage
        self.signer = signer
        self.expert_router = expert_router
        self.auth = _SchedulerAuth(authorized_schedulers, nonce_path)
        stage.expert_call = self._expert_call
        self._execution_lock = threading.Lock()
        self._next_frame = {}

    def _expert_client(self, layer, expert):
        if callable(self.expert_router):
            return self.expert_router(layer, expert)
        return self.expert_router.get((layer, expert))

    def _expert_call(self, frame_context, layer, expert, rows):
        client = self._expert_client(layer, expert)
        if client is None:
            raise RuntimeError(f'no expert client for {layer}/{expert}')
        record = frame_context.routing
        input_hash = activation_hash(self.signer.root, layer,
                                     frame_context.a_att[layer])
        proto = RoutingRecord(frame=frame_context.frame, layer=layer,
                              positions=record['positions'],
                              experts=record['experts'], input_hash=input_hash)
        routing_hash = moe_core.routing_record_hash(proto)
        out, receipt = client.call_expert(frame_context.request_id, frame_context.frame, layer,
                                          expert, rows, routing_hash, input_hash)
        frame_context.expert_receipts.append(receipt)
        frame_context.pending_rows[(layer, expert)] = (rows, out)
        return out

    def Forward(self, request, context):
        # Transitional per-stage fence: retain until independent isolation proof.
        with self._execution_lock:
            if hasattr(context, 'is_active') and not context.is_active():
                context.abort(grpc.StatusCode.CANCELLED, 'request cancelled before execution')
            return self._forward_locked(request, context)

    def _forward_locked(self, request, context):
        try:
            self.auth.check(request)
            if request.moe_call_kind != 'attention':
                raise ValueError('attention service accepts attention calls only')
            if (request.executed_start_layer != self.stage.start or
                    request.executed_end_layer != self.stage.end):
                raise ValueError('executed span differs from served stage')
            if request.moe_frame != self._next_frame.get(request.request_id, 0):
                raise ValueError('unexpected or duplicate request frame')
            frame_context = FrameContext(request.request_id, request.moe_frame)
            if request.input_ids:
                input_ids, hidden = list(request.input_ids), None
            else:
                tensor = bytes_tensor(request.hidden_bf16, request.hidden_shape,
                                      self.stage.device)
                hidden = tensor.reshape(tensor.shape[1], tensor.shape[2])
                input_ids = None
            out, records, a_att, logits, input_rows = self.stage.forward_frame(
                request.request_id, request.moe_frame,
                input_ids=input_ids, hidden=hidden, frame_context=frame_context)
            root = self.signer.root
            record_protos = []
            for record in records:
                layer = record['layer']
                record_protos.append(RoutingRecord(
                    frame=request.moe_frame, layer=layer,
                    positions=record['positions'], experts=record['experts'],
                    input_hash=activation_hash(root, layer, a_att[layer])))
            receipt = self.signer.segment(
                request_id=request.request_id, kind='attention',
                layer=self.stage.start, frame=request.moe_frame,
                start_layer=self.stage.start, end_layer=self.stage.end,
                input_hash=activation_hash(root, self.stage.start, input_rows),
                output_hash=activation_hash(root, self.stage.end, out),
                routing_record_hash=moe_core.routing_set_hash(record_protos),
                params_hash=self.stage.params_hash,
                token_rows=int(out.shape[0]))
            # Only this stage's own frame receipt: expert nodes are sealed
            # once per frame by the assembler, after every stage finished
            # the frame (a node may serve several stages in one frame).
            frame_receipts = [self.signer.frame(
                request.request_id, request.moe_frame,
                [moe_core.segment_receipt_hash(receipt)])]
            raw, shape = rows_bytes(out)
            activations = {}
            for layer, tensor in a_att.items():
                activations[str(layer)] = rows_bytes(tensor)[0]
            for (layer, expert), (rin, rout) in frame_context.pending_rows.items():
                activations[f'expin:{layer}:{expert}'] = rows_bytes(rin)[0]
                activations[f'expout:{layer}:{expert}'] = rows_bytes(rout)[0]
            response = ShardForwardResponse(
                hidden_bf16=raw, hidden_shape=shape, routing_records=record_protos,
                segment_receipt=receipt, expert_receipts=frame_context.expert_receipts,
                frame_receipts=frame_receipts, activations=activations)
            if logits is not None:
                top = torch.topk(logits, 16)
                response.token_id = int(top.indices[0])
                response.topk_ids.extend(int(x) for x in top.indices.tolist())
                response.topk_logits.extend(float(x) for x in top.values.tolist())
            self._next_frame[request.request_id] = request.moe_frame + 1
            return response
        except PermissionError as exc:
            context.abort(grpc.StatusCode.UNAUTHENTICATED, str(exc))
        except (ValueError, RuntimeError, KeyError) as exc:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, str(exc))


class _DispatchState:
    def __init__(self):
        self.lock = threading.RLock()
        self.last_nonce = 0


_dispatch_states_lock = threading.Lock()
_dispatch_states = {}


class _BaseClient:
    def __init__(self, target, scheduler_key, profile=None):
        address, fingerprint = target.rsplit('@', 1)
        options = (('grpc.max_send_message_length', MAX_MESSAGE_BYTES),
                   ('grpc.max_receive_message_length', MAX_MESSAGE_BYTES))
        self.channel = pinned_channel(address, fingerprint, options)
        self.stub = ShardPipelineStub(self.channel)
        self.scheduler_key = scheduler_key
        self.profile = profile
        self._nonce_lock = threading.Lock()
        self._last_nonce = 0
        from cryptography.hazmat.primitives import serialization
        scheduler = scheduler_key.public_key().public_bytes(
            serialization.Encoding.Raw, serialization.PublicFormat.Raw)
        # Share ordering across client objects for the same scheduler/target.
        with _dispatch_states_lock:
            self._dispatch_state = _dispatch_states.setdefault(
                (address, fingerprint, scheduler), _DispatchState())

    def _signed(self, request):
        with self._dispatch_state.lock, self._nonce_lock:
            request.nonce = max(time.time_ns(), self._last_nonce + 1,
                                self._dispatch_state.last_nonce + 1)
            self._last_nonce = request.nonce
            self._dispatch_state.last_nonce = request.nonce
        sign(request, self.scheduler_key)
        return request

    def _forward(self, request, timeout):
        # A synchronous response confirms receiver authentication/persistence
        # before another nonce for this scheduler/target may be dispatched.
        with self._dispatch_state.lock:
            return self.stub.Forward(self._signed(request), timeout=timeout)

    def close(self):
        self.channel.close()


class MoeExpertClient(_BaseClient):
    def call_expert(self, request_id, frame, layer, expert, rows,
                    routing_hash, input_hash, timeout=120):
        raw, shape = rows_bytes(rows)
        request = ShardForwardRequest(
            request_id=request_id, step=frame, hidden_bf16=raw,
            hidden_shape=shape, moe_call_kind='expert', moe_layer=layer,
            moe_expert=expert, moe_frame=frame,
            moe_routing_record_hash=routing_hash, moe_input_hash=input_hash)
        response = self._forward(request, timeout)
        receipt = response.segment_receipt
        verify(receipt)
        if (receipt.kind != 'expert' or receipt.layer != layer or
                receipt.expert != expert or receipt.frame != frame or
                receipt.request_id != request_id or
                receipt.routing_record_hash != routing_hash or
                receipt.input_hash != input_hash or
                receipt.token_rows != rows.shape[0]):
            raise ValueError('expert receipt binding mismatch')
        if self.profile is not None:
            verify_segment_profile(receipt, self.profile)
        out = bytes_tensor(response.hidden_bf16, response.hidden_shape, rows.device)
        return out.reshape(out.shape[1], out.shape[2]), receipt

    def seal(self, request_id, frame, timeout=60):
        request = ShardForwardRequest(request_id=request_id, step=frame,
                                      moe_call_kind='seal', moe_frame=frame)
        response = self._forward(request, timeout)
        if len(response.frame_receipts) != 1:
            raise ValueError('expert seal returned no frame receipt')
        receipt = response.frame_receipts[0]
        verify(receipt)
        if receipt.request_id != request_id or receipt.frame != frame:
            raise ValueError('frame receipt binding mismatch')
        return receipt


def verify_bound_signature(receipt):
    try:
        verify(receipt)
    except Exception as exc:
        raise ValueError('signature invalid') from exc
    if receipt.node_id != node_id(receipt.signer):
        raise ValueError('signature valid but binding mismatch: node')


def require_binding(condition, field):
    if not condition:
        raise ValueError('signature valid but bound to the wrong request: ' + field)


def verify_frame_segments(seal, request_id, frame, node, segments):
    verify_bound_signature(seal)
    require_binding(seal.request_id == request_id, 'seal request_id')
    require_binding(seal.frame == frame, 'seal frame')
    require_binding(seal.node_id == node, 'seal node')
    hashes = [moe_core.segment_receipt_hash(r) for r in segments]
    require_binding(seal.segment_count == len(hashes), 'seal segment_count')
    require_binding(seal.segments_root == moe_core.frame_root(hashes), 'seal segments_root')


def verify_attention_response(response, request_id, frame, stage_profile):
    receipt = response.segment_receipt
    verify_bound_signature(receipt)
    require_binding(receipt.request_id == request_id, 'attention request_id')
    require_binding(receipt.frame == frame, 'attention frame')
    require_binding(receipt.node_id == stage_profile.node_id, 'attention node')
    require_binding(receipt.kind == 'attention' and
                    receipt.layer == stage_profile.shard_start_layer and
                    receipt.start_layer == stage_profile.shard_start_layer and
                    receipt.end_layer == stage_profile.shard_end_layer and
                    receipt.params_hash == stage_profile.shard_parameters_hash,
                    'attention span/params/kind')
    verify_segment_profile(receipt, stage_profile)
    require_binding(len(response.hidden_shape) == 3, 'hidden shape')
    require_binding(receipt.output_hash == moe_core.activation_hash(
        receipt.manifest_root, receipt.end_layer,
        response.hidden_bf16, list(response.hidden_shape)), 'attention output_hash')
    records = {r.layer: r for r in response.routing_records}
    require_binding(len(records) == len(response.routing_records) and
                    set(records) == set(range(receipt.start_layer, receipt.end_layer)),
                    'routing layers')
    for record in response.routing_records:
        require_binding(record.frame == frame, 'routing frame')
        require_binding(record.positions > 0 and
                        len(record.experts) % record.positions == 0, 'routing shape')
        raw = response.activations.get(str(record.layer))
        require_binding(raw is not None and record.input_hash == moe_core.activation_hash(
            receipt.manifest_root, record.layer, raw,
            [1, record.positions, response.hidden_shape[2]]), 'routing input_hash')
    require_binding(receipt.routing_record_hash ==
                    moe_core.routing_set_hash(response.routing_records), 'routing set')
    seen = set()
    for expert in response.expert_receipts:
        verify_bound_signature(expert)
        require_binding(expert.request_id == request_id, 'expert request_id')
        require_binding(expert.frame == frame, 'expert frame')
        record = records.get(expert.layer)
        require_binding(record is not None and expert.kind == 'expert', 'expert layer/kind')
        require_binding(expert.expert in record.experts and
                        (expert.layer, expert.expert) not in seen, 'expert selection')
        seen.add((expert.layer, expert.expert))
        require_binding(expert.routing_record_hash == moe_core.routing_record_hash(record),
                        'expert routing_record_hash')
        require_binding(expert.input_hash == record.input_hash, 'expert input_hash')
        require_binding(expert.start_layer == expert.layer and
                        expert.end_layer == expert.layer + 1, 'expert span')
        width = len(record.experts) // record.positions
        row_count = sum(expert.expert in record.experts[i:i + width]
                        for i in range(0, len(record.experts), width))
        require_binding(expert.token_rows == row_count, 'expert token_rows')
        raw = response.activations.get(f'expout:{expert.layer}:{expert.expert}')
        require_binding(raw is not None and expert.output_hash == moe_core.activation_hash(
            receipt.manifest_root, expert.layer, raw,
            [1, row_count, response.hidden_shape[2]]), 'expert output_hash')
    expected = {(r.layer, e) for r in records.values() for e in r.experts}
    require_binding(seen == expected, 'expert segment set')
    require_binding(len(response.frame_receipts) == 1, 'attention seal set')
    verify_frame_segments(response.frame_receipts[0], request_id, frame,
                          receipt.node_id, [receipt])


class MoeAttentionClient(_BaseClient):
    def call_stage(self, request_id, frame, stage_profile, input_ids=None,
                   hidden=None, timeout=300):
        request = ShardForwardRequest(
            request_id=request_id, step=frame, moe_call_kind='attention',
            moe_frame=frame, executed_start_layer=stage_profile.shard_start_layer,
            executed_end_layer=stage_profile.shard_end_layer)
        if input_ids is not None:
            request.input_ids.extend(input_ids)
        else:
            raw, shape = rows_bytes(hidden)
            request.hidden_bf16 = raw
            request.hidden_shape.extend(shape)
        response = self._forward(request, timeout)
        verify_attention_response(response, request_id, frame, stage_profile)
        return response


def verify_segment_profile(receipt, profile):
    """Bind a segment receipt to the registered profile that authorized it."""
    verify(profile)
    if (receipt.node_id != profile.node_id or receipt.signer != profile.signer or
            receipt.chain_id != profile.chain_id or
            receipt.manifest_root != profile.manifest_root or
            receipt.execution_profile_ver != profile.execution_profile_ver):
        raise ValueError('segment signer is not the registered profile')
    if receipt.kind == 'expert':
        for unit in profile.moe_units:
            if (unit.layer == receipt.layer and unit.expert == receipt.expert and
                    unit.params_hash == receipt.params_hash):
                return
        raise ValueError('expert segment not in registered units')
    if receipt.params_hash != profile.shard_parameters_hash:
        raise ValueError('attention segment params differ from registration')
