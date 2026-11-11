"""Prepare and submit publisher-owned signed release artifacts."""
import argparse
import base64
import hashlib
import json
from pathlib import Path

from ember_core import digest, sign, verify
from ember_pb2 import ChainGenesis, ModelManifest
from fetcher import verify_local
from .join import identity, _chain_json, _chain_tx


def verify_release(opts):
    genesis = ChainGenesis.FromString(Path(opts.genesis).read_bytes())
    manifest = ModelManifest.FromString(Path(opts.manifest).read_bytes())
    verify(genesis)
    verify(manifest)
    if genesis.chain_id != manifest.chain_id or genesis.model_manifest_root != digest(manifest):
        raise ValueError('profile manifest root mismatch')
    for artifact in manifest.artifacts:
        verify_local(artifact, Path(opts.model_path).expanduser())
    result = dict(result='PASS', manifest_root=digest(manifest).hex(),
                  profile_hash=digest(genesis).hex(), artifacts=len(manifest.artifacts))
    if opts.conformance:
        from engine import load_engine, require_full_evidence
        from conformance import CASES, validate_step
        adapter = load_engine('transformers', model_path=opts.model_path, device=opts.device)
        require_full_evidence(adapter)
        rows = []
        for prompt, seed in CASES:
            steps = list(adapter.generate(prompt, seed, 2))
            if not 1 <= len(steps) <= 2:
                raise ValueError('invalid conformance token count')
            for step in steps:
                validate_step(step)
            replay = list(adapter.replay(prompt, seed, [s.token for s in steps]))
            if [s.token for s in replay] != [s.token for s in steps]:
                raise ValueError('forced-prefix replay mismatch')
            rows.append(dict(seed=seed, tokens=len(steps)))
        adapter.close()
        result['conformance'] = rows
    print(json.dumps(result, sort_keys=True))


def prepare(opts):
    key, _ = identity(opts.state_dir)
    genesis = ChainGenesis.FromString(Path(opts.genesis).read_bytes())
    manifest = ModelManifest.FromString(Path(opts.manifest).read_bytes())
    verify(genesis)
    verify(manifest)
    if genesis.model_manifest_root != digest(manifest):
        raise ValueError('template manifest root mismatch')
    if manifest.shards:
        raise ValueError('origin publication requires original artifacts; derived shard releases use the shard builder')
    for artifact in manifest.artifacts:
        verify_local(artifact, Path(opts.model_path).expanduser())
        artifact.source = 'git:' + artifact.name
    manifest.chain_id = opts.model_id
    sign(manifest, key)
    genesis.chain_id = opts.model_id
    genesis.execution_profile_ver = opts.profile_version
    genesis.model_manifest_root = digest(manifest)
    sign(genesis, key)
    g = genesis.SerializeToString(deterministic=True)
    m = manifest.SerializeToString(deterministic=True)
    action = dict(type='register_model', model_id=opts.model_id,
                  genesis_hash=hashlib.sha256(g).hexdigest(),
                  profile_hash=digest(genesis).hex(),
                  genesis=base64.b64encode(g).decode(),
                  manifest=base64.b64encode(m).decode(),
                  origin=dict(repository=opts.origin, revision=opts.revision))
    if opts.previous_profile_hash:
        action['previous_profile_hash'] = opts.previous_profile_hash
    output = Path(opts.output)
    output.mkdir(parents=True, exist_ok=False)
    (output / 'genesis.pb').write_bytes(g)
    (output / 'manifest.pb').write_bytes(m)
    (output / 'registration.json').write_text(json.dumps(action, indent=2) + '\n')
    print(json.dumps(dict(model_id=opts.model_id, manifest_root=digest(manifest).hex(),
                          profile_hash=digest(genesis).hex())))


def main(args):
    parser = argparse.ArgumentParser(prog='ember-node profile')
    sub = parser.add_subparsers(dest='command', required=True)
    p = sub.add_parser('verify')
    for name in ('genesis', 'manifest', 'model-path'):
        p.add_argument('--' + name, required=True)
    p.add_argument('--conformance', action='store_true')
    p.add_argument('--device', choices=('cpu', 'cuda', 'auto'), default='cpu')
    p = sub.add_parser('prepare')
    for name in ('state-dir', 'genesis', 'manifest', 'model-path', 'model-id',
                 'profile-version', 'origin', 'revision', 'output'):
        p.add_argument('--' + name, required=True)
    p.add_argument('--previous-profile-hash')
    for name in ('submit', 'endorse', 'revoke', 'claim-bond', 'challenge'):
        p = sub.add_parser(name)
        for flag in ('state-dir', 'chain-rpc', 'ledger-chain-id'):
            p.add_argument('--' + flag, required=True)
        p.add_argument('--chain-cli', default='emberchain')
        if name in ('submit', 'challenge'):
            p.add_argument('--action-file', required=True)
        elif name in ('endorse', 'revoke'):
            p.add_argument('--manifest-root', required=True)
        else:
            p.add_argument('--model-id', required=True)
    opts = parser.parse_args(args)
    if opts.command == 'verify':
        verify_release(opts)
        return
    if opts.command == 'prepare':
        prepare(opts)
        return
    _, ident = identity(opts.state_dir)
    state = _chain_json(opts.chain_cli, opts.chain_rpc, '/state')
    if state['parameters']['chain_id'] != opts.ledger_chain_id:
        raise ValueError('ledger chain ID mismatch')
    if opts.command in ('submit', 'challenge'):
        action = json.loads(Path(opts.action_file).read_text())
        expected = 'register_model' if opts.command == 'submit' else 'challenge_publication'
        if action.get('type') != expected:
            raise ValueError('wrong action type')
    elif opts.command in ('endorse', 'revoke'):
        action = dict(type='endorse_profile' if opts.command == 'endorse' else 'revoke_endorsement',
                      profile_hash=opts.manifest_root)
    else:
        action = dict(type='claim_publisher_bond', model_id=opts.model_id)
    _chain_tx(opts, Path(opts.state_dir) / 'identity.key',
              state['accounts'][ident]['nonce'] + 1, action)


if __name__ == '__main__':
    import sys
    main(sys.argv[1:])
