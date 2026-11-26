"""Prepare and submit publisher-owned signed release artifacts."""
import argparse
import base64
import hashlib
import json
import subprocess
import importlib.util
import os
import re
import tempfile
from types import SimpleNamespace
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
        from engine import require_full_evidence
        from conformance import CASES, validate_step
        from engine.adapters.transformers import TransformersEngine
        from auditor.policy import NUMERIC_EXECUTION_PROFILES, validate_policy, select_band, engine_id
        from auditor.gpu import gpu_engine_id
        from auditor.numeric import LEGACY, RELATIVE_L2, RANK_PROB, compare
        policy = genesis.numeric_audit_policy
        legacy = (genesis.execution_profile_ver not in NUMERIC_EXECUTION_PROFILES
                  and not policy.bands and policy.metric in ('', LEGACY, RELATIVE_L2))
        if not legacy:
            validate_policy(policy)
        adapter = TransformersEngine(opts.model_path, device=opts.device,
                                     layers=tuple(map(int, policy.layers)) if policy.layers else None)
        try:
            metric = policy.metric or LEGACY
            if legacy:
                tolerance = genesis.audit_params.numeric_tolerance if genesis.HasField('audit_params') else .001
                result['conformance_policy'] = 'legacy exact-ID (' + metric + ')'
            else:
                runtime_id = (gpu_engine_id if metric == RANK_PROB else engine_id)(
                    adapter.capabilities(), adapter.resource_metadata())
                band = select_band(policy, runtime_id, adapter.device, adapter)
                if band is None:
                    raise ValueError('policy has no calibrated band for the actual engine/torch/device runtime')
                tolerance = band.tolerance
            require_full_evidence(adapter)
            layers = set(policy.layers) if policy.layers else set(map(str, adapter.layers))
            rows = []
            for prompt, seed in CASES:
                steps = list(adapter.generate(prompt, seed, 2))
                if not 1 <= len(steps) <= 2:
                    raise ValueError('invalid conformance token count')
                for step in steps:
                    validate_step(step)
                    if set(step.activations) != layers:
                        raise ValueError('conformance evidence does not match signed policy layers')
                replay = list(adapter.replay(prompt, seed, [s.token for s in steps]))
                if [s.token for s in replay] != [s.token for s in steps]:
                    raise ValueError('forced-prefix replay mismatch')
                distance, _ = compare(steps, replay, metric)
                if not distance <= tolerance:
                    raise ValueError('conformance replay exceeds signed numeric tolerance' if legacy else
                                     'conformance replay exceeds supplied calibration band')
                rows.append(dict(seed=seed, tokens=len(steps), distance=distance))
        finally:
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
    config_path = Path(opts.model_path).expanduser() / 'config.json'
    manifest.config_bytes = config_path.read_bytes()
    manifest.canonical_config = subprocess.check_output(
        [getattr(opts, 'chain_cli', 'emberchain'), 'canonical-config', str(config_path)])
    shapes, compute = publication_compute(opts, manifest)
    sources = [(opts.origin, opts.revision)] + [(s.repository, s.revision) for s in manifest.sources]
    manifest.ClearField('sources')
    seen = set()
    for repository, revision in sources:
        if (repository, revision) not in seen:
            manifest.sources.add(repository=repository, revision=revision)
            seen.add((repository, revision))
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
                  origin=dict(repository=opts.origin, revision=opts.revision),
                  pricing=dict(target_executors=1, target_auditors=1, executor_weight=80,
                               auditor_weight=15, consumer_weight=5),
                  audit_comp=dict(formula_ver=2, o_fixed=1, margin_bps=10000,
                                  audit_share_bps=1500, benchmark_ref='ab' * 32))
    action.update(tensor_shapes=shapes, declared_total_params=compute['TotalParams'],
                  declared_active_params=compute['ActiveParams'],
                  declared_flops_per_token=compute['FlopsPerToken'])
    if opts.previous_profile_hash:
        action['previous_profile_hash'] = opts.previous_profile_hash
    output = Path(opts.output)
    output.mkdir(parents=True, exist_ok=False)
    (output / 'genesis.pb').write_bytes(g)
    (output / 'manifest.pb').write_bytes(m)
    (output / 'registration.json').write_text(json.dumps(action, indent=2) + '\n')
    print(json.dumps(dict(model_id=opts.model_id, manifest_root=digest(manifest).hex(),
                          profile_hash=digest(genesis).hex())))



def publication_compute(opts, manifest):
    """Read real safetensors headers; use the ledger's compute implementation."""
    from safetensors import safe_open
    shapes = []
    seen = set()
    for path in sorted(Path(opts.model_path).expanduser().glob('*.safetensors')):
        with safe_open(str(path), framework='pt', device='cpu') as reader:
            for name in sorted(reader.keys()):
                if name in seen:
                    raise ValueError('duplicate tensor across weight files: ' + name)
                seen.add(name)
                tensor = reader.get_slice(name)
                shapes.append(dict(name=name, shape=tensor.get_shape(), dtype=tensor.get_dtype()))
    with tempfile.TemporaryDirectory(prefix='ember-shapes-') as work:
        path = Path(work) / 'shapes.json'
        path.write_text(json.dumps(shapes))
        compute = json.loads(subprocess.check_output([
            opts.chain_cli, 'derive-compute', '--config',
            str(Path(opts.model_path).expanduser() / 'config.json'), '--shapes', str(path)], text=True))
    manifest.total_layers = len(compute['LayerFlops'])
    del manifest.layer_flops[:]
    manifest.layer_flops.extend(compute['LayerFlops'])
    manifest.embedding_flops = compute['EmbeddingFlops']
    manifest.head_flops = compute['HeadFlops']
    return shapes, compute


def _builder(name):
    root = Path(__file__).resolve().parents[2] / 'chain-genesis'
    spec = importlib.util.spec_from_file_location('ember_builder_' + name, root / (name + '.py'))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module.build


def _published_record(opts):
    result = subprocess.run([opts.chain_cli, 'query', '--rpc', opts.chain_rpc,
                             '--path', '/model/' + opts.model_id],
                            capture_output=True, text=True, timeout=30)
    if result.returncode:
        if re.search(r'query code=1 log=not found\s*$', result.stderr):
            return None
        raise ValueError('cannot establish publication status: ' + result.stderr.strip())
    return json.loads(result.stdout)


def readback(opts, record, ident):
    action = json.loads((Path(opts.output) / 'registration.json').read_text())
    publication = (record or {}).get('publication')
    if not publication or publication.get('invalid') or publication.get('publisher') != ident or int(publication.get('registered_height', 0)) <= 0:
        raise ValueError('readback requires a valid committed publication by this publisher')
    for field in ('genesis', 'manifest'):
        local = (Path(opts.output) / (field + '.pb')).read_bytes()
        if base64.b64decode(publication[field], validate=True) != local or base64.b64decode(action[field], validate=True) != local:
            raise ValueError('published ' + field + ' differs from local bytes; refusing another bond')
    graw = (Path(opts.output) / 'genesis.pb').read_bytes()
    g = ChainGenesis.FromString(graw)
    m = ModelManifest.FromString((Path(opts.output) / 'manifest.pb').read_bytes())
    verify(g); verify(m)
    if (g.chain_id != opts.model_id or m.chain_id != opts.model_id or g.signer != m.signer or
            hashlib.sha256(g.signer).hexdigest() != ident or
            hashlib.sha256(graw).hexdigest() != record['genesis_hash'] or
            digest(g).hex() != record['profile_hash'] or
            digest(m).hex() != publication['manifest_root'] or g.model_manifest_root != digest(m) or
            publication['origin'] != action['origin']):
        raise ValueError('committed publication identity/hash/root/origin mismatch')
    return dict(result='PASS', model_id=opts.model_id, publisher=ident,
                registered_height=publication['registered_height'], genesis_sha256=record['genesis_hash'],
                profile_hash=record['profile_hash'], manifest_root=publication['manifest_root'])


def publish(opts):
    """Build atomically, verify before spending, and resolve repeats read-only."""
    from auditor.policy import validate_policy
    from google.protobuf.json_format import ParseDict
    from ember_pb2 import NumericAuditPolicy
    from urllib.parse import urlparse
    policy = ParseDict(json.loads(Path(opts.policy).read_text()), NumericAuditPolicy())
    validate_policy(policy)
    config = json.loads((Path(opts.model_path).expanduser() / 'config.json').read_text())
    if any(int(layer) >= config['num_hidden_layers'] for layer in policy.layers):
        raise ValueError('policy samples a layer outside the actual model; provide measured policy for this architecture')
    origin = urlparse(opts.origin)
    if (origin.scheme != 'https' or not origin.hostname or origin.username or origin.query or origin.fragment or
            not re.fullmatch('[0-9a-f]{40}', opts.revision)):
        raise ValueError('origin requires an HTTPS repository and a full lowercase 40-hex revision')
    _, ident = identity(opts.state_dir)
    parameters = _chain_json(opts.chain_cli, opts.chain_rpc, '/parameters')
    if parameters['chain_id'] != opts.ledger_chain_id:
        raise ValueError('ledger chain ID mismatch')
    record = _published_record(opts)
    output = Path(opts.output).absolute()
    if record and not record.get('publication'):
        raise ValueError('model ID already exists without a publication; refusing to overwrite it')
    if record and record.get('publication'):
        # Never rebuild with a new random template or rebroadcast an uncertain tx.
        print(json.dumps(readback(opts, record, ident), sort_keys=True))
        return
    if not output.exists():
        output.parent.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(prefix='.ember-publish-', dir=output.parent) as work:
            work = Path(work)
            _builder('build')(opts.model_path, work / 'base', opts.model_id + '-base', opts.profile_version)
            _builder('build_audit_profile')(work / 'base', work / 'audit', opts.policy,
                                          opts.model_id + '-audit', opts.profile_version)
            prepared = SimpleNamespace(**vars(opts))
            prepared.genesis = str(work / 'audit/genesis.pb')
            prepared.manifest = str(work / 'audit/manifest.pb')
            prepared.output = str(work / 'prepared')
            prepare(prepared)
            prepared.genesis = str(work / 'prepared/genesis.pb')
            prepared.manifest = str(work / 'prepared/manifest.pb')
            prepared.conformance = True
            verify_release(prepared)
            os.rename(work / 'prepared', output)
    else:
        action = json.loads((output / 'registration.json').read_text())
        g = ChainGenesis.FromString((output / 'genesis.pb').read_bytes())
        if (action.get('model_id') != opts.model_id or action.get('origin') != dict(repository=opts.origin, revision=opts.revision)
                or g.execution_profile_ver != opts.profile_version or g.numeric_audit_policy != policy):
            raise ValueError('existing output differs from requested publication; use a fresh output directory')
        check = SimpleNamespace(**vars(opts), genesis=str(output / 'genesis.pb'),
                                manifest=str(output / 'manifest.pb'), conformance=True)
        verify_release(check)
    action = json.loads((output / 'registration.json').read_text())
    graw = (output / 'genesis.pb').read_bytes()
    g = ChainGenesis.FromString(graw)
    if (action.get('type') != 'register_model' or action.get('model_id') != opts.model_id or
            action.get('genesis') != base64.b64encode(graw).decode() or
            action.get('manifest') != base64.b64encode((output / 'manifest.pb').read_bytes()).decode() or
            action.get('genesis_hash') != hashlib.sha256(graw).hexdigest() or
            action.get('profile_hash') != digest(g).hex() or hashlib.sha256(g.signer).hexdigest() != ident):
        raise ValueError('prepared action does not match local signed artifacts/publisher')
    if opts.dry_run:
        print(json.dumps(dict(result='PASS', dry_run=True, output=str(output))))
        return
    pending = output / 'submission-pending'
    if pending.exists():
        raise ValueError('previous submission outcome is unresolved; no committed publication found. Resolve the transaction before removing submission-pending; refusing automatic rebroadcast')
    account = _chain_json(opts.chain_cli, opts.chain_rpc, '/account/' + ident)
    bond = _chain_json(opts.chain_cli, opts.chain_rpc, '/publishing')['bond']
    if account['balance'] < bond:
        raise ValueError('publisher balance is below the required publication bond')
    action = json.loads((output / 'registration.json').read_text())
    pending.write_text('Submission attempted; retain until committed readback succeeds.\n')
    try:
        _chain_tx(opts, Path(opts.state_dir) / 'identity.key', account['nonce'] + 1, action)
    except (subprocess.SubprocessError, OSError) as exc:
        raise ValueError('submission failed or outcome is uncertain; rerun with the same output to read back before any retry: ' + str(exc)) from exc
    record = _published_record(opts)
    result = readback(opts, record, ident)
    pending.unlink()
    print(json.dumps(result, sort_keys=True))


def main(args):
    parser = argparse.ArgumentParser(prog='ember-node profile')
    sub = parser.add_subparsers(dest='command', required=True)
    p = sub.add_parser('publish')
    for name in ('state-dir', 'model-path', 'model-id', 'profile-version', 'origin',
                 'revision', 'output', 'policy', 'chain-rpc', 'ledger-chain-id'):
        p.add_argument('--' + name, required=True, help='explicit publisher input; policy must be measured')
    p.add_argument('--chain-cli', default='emberchain')
    p.add_argument('--previous-profile-hash')
    p.add_argument('--dry-run', action='store_true')
    p.add_argument('--device', choices=('cpu', 'cuda'), required=True)
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
    p.add_argument('--chain-cli', default='emberchain', help='matching RC chain binary for canonical identity bytes')
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
    if opts.command == 'publish':
        try:
            publish(opts)
        except Exception as exc:
            parser.exit(1, 'Publication stopped: ' + str(exc) + '\n')
        return
    if opts.command == 'verify':
        verify_release(opts)
        return
    if opts.command == 'prepare':
        prepare(opts)
        return
    _, ident = identity(opts.state_dir)
    state = {'parameters': _chain_json(opts.chain_cli, opts.chain_rpc, '/parameters'),
             'accounts': {ident: _chain_json(opts.chain_cli, opts.chain_rpc, '/account/' + ident)}}
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
