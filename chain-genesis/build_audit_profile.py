"""Publish a separate, immutable experimental union-audit chain; never edit the base profile."""
import argparse
import hashlib
import json
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[1]
sys.path[:0] = [str(ROOT / p) for p in ('proto', 'core-py', 'node')]
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from google.protobuf.json_format import MessageToDict, ParseDict
from ember_core import digest, sign, verify
from ember_pb2 import ChainGenesis, ModelManifest, NumericAuditPolicy
from auditor.policy import validate_policy


def unsigned(message):
    result = type(message)()
    result.CopyFrom(message)
    result.ClearField('signer'); result.ClearField('signature')
    return result.SerializeToString(deterministic=True)


DEFAULT_CHAIN_ID = 'ember-qwen2.5-1.5b-union-v1'
DEFAULT_EXECUTION_PROFILE = 'transformers-qwen2.5-union-v1'
DEFAULT_SCOPE = 'Experimental corpus calibration, not a public production chain'


def build(source, output, policy_file, chain_id=DEFAULT_CHAIN_ID,
          execution_profile_ver=DEFAULT_EXECUTION_PROFILE, scope=DEFAULT_SCOPE):
    source, output = Path(source), Path(output)
    genesis = ChainGenesis.FromString((source / 'genesis.pb').read_bytes())
    manifest = ModelManifest.FromString((source / 'manifest.pb').read_bytes())
    verify(genesis); verify(manifest)
    if genesis.model_manifest_root != digest(manifest):
        raise ValueError('source manifest binding mismatch')
    policy = ParseDict(json.loads(Path(policy_file).read_text()), NumericAuditPolicy())
    validate_policy(policy)
    if source.resolve() == output.resolve() or genesis.chain_id == chain_id:
        raise ValueError('new profile requires a separate source and chain')
    manifest.chain_id = genesis.chain_id = chain_id
    genesis.execution_profile_ver = execution_profile_ver
    genesis.numeric_audit_policy.CopyFrom(policy)
    genesis.tolerance_table_ref = 'sha256:' + hashlib.sha256(policy.SerializeToString(deterministic=True)).hexdigest()
    paths = [output / name for name in ('genesis.pb', 'manifest.pb', 'genesis.json')]
    if any(p.exists() for p in paths):
        if not all(p.is_file() for p in paths):
            raise ValueError('partial published profile; manual recovery required')
        old = ChainGenesis.FromString(paths[0].read_bytes())
        old_manifest = ModelManifest.FromString(paths[1].read_bytes())
        verify(old); verify(old_manifest)
        genesis.model_manifest_root = digest(old_manifest)
        if old.model_manifest_root != digest(old_manifest) or unsigned(old) != unsigned(genesis) or unsigned(old_manifest) != unsigned(manifest):
            raise ValueError('published profile is immutable; use a new version')
        return old
    key = Ed25519PrivateKey.generate()
    sign(manifest, key)
    genesis.model_manifest_root = digest(manifest)
    sign(genesis, key)
    output.mkdir(parents=True, exist_ok=True)
    paths[0].write_bytes(genesis.SerializeToString(deterministic=True))
    paths[1].write_bytes(manifest.SerializeToString(deterministic=True))
    paths[2].write_text(json.dumps({'experimental': True,
        'scope': scope,
        'genesis': MessageToDict(genesis, preserving_proto_field_name=True),
        'manifest': MessageToDict(manifest, preserving_proto_field_name=True)}, indent=2)+'\n')
    return genesis


if __name__ == '__main__':
    cli = argparse.ArgumentParser()
    cli.add_argument('--source', default=str(ROOT / 'chain-genesis/ember-qwen2.5-1.5b'))
    cli.add_argument('--output', default=str(ROOT / 'chain-genesis/ember-qwen2.5-1.5b-union-v1'))
    cli.add_argument('--policy', required=True)
    cli.add_argument('--chain-id', default=DEFAULT_CHAIN_ID)
    cli.add_argument('--execution-profile-ver', default=DEFAULT_EXECUTION_PROFILE)
    cli.add_argument('--scope', default=DEFAULT_SCOPE)
    args = cli.parse_args()
    print(build(args.source, args.output, args.policy, args.chain_id,
                args.execution_profile_ver, args.scope).chain_id)
