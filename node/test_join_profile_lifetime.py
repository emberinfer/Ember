"""Exercise signed authentication profiles across delayed gateway sessions."""
import base64
import copy
import json
import threading
from unittest.mock import patch

from agent import Agent
from agent.__main__ import main
from agent.join import _join_single, identity
from agent.multihome import Claims, Registration, Sessions
from ember_core import digest, sign, verify
from ember_pb2 import ChainGenesis, ModelManifest, ResourceProfile, TunnelFrame


def test_profile_lifetime_and_delayed_session_freshness(tmp_path):
    with patch('sys.argv', ['agent', 'join', '--chain', 'isolated-model',
            '--engine', 'mock', '--state-dir', str(tmp_path),
            '--coordinator', '127.0.0.1:1', '--insecure-localhost',
            '--chain-rpc', 'memory', '--ledger-chain-id', 'ledger',
            '--no-exec', '--no-audit', '--no-liveness-enabled', '--health-listen', '']), \
            patch('agent.join.join') as capture:
        main()
    args = capture.call_args.args[0]
    key, _ = identity(tmp_path)
    manifest = ModelManifest(chain_id=args.chain)
    sign(manifest, key)
    genesis = ChainGenesis(chain_id=args.chain, model_manifest_root=digest(manifest),
                           execution_profile_ver='v1')
    sign(genesis, key)
    frame = TunnelFrame(payload=json.dumps(dict(
        genesis=base64.b64encode(genesis.SerializeToString()).decode(),
        manifest=base64.b64encode(manifest.SerializeToString()).decode(),
        questions=['one', 'two'], nonce=base64.b64encode(b'nonce').decode())).encode())
    shared = dict(claims=Claims(), registration=Registration(), initialization=Registration(),
        runtime={}, sessions=Sessions(['127.0.0.1:1']), nonce_lock=threading.Lock(),
        execution_lock=threading.Lock(), renewal=Registration(), nonces=None)
    now = [1000]
    proofs, registrations = [], []
    def register(options, state, node, profile):
        assert options.no_exec and options.no_audit
        registrations.append(profile)
        return profile
    def serve(self, address, **kwargs):
        proof = kwargs['authenticate'](frame)
        profile = ResourceProfile.FromString(base64.b64decode(json.loads(proof.payload)['profile']))
        verify(profile)
        proofs.append(profile)
    with patch.object(Agent, 'serve', serve), \
            patch('agent.join.time.time', side_effect=lambda: now[0]), \
            patch('agent.join._chain_state_with_retry', return_value=dict(
                parameters=dict(chain_id='ledger'), models={args.chain: {}})), \
            patch('agent.join.register_profile', side_effect=register), \
            patch('agent.join.start_registration_renewal'):
        _join_single(copy.copy(args), shared)
        assert proofs[0].expires_unix == 1240
        now[0] = 1125
        _join_single(copy.copy(args), shared)
        assert proofs[0] == proofs[1] and proofs[1].expires_unix > now[0]
        assert len(registrations) == 1
        now[0] = 1211  # Existing shared freshness guard must refresh at <30s.
        _join_single(copy.copy(args), shared)
        assert proofs[2].expires_unix == 1451 and len(registrations) == 2
    before, after = ResourceProfile(), ResourceProfile()
    before.CopyFrom(proofs[0]); after.CopyFrom(proofs[2])
    for profile in (before, after):
        profile.ClearField('expires_unix'); profile.ClearField('signature')
    assert before == after
