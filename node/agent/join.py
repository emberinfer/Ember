"""Signed, pinned-cert node enrollment over the outbound tunnel."""
import base64
import hashlib
import json
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
from fetcher import fetch, verify_local
from pinned_tls import pinned_channel

from . import Agent
from .health import start_health_server


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
            trust_state = _chain_json(args.chain_cli, args.chain_rpc, '/state')
            if trust_state['parameters']['chain_id'] != args.ledger_chain_id:
                raise ValueError('ledger chain ID mismatch')
            check_model_trust(trust_state, args.chain, getattr(args, 'trust_policy', None))
            def refresh_trust():
                current = _chain_json(args.chain_cli, args.chain_rpc, '/state')
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
                        fetch(artifact, root)
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
                agent.engine = load_engine(args.engine, endpoint=args.engine_endpoint,
                    model=args.engine_model, server_version=args.engine_version,
                    api_key=os.environ.get('EMBER_ENGINE_API_KEY'))
        elif manifest.artifacts:
            raise ValueError('mock engine cannot claim a real-weight chain')
        if manifest.artifacts:
            require_full_evidence(agent.engine)
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
            if getattr(args, 'audit_role', False):
                p.engine_capabilities.roles.append('auditor')
            sign(p, key)
            return p
        agent.profile = profile
        agent.chain_args = args
        agent.genesis_bytes = anchor[0]
        signed_profile = profile()
        if getattr(args, 'chain_rpc', None):
            register_on_chain(args, state=Path(args.state_dir), ident=ident,
                              profile=signed_profile)
        proof = {'profile': base64.b64encode(signed_profile.SerializeToString(deterministic=True)).decode(),
                 'signature': base64.b64encode(key.sign(join_challenge(base64.b64decode(data['nonce'])))).decode(),
                 'answers': answers}
        return TunnelFrame(node_id=ident, kind='authenticate', payload=json.dumps(proof).encode())

    def channel_factory(target):
        return pinned_channel(target, args.tls_fingerprint) if args.tls_fingerprint else grpc.insecure_channel(target)

    agent.serve(address, channel_factory=channel_factory, authenticate=authenticate)


def _chain_json(cli, rpc, path):
    raw = subprocess.check_output([cli, 'query', '--rpc', rpc, '--path', path],
                                  text=True, timeout=30)
    return json.loads(raw)


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
    chain_state = _chain_json(args.chain_cli, args.chain_rpc, '/state')
    prior = chain_state.get('nodes', {}).get(ident)
    for _ in range(30):
        if not prior or int(chain_state['height']) >= int(prior['registered_at']) + 2:
            break
        time.sleep(0.5)
        chain_state = _chain_json(args.chain_cli, args.chain_rpc, '/state')
    ttl = min(int(getattr(args, 'registration_ttl', 200000)),
              int(parameters.get('registration_ttl_blocks', 100)))
    if ttl < 2:
        raise ValueError('registration TTL must be at least two blocks')
    _chain_tx(args, key_path, nonce + 1, {'type': 'register_node',
        'profile': base64.b64encode(profile.SerializeToString(
            deterministic=True)).decode(),
        'expires_height': int(chain_state['height']) + ttl})
