"""Build immutable, independently deployable Qwen2 pipeline shard artifacts."""
import argparse
import hashlib
import json
import shutil
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path[:0] = [str(ROOT / 'proto'), str(ROOT / 'core-py')]
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from ember_core import digest, sign
from ember_pb2 import (Artifact, AuditParams, ChainGenesis, ModelManifest,
                       NumericAuditPolicy, ShardSpec, ToleranceBand)
from safetensors import safe_open
from safetensors.torch import save_file
import torch
import transformers


SHARED_FILES = ('config.json', 'generation_config.json', 'tokenizer.json',
                'tokenizer_config.json', 'vocab.json', 'merges.txt')


def file_hash(path):
    h = hashlib.sha256()
    with open(path, 'rb') as stream:
        while block := stream.read(4 * 1024 * 1024):
            h.update(block)
    return h.digest()


def ranges(total, count):
    if count < 2 or count > total:
        raise ValueError('shard count must be between 2 and the layer count')
    return [(i * total // count, (i + 1) * total // count) for i in range(count)]


def build(model, output, count=2, price_output=1, union_tolerance=None,
          calibration_ref=None, separate_boundaries=False):
    model, output = Path(model).expanduser(), Path(output)
    config = json.loads((model / 'config.json').read_text())
    if config.get('model_type') != 'qwen2':
        raise ValueError('only Qwen2 is supported')
    total = int(config['num_hidden_layers'])
    files = sorted(model.glob('*.safetensors'))
    if not files or list(model.glob('*.incomplete')) or list(model.glob('*.part')):
        raise ValueError('complete safetensors weights required')
    if output.exists() and any(output.iterdir()):
        raise ValueError('shard output must be a new empty directory')
    output.mkdir(parents=True, exist_ok=True)
    readers = [safe_open(str(path), framework='pt', device='cpu') for path in files]
    source = {key: reader for reader in readers for key in reader.keys()}
    topology = ranges(total, count)
    if separate_boundaries:
        topology = [(0, 0), *topology, (total, total)]
    layout = count + 2 if separate_boundaries else count
    manifest = ModelManifest(chain_id=f'ember-qwen2.5-1.5b-pipeline{layout}'+('-units' if separate_boundaries else ''),
                             total_layers=total, license='Apache-2.0',
                             config_hash=file_hash(model / 'config.json'))
    graph = hashlib.sha256()
    for index, (start, end) in enumerate(topology):
        keys = [key for key in source if any(
            key.startswith(f'model.layers.{layer}.') for layer in range(start, end))]
        if index == 0:
            keys.append('model.embed_tokens.weight')
        if index == layout - 1:
            keys.extend(['model.norm.weight', 'model.embed_tokens.weight'])
        keys = sorted(set(keys))
        tensors = {key: source[key].get_tensor(key) for key in keys}
        name = f'shard-{index:03d}.safetensors'
        path = output / name
        save_file(tensors, str(path))
        checksum = file_hash(path)
        graph.update(index.to_bytes(4, 'big') + checksum)
        manifest.artifacts.append(Artifact(name=name, sha256=checksum,
                                           size=path.stat().st_size))
        parameters = hashlib.sha256(
            b'ember/shard-parameters/v1\0' + index.to_bytes(4, 'big') + checksum).digest()
        manifest.shards.append(ShardSpec(
            unit_kind=("embedding" if start == end == 0 else "lm_head" if start == end == total else "layers") if separate_boundaries else "",
            index=index, start_layer=start, end_layer=end,
            has_embedding=index == 0, has_lm_head=index == layout - 1,
            artifacts=[name], parameters_hash=parameters))
    manifest.graph_hash = graph.digest()
    for name in SHARED_FILES:
        source_path = model / name
        if source_path.is_file():
            shutil.copyfile(source_path, output / name)
            manifest.artifacts.append(Artifact(name=name, sha256=file_hash(source_path),
                                               size=source_path.stat().st_size))
    key = Ed25519PrivateKey.generate()
    sign(manifest, key)
    genesis = ChainGenesis(
        chain_id=manifest.chain_id, model_manifest_root=digest(manifest),
        tokenizer_hash=file_hash(model / 'tokenizer.json'), quant='none', dtype='bfloat16',
        execution_profile_ver=f'transformers-qwen2.5-pipeline{layout}'+('-units-v1' if separate_boundaries else '-v1'),
        tolerance_table_ref='sdpa-same-profile-bf16-required', price_input=1,
        price_output=price_output, audit_params=AuditParams(
            new_node_percent=30, mature_node_percent=5, floor_percent=2,
            inline_percent=3, canary_percent=1, slash_multiplier=20,
            min_stake_multiplier=50, exposure_divisor=5, dispute_seconds=60,
            numeric_tolerance=0.001))
    if union_tolerance is not None:
        if (not 0 < union_tolerance < 1 or calibration_ref is None or
                len(calibration_ref) != 32):
            raise ValueError('union audit requires a finite threshold and 32-byte calibration ref')
        genesis.numeric_audit_policy.CopyFrom(NumericAuditPolicy(
            metric='union-interval-v1', layers=['0', '14', '27'], top_k=16,
            calibration_ref=calibration_ref, bands=[ToleranceBand(
                producer_engine=(f'pipeline[transformers-shard/{transformers.__version__}'
                                 f'|torch={torch.__version__}]'),
                producer_device='cpu',
                reference_engine=(f'transformers/{transformers.__version__}'
                                  f'|torch={torch.__version__}'),
                reference_device='cpu', tolerance=union_tolerance,
                mode='enforce')]))
    sign(genesis, key)
    (output / 'manifest.pb').write_bytes(manifest.SerializeToString(deterministic=True))
    (output / 'genesis.pb').write_bytes(genesis.SerializeToString(deterministic=True))
    (output / 'topology.json').write_text(json.dumps({
        'chain_id': manifest.chain_id, 'total_layers': total,
        'shards': [{'index': i, 'start_layer': start, 'end_layer': end,
                    'has_embedding': i == 0, 'has_lm_head': i == layout - 1}
                   for i, (start, end) in enumerate(topology)]}, indent=2) + '\n')
    return genesis, manifest


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--model', default='~/models/Qwen2.5-1.5B-Instruct')
    parser.add_argument('--output', required=True)
    parser.add_argument('--shards', type=int, default=2)
    parser.add_argument('--separate-boundaries', action='store_true')
    parser.add_argument('--price-output', type=int, default=1)
    parser.add_argument('--union-tolerance', type=float)
    parser.add_argument('--calibration-ref', help='hex SHA-256 of retained calibration evidence')
    args = parser.parse_args()
    if args.price_output < 1:
        raise ValueError('price output must be positive')
    calibration = bytes.fromhex(args.calibration_ref) if args.calibration_ref else None
    build(args.model, args.output, args.shards, args.price_output,
          args.union_tolerance, calibration, args.separate_boundaries)
