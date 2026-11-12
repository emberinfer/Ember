"""Build a signed Qwen chain genesis from complete, locally verified artifacts."""
import argparse
import hashlib
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path[:0] = [str(ROOT / 'proto'), str(ROOT / 'core-py')]
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from google.protobuf.json_format import MessageToDict
from ember_core import digest, sign, verify
from ember_pb2 import Artifact, AuditParams, ChainGenesis, ModelManifest


def sha256(path):
    h = hashlib.sha256()
    with open(path, 'rb') as stream:
        while block := stream.read(4 * 1024 * 1024):
            h.update(block)
    return h.digest()


def build(model, output, chain_id='ember-qwen2.5-1.5b',
          execution_profile_ver='transformers-qwen2.5-v1'):
    model = Path(model).expanduser()
    if not (model / 'config.json').is_file():
        raise ValueError('config.json missing')
    if list(model.glob('*.incomplete')) or list(model.glob('*.part')):
        raise ValueError('model download incomplete')
    weights = sorted(model.glob('*.safetensors'))
    if not weights:
        raise ValueError('safetensors weights missing')
    config = json.loads((model / 'config.json').read_text())
    if config.get('num_hidden_layers', 0) < 28:
        raise ValueError('unexpected model configuration')
    files = sorted({*weights, *(p for p in model.iterdir() if p.is_file() and p.name in
                             {'config.json', 'generation_config.json', 'tokenizer.json',
                              'tokenizer_config.json', 'special_tokens_map.json',
                              'tokenizer.model', 'vocab.json', 'merges.txt'})})
    graph = hashlib.sha256(b''.join(p.name.encode() + sha256(p) for p in weights)).digest()
    manifest = ModelManifest(chain_id=chain_id, config_hash=sha256(model / 'config.json'),
                             graph_hash=graph, license='Apache-2.0')
    for file in files:
        manifest.artifacts.append(Artifact(name=file.name, sha256=sha256(file), size=file.stat().st_size))
    output = Path(output)
    paths = [output / name for name in ('genesis.json', 'manifest.pb', 'genesis.pb')]
    if any(path.exists() for path in paths):
        if not all(path.is_file() for path in paths):
            raise ValueError('published genesis is incomplete; manual recovery required')
        existing_manifest = ModelManifest.FromString(paths[1].read_bytes())
        existing_genesis = ChainGenesis.FromString(paths[2].read_bytes())
        verify(existing_manifest)
        verify(existing_genesis)
        if (existing_manifest.chain_id != chain_id or existing_genesis.chain_id != chain_id or
                existing_genesis.model_manifest_root != digest(existing_manifest) or
                existing_genesis.tokenizer_hash != sha256(model / 'tokenizer.json') or
                existing_genesis.execution_profile_ver != execution_profile_ver):
            raise ValueError('published genesis identity mismatch; use a new chain ID')
        existing_manifest.ClearField('signer')
        existing_manifest.ClearField('signature')
        if existing_manifest.SerializeToString(deterministic=True) != manifest.SerializeToString(deterministic=True):
            raise ValueError('model artifacts changed; published genesis is immutable')
        return json.loads(paths[0].read_text())
    key = Ed25519PrivateKey.generate()
    sign(manifest, key)
    genesis = ChainGenesis(chain_id=chain_id, model_manifest_root=digest(manifest),
                           tokenizer_hash=sha256(model / 'tokenizer.json'), quant='none', dtype='bfloat16',
                           execution_profile_ver=execution_profile_ver,
                           tolerance_table_ref='cpu-bf16-initial-uncalibrated',
                           price_input=1, price_output=1,
                           audit_params=AuditParams(new_node_percent=30, mature_node_percent=5,
                                                    floor_percent=2, inline_percent=3, canary_percent=1,
                                                    slash_multiplier=20, min_stake_multiplier=50,
                                                    exposure_divisor=5, dispute_seconds=60,
                                                    numeric_tolerance=0.001))
    sign(genesis, key)
    output.mkdir(parents=True, exist_ok=True)
    document = {'genesis': MessageToDict(genesis, preserving_proto_field_name=True),
                'manifest': MessageToDict(manifest, preserving_proto_field_name=True),
                'sampled_layers': [0, 14, 27], 'cpu_profile': {'device': 'cpu', 'dtype': 'bfloat16'},
                'gpu_profile': {'device': 'cuda', 'dtype': 'bfloat16', 'min_available_gib': 16,
                                'max_process_gib': 8},
                'governance_unresolved': {'ledger_route': None, 'credit_scope': None,
                                          'future_launch_model': None}}
    (output / 'genesis.json').write_text(json.dumps(document, indent=2, sort_keys=True) + '\n')
    (output / 'manifest.pb').write_bytes(manifest.SerializeToString(deterministic=True))
    (output / 'genesis.pb').write_bytes(genesis.SerializeToString(deterministic=True))
    return document


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--model', default='~/models/Qwen2.5-1.5B-Instruct')
    parser.add_argument('--output', default='chain-genesis/ember-qwen2.5-1.5b')
    parser.add_argument('--chain-id', default='ember-qwen2.5-1.5b')
    parser.add_argument('--execution-profile-ver', default='transformers-qwen2.5-v1')
    args = parser.parse_args()
    build(args.model, args.output, args.chain_id, args.execution_profile_ver)
