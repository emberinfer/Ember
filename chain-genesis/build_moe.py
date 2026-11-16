"""Build a signed MoE (expert-sharded) model profile for an HF MoE model.

Emits, into a fresh output directory:
  manifest.pb / genesis.pb   signed ModelManifest (with MoeLayout) and
                             ChainGenesis (with MoE segment audit bands)
  moe_layout.json            human-readable layout + per-unit params hashes
  profiles.json              per-unit registration facts (Merkle proofs for
                             every expert unit; stage params hashes)

The publisher key is ephemeral unless EMBER_PUBLISHER_KEY_FILE names an
existing ed25519 private key file (raw 32-byte seed), matching the other
chain-genesis builders' drill posture.
"""
import argparse
import hashlib
import json
import math
import os
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path[:0] = [str(ROOT / 'proto'), str(ROOT / 'core-py')]
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from ember_core import digest, sign
from ember_core import moe as moe_core
from ember_pb2 import (Artifact, ChainGenesis, ModelManifest, MoeLayout,
                       NumericAuditPolicy, ShardSpec, ToleranceBand)
from safetensors import safe_open

# Segment tolerance bands by segment dtype, from the MOE-0 calibrations
# (2026-11-15): bf16 expert cross-device max 4.9e-3, attention incl. KV
# rebuild max 3.6e-3. FP8 expert: auditors replay FP8-natively
# (engine/fp8.py — the production caliber itself), and the native
# calibration on real Qwen3.6-35B segments measured same-scheme
# production-vs-replay at max 3.1e-6 (accumulation order only); the
# 0.008 band is set by bf16-evidence rounding plus an allowance for
# production-kernel implementation variance, sits ~3x below the
# FP8-vs-bf16 cross-caliber gap (min 2.5e-2, which must NOT pass), and
# ~50x below the smallest measured weight-swap cheater distance
# (0.42). The earlier 0.06 FP8 expert figure was the price of bf16
# replay and is retired. The FP8 attention figure remains a
# placeholder: FP8 attention segments were not separately calibrated.
BANDS = {
    'bf16': {'expert': 0.008, 'attention': 0.006},
    'fp8-e4m3-block128': {'expert': 0.008, 'attention': 0.06},
}
DEVICE_PAIRS = (('cpu', 'cpu'), ('cuda', 'cuda'), ('cpu', 'cuda'), ('cuda', 'cpu'))


def file_hash(path):
    h = hashlib.sha256()
    with open(path, 'rb') as stream:
        while block := stream.read(4 * 1024 * 1024):
            h.update(block)
    return h.digest()


def tensor_parts(source, keys):
    """Canonical (name, shape, dtype, raw bytes) parts for params hashing."""
    import torch
    parts = []
    for key in sorted(keys):
        tensor = source[key].get_tensor(key).contiguous()
        if tensor.dtype == torch.bfloat16:
            raw = tensor.view(torch.uint16).numpy().tobytes()
        elif tensor.dtype == torch.float8_e4m3fn:
            # numpy has no float8 dtype; hash the raw code bytes. The
            # engine's MoeWeights.parts uses the identical rule.
            raw = tensor.view(torch.uint8).numpy().tobytes()
        else:
            raw = tensor.numpy().tobytes()
        parts.append((key, tuple(tensor.shape), str(tensor.dtype), raw))
    return parts


def universal_table_from_counts(counts_path, layers, experts):
    """Derive the pinned routing-share envelope from a MOE-0-style counts
    dump ({layers, experts, top_k, requests: [{positions, counts}]}): per
    (layer, expert), the upper envelope of count/positions over the
    calibration requests, in ppm (ceil). The corpus must be produced by
    teacher-forced replay of the pinned weights (MOE-0 methodology); the
    chain treats the table as a falsifiable claim about that router."""
    doc = json.loads(Path(counts_path).read_text())
    if int(doc['layers']) != layers or int(doc['experts']) != experts:
        raise ValueError('counts dump shape does not match the model')
    table = [0] * (layers * experts)
    for req in doc['requests']:
        positions = int(req['positions'])
        for layer in range(layers):
            for expert in range(experts):
                count = int(req['counts'][layer][expert])
                ppm = -(-count * 1_000_000 // positions)
                idx = layer * experts + expert
                if ppm > table[idx]:
                    table[idx] = ppm
    return table


def parse_universal_sparse(spec, layers, experts):
    """'layer:expert:ppm,...' -> flat routing_universal_ppm table."""
    table = [0] * (layers * experts)
    for part in spec.split(','):
        layer, expert, ppm = (int(x) for x in part.split(':'))
        if not (0 <= layer < layers and 0 <= expert < experts):
            raise ValueError(f'universal entry out of range: {part}')
        table[layer * experts + expert] = ppm
    return table


def validate_universal_table(table, layers, experts, top_k, beta):
    """Mirror of the chain's ValidateMoeLayout envelope rules: exact
    length, each share in (0, 1], and a per-layer envelope budget of
    top_k*(ceil(beta)+1) shares (a realized share vector sums to top_k;
    blanket near-1.0 declarations sum to ~experts and are rejected)."""
    if len(table) != layers * experts:
        raise ValueError('universal table length mismatch')
    budget = top_k * (math.ceil(beta) + 1) * 1_000_000
    for layer in range(layers):
        layer_sum = 0
        for expert in range(experts):
            ppm = int(table[layer * experts + expert])
            if ppm < 0 or ppm > 1_000_000:
                raise ValueError('universal share out of range')
            layer_sum += ppm
        if layer_sum > budget:
            raise ValueError(f'universal envelope over budget at layer {layer}')


def build(model, output, chain_id, spans, price_output=1, segment_dtype='bf16',
          beta=4.0, scarcity_max=8, settlement='granular', key=None,
          misjudge_slash_mode=None, universal_ppm=None,
          universal_experts=None, universal_from_counts=None):
    # Profile-level misjudge governance (2026-11-15 decision): granular
    # profiles carry misjudge_slash_mode="on" by default — Plan B's
    # honest-segment fund is built from slash proceeds, so the
    # whole-model refund-only defaults must not apply to them. Atomic
    # profiles leave the field unset and inherit the global mode.
    if misjudge_slash_mode is None:
        misjudge_slash_mode = 'on' if settlement != 'atomic' else ''
    if misjudge_slash_mode not in ('', 'on', 'off'):
        raise ValueError(f'invalid misjudge_slash_mode: {misjudge_slash_mode}')
    model, output = Path(model).expanduser(), Path(output)
    config = json.loads((model / 'config.json').read_text())
    if 'num_experts' not in config or 'num_experts_per_tok' not in config:
        raise ValueError('not an MoE config')
    total = int(config['num_hidden_layers'])
    experts = int(config['num_experts'])
    top_k = int(config['num_experts_per_tok'])
    hidden = int(config['hidden_size'])
    vocab = int(config['vocab_size'])
    # Universal-expert exemption (2026-11-15 decision): at most one source
    # for the pinned routing-share envelope; the resolved table is checked
    # against the same rules the chain enforces at manifest validation.
    sources = [universal_ppm is not None, universal_experts is not None,
               universal_from_counts is not None]
    if sum(sources) > 1:
        raise ValueError('universal envelope sources are mutually exclusive')
    if universal_experts is not None:
        universal_ppm = parse_universal_sparse(universal_experts, total, experts)
    if universal_from_counts is not None:
        universal_ppm = universal_table_from_counts(universal_from_counts,
                                                    total, experts)
    if universal_ppm is not None:
        universal_ppm = [int(x) for x in universal_ppm]
        validate_universal_table(universal_ppm, total, experts, top_k, beta)
    files = sorted(model.glob('*.safetensors'))
    if not files or list(model.glob('*.incomplete')):
        raise ValueError('complete safetensors weights required')
    if output.exists() and any(output.iterdir()):
        raise ValueError('output must be a new empty directory')
    output.mkdir(parents=True, exist_ok=True)
    readers = [safe_open(str(path), framework='pt', device='cpu') for path in files]
    source = {key: reader for reader in readers for key in reader.keys()}

    manifest = ModelManifest(chain_id=chain_id, total_layers=total,
                             license='Apache-2.0',
                             config_hash=file_hash(model / 'config.json'))
    for path in files:
        manifest.artifacts.append(Artifact(name=path.name, sha256=file_hash(path),
                                           size=path.stat().st_size))
    # Expert params hashes + Merkle root.
    params_hashes = []
    for layer in range(total):
        for expert in range(experts):
            keys = [f'model.layers.{layer}.mlp.experts.{expert}.{proj}.weight'
                    for proj in ('gate_proj', 'up_proj', 'down_proj')]
            # FP8 checkpoints: the block scales are weight material (a
            # cheater who swaps them changes the outputs), so they join
            # the params hash whenever the checkpoint carries them. The
            # engine's expert units hash exactly the same key set.
            keys += [f'model.layers.{layer}.mlp.experts.{expert}.{proj}.weight_scale_inv'
                     for proj in ('gate_proj', 'up_proj', 'down_proj')
                     if f'model.layers.{layer}.mlp.experts.{expert}.{proj}.weight_scale_inv' in source]
            missing = [k for k in keys if k not in source]
            if missing:
                raise ValueError(f'missing expert tensors: {missing[:2]}')
            params_hashes.append(moe_core.params_hash(tensor_parts(source, keys)))
    expert_root = moe_core.expert_root(total, experts, params_hashes)
    # Layout constants from tensor shapes.
    gate = source['model.layers.0.mlp.experts.0.gate_proj.weight'].get_tensor(
        'model.layers.0.mlp.experts.0.gate_proj.weight')
    up = source['model.layers.0.mlp.experts.0.up_proj.weight'].get_tensor(
        'model.layers.0.mlp.experts.0.up_proj.weight')
    down = source['model.layers.0.mlp.experts.0.down_proj.weight'].get_tensor(
        'model.layers.0.mlp.experts.0.down_proj.weight')
    expert_params = int(gate.numel() + up.numel() + down.numel())
    q = source['model.layers.0.self_attn.q_proj.weight'].get_tensor(
        'model.layers.0.self_attn.q_proj.weight')
    kk = source['model.layers.0.self_attn.k_proj.weight'].get_tensor(
        'model.layers.0.self_attn.k_proj.weight')
    v = source['model.layers.0.self_attn.v_proj.weight'].get_tensor(
        'model.layers.0.self_attn.v_proj.weight')
    o = source['model.layers.0.self_attn.o_proj.weight'].get_tensor(
        'model.layers.0.self_attn.o_proj.weight')
    router = source['model.layers.0.mlp.gate.weight'].get_tensor(
        'model.layers.0.mlp.gate.weight')
    attention_params = int(q.numel() + kk.numel() + v.numel() + o.numel() + router.numel())
    head_dim = hidden // int(config['num_attention_heads'])
    layout = MoeLayout(
        num_layers=total, hidden_size=hidden, experts_per_layer=experts,
        top_k=top_k, expert_param_count=expert_params,
        expert_weights_root=expert_root, attention_param_count=attention_params,
        vocab_size=vocab, num_q_heads=int(config['num_attention_heads']),
        num_kv_heads=int(config.get('num_key_value_heads', config['num_attention_heads'])),
        head_dim=head_dim, segment_dtype=segment_dtype,
        engine_impl_id='olmoe-moe-v1', routing_cap_beta=beta,
        scarcity_max=scarcity_max, segment_settlement=settlement,
        misjudge_slash_mode=misjudge_slash_mode,
        routing_universal_ppm=universal_ppm or [],
        max_frame_positions=512)
    manifest.moe_layout.CopyFrom(layout)
    # Attention stage specs (params hash over the stage's own tensors:
    # attention + norms + router only, never expert weights).
    stage_facts = []
    for index, (start, end) in enumerate(spans):
        keys = []
        for layer in range(start, end):
            keys.extend(moe_core.stage_tensor_keys(lambda k: k in source, layer))
        if start == 0:
            keys.append('model.embed_tokens.weight')
        if end == total:
            keys.extend(['model.norm.weight', 'lm_head.weight'])
        keys = sorted(set(k for k in keys if k in source))
        ph = moe_core.params_hash(tensor_parts(source, keys))
        manifest.shards.append(ShardSpec(
            index=index, unit_kind='attention', start_layer=start, end_layer=end,
            has_embedding=(start == 0), has_lm_head=(end == total),
            parameters_hash=ph))
        stage_facts.append({'index': index, 'start_layer': start, 'end_layer': end,
                            'params_hash': ph.hex()})
    if key is None:
        key = Ed25519PrivateKey.generate()
    sign(manifest, key)
    root = digest(manifest)
    bands = []
    for kind in ('expert', 'attention'):
        for producer, reference in DEVICE_PAIRS:
            bands.append(ToleranceBand(
                producer_engine='olmoe-moe-v1', producer_device=producer,
                reference_engine='olmoe-moe-v1', reference_device=reference,
                tolerance=BANDS[segment_dtype][kind], mode='enforce',
                segment_kind=kind))
    genesis = ChainGenesis(
        chain_id=chain_id, model_manifest_root=root,
        execution_profile_ver='olmoe-moe-v1', price_output=price_output,
        numeric_audit_policy=NumericAuditPolicy(
            metric='union-interval-v1', bands=bands, top_k=16,
            calibration_ref=hashlib.sha256(b'moe0-calibration-2026-11-15').digest()))
    sign(genesis, key)
    (output / 'manifest.pb').write_bytes(manifest.SerializeToString())
    (output / 'genesis.pb').write_bytes(genesis.SerializeToString())
    facts = {
        'chain_id': chain_id, 'manifest_root': root.hex(),
        'layout': {'num_layers': total, 'experts_per_layer': experts,
                   'top_k': top_k, 'hidden_size': hidden,
                   'expert_param_count': expert_params,
                   'attention_param_count': attention_params,
                   'routing_cap_beta': beta, 'scarcity_max': scarcity_max,
                   'segment_settlement': settlement, 'segment_dtype': segment_dtype,
                   'misjudge_slash_mode': misjudge_slash_mode,
                   'routing_universal_entries':
                       sum(1 for x in (universal_ppm or []) if x)},
        'routing_universal_ppm': universal_ppm or [],
        'expert_params_hashes': [h.hex() for h in params_hashes],
        'stages': stage_facts,
        'expert_proofs': {
            f'{layer}:{expert}': [p.hex() for p in moe_core.expert_proof(
                total, experts, params_hashes, layer, expert)]
            for layer in range(total) for expert in range(experts)},
    }
    (output / 'moe_layout.json').write_text(json.dumps(facts, indent=1))
    return facts


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('model')
    parser.add_argument('output')
    parser.add_argument('--chain-id', required=True)
    parser.add_argument('--spans', default='0:8,8:16',
                        help='attention stage spans, e.g. 0:8,8:16')
    parser.add_argument('--price-output', type=int, default=1)
    parser.add_argument('--segment-dtype', default='bf16')
    parser.add_argument('--beta', type=float, default=4.0)
    parser.add_argument('--settlement', default='granular',
                        choices=('granular', 'atomic'))
    parser.add_argument('--misjudge-slash-mode', default=None,
                        choices=('on', 'off'),
                        help='default: on for granular, unset for atomic')
    parser.add_argument('--universal-experts', default=None,
                        help='sparse pinned routing-share envelope, '
                             '"layer:expert:ppm,..." (exclusive with '
                             '--universal-from-counts)')
    parser.add_argument('--universal-from-counts', default=None,
                        help='MOE-0-style counts dump to derive the pinned '
                             'routing-share envelope from')
    args = parser.parse_args()
    spans = []
    for part in args.spans.split(','):
        a, b = part.split(':')
        spans.append((int(a), int(b)))
    key = None
    key_file = os.environ.get('EMBER_PUBLISHER_KEY_FILE')
    if key_file:
        key = Ed25519PrivateKey.from_private_bytes(Path(key_file).read_bytes())
    facts = build(args.model, args.output, args.chain_id, spans,
                  price_output=args.price_output,
                  segment_dtype=args.segment_dtype, beta=args.beta,
                  settlement=args.settlement,
                  misjudge_slash_mode=args.misjudge_slash_mode,
                  universal_experts=args.universal_experts,
                  universal_from_counts=args.universal_from_counts, key=key)
    print(json.dumps({'chain_id': facts['chain_id'],
                      'manifest_root': facts['manifest_root'],
                      'layout': facts['layout']}, indent=1))


if __name__ == '__main__':
    main()
