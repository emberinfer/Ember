"""MoE expert-sharding mirrors of core-go/moe.go.

Every hash, frame rule and FLOPs figure here must match the Go core
bit-for-bit / unit-for-unit: the chain derives settlement from these
functions, and node/auditor code derives the same values independently.
"""
import hashlib
import math
import struct

from ember_pb2 import RoutingRecord, SegmentReceipt


def _h(domain: bytes, *parts: bytes) -> bytes:
    h = hashlib.sha256()
    h.update(domain)
    for part in parts:
        h.update(part)
    return h.digest()


def _u32(n: int) -> bytes:
    return struct.pack(">I", n)


def routing_record_hash(record: RoutingRecord) -> bytes:
    raw = record.SerializeToString(deterministic=True)
    return _h(b"ember/moe-routing-record/v1\x00", raw)


def routing_set_hash(records) -> bytes:
    ordered = sorted(records, key=lambda r: r.layer)
    h = hashlib.sha256()
    h.update(b"ember/moe-routing-set/v1\x00")
    h.update(_u32(len(ordered)))
    for record in ordered:
        h.update(routing_record_hash(record))
    return h.digest()


def segment_receipt_hash(receipt: SegmentReceipt) -> bytes:
    raw = receipt.SerializeToString(deterministic=True)
    return _h(b"ember/moe-segment-receipt/v1\x00", raw)


def frame_root(leaf_hashes) -> bytes:
    if not leaf_hashes:
        return b""
    level = sorted(leaf_hashes)
    while len(level) > 1:
        nxt = []
        for i in range(0, len(level), 2):
            if i + 1 == len(level):
                nxt.append(level[i])
            else:
                nxt.append(_h(b"ember/moe-frame-node/v1\x00", level[i], level[i + 1]))
        level = nxt
    return level[0]


def expert_leaf(layer: int, expert: int, params_hash: bytes) -> bytes:
    return _h(b"ember/moe-expert-leaf/v1\x00", _u32(layer), _u32(expert), params_hash)


def _expert_node(a: bytes, b: bytes) -> bytes:
    return _h(b"ember/moe-expert-node/v1\x00", a, b)


def expert_root(layers: int, experts: int, params_hashes) -> bytes:
    if len(params_hashes) != layers * experts:
        raise ValueError("expert leaf set size mismatch")
    level = [expert_leaf(l, e, params_hashes[l * experts + e])
             for l in range(layers) for e in range(experts)]
    while len(level) > 1:
        nxt = []
        for i in range(0, len(level), 2):
            nxt.append(level[i] if i + 1 == len(level) else _expert_node(level[i], level[i + 1]))
        level = nxt
    return level[0]


def expert_proof(layers: int, experts: int, params_hashes, layer: int, expert: int):
    """Sibling hashes from the leaf level upward for (layer, expert)."""
    level = [expert_leaf(l, e, params_hashes[l * experts + e])
             for l in range(layers) for e in range(experts)]
    idx = layer * experts + expert
    proof = []
    while len(level) > 1:
        if idx % 2 == 1:
            proof.append(level[idx - 1])
        elif idx + 1 < len(level):
            proof.append(level[idx + 1])
        nxt = []
        for i in range(0, len(level), 2):
            nxt.append(level[i] if i + 1 == len(level) else _expert_node(level[i], level[i + 1]))
        level = nxt
        idx //= 2
    return proof


def verify_expert_proof(root, layers, experts, layer, expert, params_hash, proof, leaf_index):
    if len(root) != 32 or len(params_hash) != 32 or leaf_index != layer * experts + expert:
        return False
    node = expert_leaf(layer, expert, params_hash)
    idx = leaf_index
    width = layers * experts
    step = 0
    while width > 1:
        if idx % 2 == 1:
            if step >= len(proof):
                return False
            node = _expert_node(proof[step], node)
            step += 1
        elif idx + 1 < width:
            if step >= len(proof):
                return False
            node = _expert_node(node, proof[step])
            step += 1
        idx //= 2
        width = (width + 1) // 2
    return step == len(proof) and node == root


STAGE_TENSOR_SUFFIXES = (
    'input_layernorm.weight', 'self_attn.q_proj.weight',
    'self_attn.k_proj.weight', 'self_attn.v_proj.weight',
    'self_attn.o_proj.weight', 'self_attn.q_norm.weight',
    'self_attn.k_norm.weight', 'post_attention_layernorm.weight',
    'mlp.gate.weight')


def stage_tensor_keys(has, layer: int):
    """The attention stage's committed tensor set for one layer: attention,
    norms and router — never expert weights. `has` is a key->bool callable."""
    prefix = f'model.layers.{layer}.'
    return [prefix + suffix for suffix in STAGE_TENSOR_SUFFIXES if has(prefix + suffix)]


def params_hash(parts) -> bytes:
    """Canonical weights commitment. parts: iterable of
    (name, shape tuple, dtype str, raw bytes) in caller-fixed order."""
    h = hashlib.sha256()
    h.update(b"ember/moe-params/v1\x00")
    for name, shape, dtype, raw in parts:
        encoded = name.encode()
        h.update(_u32(len(encoded)))
        h.update(encoded)
        h.update(_u32(len(shape)))
        for dim in shape:
            h.update(_u32(dim))
        dt = dtype.encode()
        h.update(_u32(len(dt)))
        h.update(dt)
        h.update(struct.pack(">Q", len(raw)))
        h.update(raw)
    return h.digest()


def activation_hash(manifest_root: bytes, layer: int, raw: bytes, shape) -> bytes:
    h = hashlib.sha256()
    h.update(b"ember/activation-boundary/v1\x00")
    h.update(manifest_root)
    h.update(_u32(layer))
    h.update(_u32(len(shape)))
    for dim in shape:
        h.update(_u32(dim))
    h.update(raw)
    return h.digest()


def frames(input_tokens: int, output_tokens: int, max_frame: int):
    if input_tokens == 0 or output_tokens == 0 or max_frame == 0:
        return []
    out = []
    remaining = input_tokens
    while remaining > max_frame:
        out.append(max_frame)
        remaining -= max_frame
    out.append(remaining)
    out.extend([1] * (output_tokens - 1))
    return out


def frame_offsets(sizes):
    out, cursor = [], 0
    for size in sizes:
        out.append(cursor)
        cursor += size
    return out


def routing_cap(layout, total_positions: int) -> int:
    return math.ceil(layout.routing_cap_beta * layout.top_k * total_positions /
                     layout.experts_per_layer)


def universal_share_ppm(layout, layer: int, expert: int) -> int:
    """Manifest-pinned routing-share envelope for (layer, expert), ppm of
    positions; 0 when the layout pins no envelope entry for it."""
    table = layout.routing_universal_ppm
    if not table or layer >= layout.num_layers or expert >= layout.experts_per_layer:
        return 0
    return table[layer * layout.experts_per_layer + expert]


def expert_routing_cap(layout, layer: int, expert: int, total_positions: int) -> int:
    """Per-expert routing cap with the universal-expert exemption:
    C_e = max(ceil(beta*k*T/E), ceil(u_e*T)). Mirrors core-go ExpertRoutingCap
    (integer-exact exemption term)."""
    cap = routing_cap(layout, total_positions)
    ppm = universal_share_ppm(layout, layer, expert)
    if ppm > 0:
        cap = max(cap, (ppm * total_positions + 999_999) // 1_000_000)
    return cap


def expert_param_count(layout, layer: int, expert: int) -> int:
    if layout.expert_param_counts:
        return layout.expert_param_counts[layer * layout.experts_per_layer + expert]
    return layout.expert_param_count


def head_flops_per_token(layout) -> int:
    if layout.head_param_count:
        return 2 * layout.head_param_count
    return 2 * layout.hidden_size * layout.vocab_size


def attention_flops(layout, start: int, end: int, size: int, offset: int) -> int:
    layers = end - start
    d = layout.hidden_size
    per_position = (2 * layout.attention_param_count + 2 * d * layout.experts_per_layer
                    + 2 * layout.top_k * d)
    context_sum = size * offset + size * (size + 1) // 2
    return layers * (size * per_position + 4 * d * context_sum)


def scarcity_ppm(holders_ref: int, holders: int, maximum: int) -> int:
    if holders == 0 or maximum == 0:
        return 1_000_000
    ppm = holders_ref * 1_000_000 // holders
    ppm = max(ppm, 1_000_000)
    return min(ppm, maximum * 1_000_000)


def draw_segments(seed: bytes, candidates, n: int):
    """candidates: list of (ref, work) with ref a SegmentRef. Mirrors
    core.DrawMoeSegments: weighted, without replacement, deterministic."""
    pool = list(candidates)
    out = []
    while len(out) < n and pool:
        total = sum(work for _, work in pool)
        h = hashlib.sha256()
        h.update(b"ember/moe-segment-draw/v1\x00")
        h.update(seed)
        h.update(_u32(len(out)))
        digest = h.digest()
        if total > 0:
            pick = int.from_bytes(digest[:8], "big") % total
            idx, acc = 0, 0
            for i, (_, work) in enumerate(pool):
                acc += work
                if pick < acc:
                    idx = i
                    break
        else:
            idx = int.from_bytes(digest[:4], "big") % len(pool)
        out.append(pool[idx][0])
        pool.pop(idx)
    return out
