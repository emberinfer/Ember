"""Signed, pinned-cert node enrollment over the outbound tunnel."""
import base64
import hashlib
import json
import math
import os
from pathlib import Path
import subprocess
import time

import grpc
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

from ember_core import digest, join_challenge, node_id, sign, verify
from ember_pb2 import ChainGenesis, ModelManifest, ResourceProfile, TunnelFrame
from engine import MockEngine, load_engine, require_full_evidence
from fetcher import fetch, verify_local, fetch_sources
from pinned_tls import pinned_channel

from . import Agent
from .health import start_health_server


CHAIN_QUERY_ATTEMPTS = 4
CHAIN_QUERY_BASE_DELAY_SECONDS = 1.0


def _model_eos_token_ids(model_path):
    """Derive all locally configured EOS IDs without requiring a tokenizer."""
    if not model_path:
        return None
    root = Path(model_path).expanduser()
    ids = set()
    for name in ('generation_config.json', 'config.json'):
        try:
            value = json.loads((root / name).read_text()).get('eos_token_id')
        except (OSError, ValueError, AttributeError):
            continue
        values = value if isinstance(value, list) else [value]
        ids.update(token for token in values if type(token) is int and token >= 0)
    try:
        from transformers import AutoTokenizer
        token = AutoTokenizer.from_pretrained(
            root, local_files_only=True, trust_remote_code=False).eos_token_id
        if type(token) is int and token >= 0:
            ids.add(token)
    except Exception:
        pass
    return sorted(ids) or None


def identity(state):
    state = Path(state)
    state.mkdir(mode=0o700, parents=True, exist_ok=True)
    path = state / 'identity.key'
    if not path.exists():
        key = Ed25519PrivateKey.generate()
        raw = key.private_bytes(serialization.Encoding.Raw, serialization.PrivateFormat.Raw,
                                serialization.NoEncryption())
        fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        with os.fdopen(fd, 'wb') as writer:
            writer.write(raw)
    if path.stat().st_mode & 0o077:
        raise ValueError('identity.key must have mode 0600')
    key = Ed25519PrivateKey.from_private_bytes(path.read_bytes())
    public = key.public_key().public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)
    return key, node_id(public)


def join(args):
    if getattr(args, 'liveness_threshold', 300) <= 0 or not math.isfinite(getattr(args, 'liveness_poll', 10)) or getattr(args, 'liveness_poll', 10) <= 0:
        raise ValueError('liveness threshold and poll must be positive and finite')
    generation_timeout = getattr(args, 'generation_timeout', 300)
    if not math.isfinite(generation_timeout) or generation_timeout <= 0:
        raise ValueError('generation timeout must be finite and positive')
    key, ident = identity(args.state_dir)
    address = args.coordinator
    host = address.rsplit(':', 1)[0]
    if not args.tls_fingerprint and not (args.insecure_localhost and host in ('127.0.0.1', 'localhost', '::1')):
        raise ValueError('TLS fingerprint required (plaintext only with --insecure-localhost on loopback)')
    agent = Agent(ident, MockEngine(), key=key,
                  authorized_schedulers=getattr(args, 'scheduler_id', ()),
                  scheduler_nonce_path=Path(args.state_dir) / 'scheduler-nonces.json')
    start_health_server(agent, getattr(args, 'health_listen', ''))
    loaded_anchor = None
    def authenticate(frame):
        nonlocal loaded_anchor
        data = json.loads(frame.payload)
        genesis = ChainGenesis.FromString(base64.b64decode(data['genesis']))
        manifest = ModelManifest.FromString(base64.b64decode(data['manifest']))
        verify(genesis)
        verify(manifest)
        if genesis.chain_id != args.chain or manifest.chain_id != args.chain or digest(manifest) != genesis.model_manifest_root:
            raise ValueError('genesis/manifest chain or hash mismatch')
        from content_policy import from_manifest
        agent.content_policy = from_manifest(genesis, manifest)
        anchor = (genesis.SerializeToString(deterministic=True), manifest.SerializeToString(deterministic=True))
        if getattr(args, 'genesis_sha256', None):
            if hashlib.sha256(anchor[0]).hexdigest() != args.genesis_sha256.lower():
                raise ValueError('seed genesis hash differs from pinned release')
        if getattr(args, 'manifest_sha256', None):
            if hashlib.sha256(anchor[1]).hexdigest() != args.manifest_sha256.lower():
                raise ValueError('seed manifest hash differs from pinned release')
        if loaded_anchor is not None and anchor != loaded_anchor:
            raise ValueError('signed genesis or manifest changed during reconnect')
        if getattr(args, 'chain_rpc', None):
            from .trust import check_model_trust
            trust_state = _chain_state_with_retry(args.chain_cli, args.chain_rpc, args.chain, ident)
            if trust_state['parameters']['chain_id'] != args.ledger_chain_id:
                raise ValueError('ledger chain ID mismatch')
            check_model_trust(trust_state, args.chain, getattr(args, 'trust_policy', None))
            def refresh_trust():
                current = _chain_state_with_retry(args.chain_cli, args.chain_rpc, args.chain, ident)
                if current['parameters']['chain_id'] != args.ledger_chain_id:
                    raise ValueError('ledger chain ID mismatch')
                check_model_trust(current, args.chain, getattr(args, 'trust_policy', None))
            agent.check_trust = refresh_trust
        if args.engine == 'transformers':
            if loaded_anchor is None:
                root = Path(args.model_path).expanduser() if args.model_path else Path.home() / 'models/Qwen2.5-1.5B-Instruct'
                for artifact in manifest.artifacts:
                    try:
                        verify_local(artifact, root)
                    except FileNotFoundError:
                        fetch_sources(artifact, root, manifest.sources) if manifest.sources else fetch(artifact, root)
                agent.engine = load_engine('transformers', model_path=str(root), device=args.device,
                                           tamper=getattr(args, 'tamper', False))
        elif args.engine == 'pipeline':
            if loaded_anchor is None:
                if not manifest.shards or not args.stage or not args.scheduler_key_file:
                    raise ValueError('pipeline manifest, stages and scheduler key are required')
                root = Path(args.model_path).expanduser() if args.model_path else Path.home() / 'models/Qwen2.5-1.5B-Instruct'
                shard_artifacts = {name for shard in manifest.shards for name in shard.artifacts}
                for artifact in manifest.artifacts:
                    if artifact.name not in shard_artifacts:
                        verify_local(artifact, root)
                agent.engine = load_engine('pipeline', model_path=str(root), stages=args.stage,
                                           scheduler_key_file=args.scheduler_key_file,
                                           manifest=manifest)
        elif args.engine in ('llamacpp', 'vllm'):
            if loaded_anchor is None:
                if manifest.sources:
                    root = (Path(args.model_path).expanduser() if args.model_path else
                            Path(args.state_dir) / 'weights' / args.chain)
                    for artifact in manifest.artifacts:
                        try:
                            verify_local(artifact, root)
                        except FileNotFoundError:
                            fetch_sources(artifact, root, manifest.sources)
                agent.engine = load_engine(args.engine, endpoint=args.engine_endpoint,
                    model=args.engine_model, server_version=args.engine_version,
                    api_key=os.environ.get('EMBER_ENGINE_API_KEY'),
                    eos_token_ids=_model_eos_token_ids(getattr(args, 'model_path', None)))
        elif manifest.artifacts:
            raise ValueError('mock engine cannot claim a real-weight chain')
        if manifest.artifacts:
            caps = agent.engine.capabilities()
            if 'full' in caps.evidence_levels:
                require_full_evidence(agent.engine)
            elif not (caps.score_kind == 'logprobs' and 'topk-logprobs' in caps.evidence_levels and caps.max_top_k >= 16):
                raise ValueError('engine cannot produce full or logprob audit evidence')
        agent.engine.generation_timeout_seconds = generation_timeout
        if loaded_anchor is None:
            from .contribution import configure
            agent.contribution = configure(args, agent.engine, args.chain)
        loaded_anchor = anchor
        answers = data['questions']  # Fixed benchmark: echo questions, not an inference proof.
        observed = hashlib.sha256(b''.join(a.encode() + b'\x00' for a in answers)).digest()
        def profile():
            runtime = agent.engine.resource_metadata()
            p = ResourceProfile(node_id=ident, chain_id=args.chain, gpu=agent.engine.device,
                memory_bytes=os.sysconf('SC_PHYS_PAGES') * os.sysconf('SC_PAGE_SIZE'),
                engine_ver=args.engine + '-v1', benchmark_result_ref=observed,
                expires_unix=int(time.time()) + 120, manifest_root=genesis.model_manifest_root,
                execution_profile_ver=genesis.execution_profile_ver,
                requested_device=runtime.get('requested_device', ''),
                actual_device=runtime.get('actual_device', agent.engine.device),
                device_reason=runtime.get('device_reason', ''),
                torch_version=runtime.get('torch_version', ''),
                transformers_version=runtime.get('transformers_version', ''),
                cuda_version=runtime.get('cuda_version', ''),
                accelerator_name=runtime.get('accelerator_name', ''),
                available_memory_bytes=runtime.get('available_memory_bytes', 0),
                shard_parameters_hash=runtime.get('shard_parameters_hash', b''),
                engine_capabilities=agent.engine.capabilities().to_proto())
            p.contribution.CopyFrom(agent.contribution.signal())
            from .registration_roles import opt_outs
            availability = opt_outs(args, p.engine_capabilities)
            args.no_audit, args.no_exec = availability['no_audit'], availability['no_exec']
            if getattr(args, 'audit_role', False):
                p.engine_capabilities.roles.append('auditor')
            scheme_hash = getattr(args, 'scheme_hash', None)
            if scheme_hash:
                p.scheme_hash = bytes.fromhex(scheme_hash)
                if len(p.scheme_hash) != 32:
                    raise ValueError('scheme hash must be 32 bytes')
            from .gpu_budget import declared_budget
            budget = declared_budget(runtime, agent.engine)
            if budget is not None:
                p.gpu_budget.CopyFrom(budget)
            sign(p, key)
            return p
        agent.profile = profile
        agent.chain_args = args
        agent.genesis_bytes = anchor[0]
        args._contribution_policy = agent.contribution.policy
        signed_profile = profile()
        if getattr(args, 'chain_rpc', None):
            try:
                register_on_chain(args, state=Path(args.state_dir), ident=ident,
                                  profile=signed_profile)
            except Exception as exc:
                # A failed registration refresh must not kill the node: any
                # earlier registration stays active on-chain, and the next
                # reconnect retries the refresh.
                print(f'REGISTER retry deferred: {exc}', flush=True)
        proof = {'profile': base64.b64encode(signed_profile.SerializeToString(deterministic=True)).decode(),
                 'signature': base64.b64encode(key.sign(join_challenge(base64.b64decode(data['nonce'])))).decode(),
                 'answers': answers}
        return TunnelFrame(node_id=ident, kind='authenticate', payload=json.dumps(proof).encode())

    def channel_factory(target):
        return pinned_channel(target, args.tls_fingerprint) if args.tls_fingerprint else grpc.insecure_channel(target)

    if getattr(args, 'chain_rpc', None) and getattr(args, 'liveness_enabled', True):
        import threading
        threading.Thread(target=send_liveness, args=(args, Path(args.state_dir), ident),
                         daemon=True).start()
    agent.serve(address, channel_factory=channel_factory, authenticate=authenticate)


def _chain_json(cli, rpc, path):
    raw = subprocess.check_output([cli, 'query', '--rpc', rpc, '--path', path],
                                  text=True, timeout=30)
    return json.loads(raw)


def _chain_json_with_retry(cli, rpc, path):
    # Reuse register_on_chain's four-attempt bound and its/LIVENESS's 1s base
    # and retry log style, with exponential backoff. Unlike background liveness,
    # authentication must answer or fail, so it cannot retry forever.
    for attempt in range(CHAIN_QUERY_ATTEMPTS):
        try:
            return _chain_json(cli, rpc, path)
        except (subprocess.CalledProcessError, subprocess.TimeoutExpired,
                OSError, json.JSONDecodeError) as exc:
            # Includes FileNotFoundError: even configuration failures stay bounded
            # and retain their original exception/reporting semantics.
            if attempt == CHAIN_QUERY_ATTEMPTS - 1:
                print(f'AUTH chain query failed after {CHAIN_QUERY_ATTEMPTS} attempts: '
                      f'{path}: {exc}', flush=True)
                raise
            print(f'AUTH chain query retry {attempt + 1}/{CHAIN_QUERY_ATTEMPTS}: '
                  f'{path}: {exc}', flush=True)
            time.sleep(CHAIN_QUERY_BASE_DELAY_SECONDS * 2 ** attempt)



def _chain_height(rpc):
    # Read current height from the RPC abci_info method
    # (tiny response) instead of the full /state read.
    import urllib.request
    with urllib.request.urlopen(rpc.rstrip('/') + '/abci_info', timeout=10) as resp:
        doc = json.loads(resp.read().decode())
    return int(doc['result']['response']['last_block_height'])


def _chain_state_targeted(cli, rpc, chain_id, ident):
    # Minimal state view assembled from targeted
    # queries rather than the full state.
    # governors/endorsements stay empty and fail closed in
    # check_model_trust when the local policy does not pin a publisher.
    state = {'parameters': _chain_json(cli, rpc, '/parameters'),
             'models': {}, 'governors': {}, 'endorsements': {},
             'nodes': {}, 'height': 0}
    if chain_id:
        state['models'][chain_id] = _chain_json(cli, rpc, '/model/' + chain_id)
    if ident:
        try:
            state['nodes'][ident] = _chain_json(cli, rpc, '/node/' + ident)
        except Exception:
            pass
    try:
        state['height'] = _chain_height(rpc)
    except Exception:
        pass
    return state


def _chain_state_with_retry(cli, rpc, chain_id, ident):
    # Same four-attempt bound/backoff as _chain_json_with_retry, applied
    # to the targeted state view.
    for attempt in range(CHAIN_QUERY_ATTEMPTS):
        try:
            return _chain_state_targeted(cli, rpc, chain_id, ident)
        except (subprocess.CalledProcessError, subprocess.TimeoutExpired,
                OSError, json.JSONDecodeError) as exc:
            if attempt == CHAIN_QUERY_ATTEMPTS - 1:
                print(f'AUTH chain query failed after {CHAIN_QUERY_ATTEMPTS} attempts: '
                      f'targeted state: {exc}', flush=True)
                raise
            print(f'AUTH chain query retry {attempt + 1}/{CHAIN_QUERY_ATTEMPTS}: '
                  f'targeted state: {exc}', flush=True)
            time.sleep(CHAIN_QUERY_BASE_DELAY_SECONDS * 2 ** attempt)


def _chain_tx(args, key_path, nonce, action):
    subprocess.run([args.chain_cli, 'tx', '--rpc', args.chain_rpc,
                    '--key', str(key_path), '--chain-id', args.ledger_chain_id,
                    '--nonce', str(nonce), '--action', json.dumps(
                        action, separators=(',', ':'))],
                   check=True, timeout=60)


def register_on_chain(args, state, ident, profile):
    if not args.chain_cli or not args.ledger_chain_id:
        raise ValueError('chain registration requires --chain-cli and --ledger-chain-id')
    key_path = state / 'identity.key'
    account = _chain_json(args.chain_cli, args.chain_rpc, '/account/' + ident)
    model = _chain_json(args.chain_cli, args.chain_rpc, '/model/' + profile.chain_id)
    if (getattr(args, 'genesis_sha256', None) and
            model['genesis_hash'].lower() != args.genesis_sha256.lower()):
        raise ValueError('ledger model genesis hash differs from pinned release')
    expected_manifest_hash = (hashlib.sha256(base64.b64decode(model['publication']['manifest'])).hexdigest()
                              if model.get('publication') else model['profile_hash'].lower())
    if (getattr(args, 'manifest_sha256', None) and
            expected_manifest_hash != args.manifest_sha256.lower()):
        raise ValueError('ledger model manifest hash differs from pinned release')
    parameters = _chain_json(args.chain_cli, args.chain_rpc, '/parameters')
    minimum = int(parameters.get('min_node_stake', 1))
    nonce = int(account['nonce'])
    stake = int(account['stake'])
    if stake < minimum:
        _chain_tx(args, key_path, nonce + 1,
                  {'type': 'stake', 'amount': minimum - stake})
        nonce += 1
    chain_state = _chain_state_targeted(args.chain_cli, args.chain_rpc, profile.chain_id, ident)
    prior = chain_state.get('nodes', {}).get(ident)
    for _ in range(30):
        if not prior or int(chain_state['height']) >= int(prior['registered_at']) + 2:
            break
        time.sleep(0.5)
        chain_state = _chain_state_targeted(args.chain_cli, args.chain_rpc, profile.chain_id, ident)
    ttl = min(int(getattr(args, 'registration_ttl', 200000)),
              int(parameters.get('registration_ttl_blocks', 100)))
    if ttl < 2:
        raise ValueError('registration TTL must be at least two blocks')
    from .registration_roles import opt_outs
    action = {'type': 'register_node',
              **opt_outs(args, profile.engine_capabilities),
              'profile': base64.b64encode(profile.SerializeToString(
                  deterministic=True)).decode(),
              'expires_height': int(chain_state['height']) + ttl,
              **({'caps': {'profiles': args._contribution_policy['profiles'],
                            'global': args._contribution_policy['global']}}
                 if hasattr(args, '_contribution_policy') else {})}
    # The liveness thread signs proofs from the same account; a proof landing
    # between the nonce read above and this submission would stale the nonce.
    # Re-read the account and retry instead of failing the registration.
    for attempt in range(4):
        try:
            _chain_tx(args, key_path, nonce + 1, action)
            return
        except Exception:
            if attempt == 3:
                raise
            time.sleep(1)
            account = _chain_json(args.chain_cli, args.chain_rpc,
                                  '/account/' + ident)
            nonce = int(account['nonce'])


def liveness_due(height, last_proof, window, threshold=300, minimum=60):
    elapsed = height - last_proof
    return elapsed >= max(1, minimum) and window - elapsed < threshold


def send_liveness(args, state, ident, stop=None):
    """Block-height cadence; RPC failure retries without advancing local proof state."""
    while stop is None or not stop.is_set():
        try:
            chain = _chain_state_targeted(args.chain_cli, args.chain_rpc, None, ident)
            registration = chain.get('nodes', {}).get(ident)
            try:
                parameters = _chain_json(args.chain_cli, args.chain_rpc, '/parameters')
                policy = parameters.get('liveness', {})
                interval = int(policy.get('proof_interval', 240))
                window = int(policy.get('liveness_window', 960))
                minimum = int(policy.get('proof_min_interval', 60))
            except Exception:
                interval, window, minimum = 240, 960, 60
            height = int(chain['height'])
            last = int(registration.get('last_proof_height', registration['registered_at'])) if registration else height
            if registration and (height - last >= max(minimum, interval, 1) or
                    liveness_due(height, last, window,
                                 getattr(args, 'liveness_threshold', 300), minimum)):
                account = _chain_json(args.chain_cli, args.chain_rpc, '/account/' + ident)
                _chain_tx(args, state / 'identity.key', int(account['nonce']) + 1,
                          {'type': 'prove_liveness', 'node': ident})
        except Exception as exc:
            print(f'LIVENESS retry: {exc}', flush=True)
        if stop is not None:
            stop.wait(getattr(args, 'liveness_poll', 10))
        else:
            time.sleep(getattr(args, 'liveness_poll', 10))
