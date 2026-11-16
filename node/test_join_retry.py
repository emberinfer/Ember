"""Bounded authentication query retries, including reconnect challenges."""
import base64
import hashlib
import json
import subprocess
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import call, patch

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from ember_core import digest, sign
from ember_pb2 import ChainGenesis, ModelManifest, TunnelFrame
from engine import MockEngine
from agent.join import CHAIN_QUERY_ATTEMPTS, _chain_json_with_retry, join


class JoinRetryTest(unittest.TestCase):
    def test_transient_then_success(self):
        for failures in ([subprocess.CalledProcessError(1, 'cli'),
                          subprocess.TimeoutExpired('cli', 30)],
                         [json.JSONDecodeError('empty response', '', 0)],
                         [OSError('temporary read failure')]):
            with self.subTest(failures=failures), patch('agent.join._chain_json',
                    side_effect=[*failures, {'ok': True}]) as query, \
                    patch('agent.join.time.sleep') as sleep:
                self.assertEqual(_chain_json_with_retry('cli', 'rpc', '/state'), {'ok': True})
                self.assertEqual(query.call_count, len(failures) + 1)
                self.assertEqual(sleep.call_args_list, [call(2 ** i) for i in range(len(failures))])

    def test_persistent_failure_is_bounded(self):
        error = subprocess.CalledProcessError(1, 'cli')
        with patch('agent.join._chain_json', side_effect=error) as query, \
                patch('agent.join.time.sleep') as sleep, patch('builtins.print') as log:
            with self.assertRaises(subprocess.CalledProcessError) as raised:
                _chain_json_with_retry('cli', 'rpc', '/state')
            self.assertIs(raised.exception, error)
            self.assertEqual(query.call_count, CHAIN_QUERY_ATTEMPTS)
            self.assertEqual(sleep.call_args_list, [call(1), call(2), call(4)])
            self.assertIn('failed after 4 attempts: /state:', log.call_args.args[0])
            self.assertTrue(log.call_args.kwargs['flush'])

    def test_unexpected_exception_is_not_retried(self):
        with patch('agent.join._chain_json', side_effect=RuntimeError('unexpected')) as query, \
                patch('agent.join.time.sleep') as sleep:
            with self.assertRaises(RuntimeError):
                _chain_json_with_retry('cli', 'rpc', '/state')
            query.assert_called_once()
            sleep.assert_not_called()

    def run_join(self, serve, query_effect):
        signer = Ed25519PrivateKey.generate()
        manifest = ModelManifest(chain_id='ember-test')
        sign(manifest, signer)
        genesis = ChainGenesis(chain_id='ember-test', model_manifest_root=digest(manifest),
                               execution_profile_ver='v1')
        sign(genesis, signer)
        frame = TunnelFrame(payload=json.dumps({
            'genesis': base64.b64encode(genesis.SerializeToString()).decode(),
            'manifest': base64.b64encode(manifest.SerializeToString()).decode(),
            'nonce': base64.b64encode(b'n' * 32).decode(), 'questions': ['hello'],
        }).encode())
        def fake_serve(agent, address, channel_factory, authenticate):
            serve(agent, authenticate, frame, query)
        with tempfile.TemporaryDirectory() as directory:
            args = SimpleNamespace(state_dir=directory, coordinator='127.0.0.1:19191',
                tls_fingerprint='a' * 64, chain='ember-test', engine='transformers',
                model_path=directory, device='cpu', insecure_localhost=False,
                chain_rpc='rpc', chain_cli='cli', ledger_chain_id='ledger-test',
                genesis_sha256=hashlib.sha256(genesis.SerializeToString(deterministic=True)).hexdigest(),
                manifest_sha256=hashlib.sha256(manifest.SerializeToString(deterministic=True)).hexdigest())
            # Registration has independent queries/tx retries; isolate the trust path.
            # Suppress the background cadence thread so it cannot consume query effects.
            with patch('agent.join.Agent.serve', fake_serve), \
                    patch('agent.join.load_engine', return_value=MockEngine()) as loader, \
                    patch('agent.join.register_on_chain'), patch('threading.Thread.start'), \
                    patch('agent.join.time.sleep'), \
                    patch('agent.join._chain_state_targeted', side_effect=query_effect) as query:
                join(args)
                return loader.call_count

    @staticmethod
    def trust_state():
        # No publication/policy: exercise the real check_model_trust early return.
        return {'parameters': {'chain_id': 'ledger-test'}, 'models': {'ember-test': {}}}

    def test_initial_join_reauth_and_refresh_recover(self):
        state = self.trust_state()
        def serve(agent, authenticate, frame, query):
            self.assertEqual(authenticate(frame).kind, 'authenticate')
            self.assertEqual(query.call_count, 3)
            # Agent.serve uses this same closure for each reconnect challenge.
            self.assertEqual(authenticate(frame).kind, 'authenticate')
            self.assertEqual(query.call_count, 5)
            agent.check_trust()
            self.assertEqual(query.call_count, 7)
            self.assertEqual(query.call_args_list, [call('cli', 'rpc', 'ember-test', agent.node_id)] * 7)
        self.assertEqual(self.run_join(serve, [subprocess.CalledProcessError(1, 'cli'),
            subprocess.TimeoutExpired('cli', 30), state,
            json.JSONDecodeError('empty response', '', 0), state,
            OSError('read failed'), state]), 1)

    def test_authenticate_persistent_failure(self):
        def serve(agent, authenticate, frame, query):
            with self.assertRaises(subprocess.CalledProcessError):
                authenticate(frame)
            self.assertEqual(query.call_args_list,
                             [call('cli', 'rpc', 'ember-test', agent.node_id)] * CHAIN_QUERY_ATTEMPTS)
        self.assertEqual(self.run_join(serve, subprocess.CalledProcessError(1, 'cli')), 0)

    def test_ledger_mismatch_is_not_retried(self):
        def serve(agent, authenticate, frame, query):
            with self.assertRaisesRegex(ValueError, 'ledger chain ID mismatch'):
                authenticate(frame)
            query.assert_called_once_with('cli', 'rpc', 'ember-test', agent.node_id)
        self.run_join(serve, [{'parameters': {'chain_id': 'wrong'}}])

    def test_trust_validation_is_not_retried(self):
        def serve(agent, authenticate, frame, query):
            with self.assertRaisesRegex(ValueError, 'publication is proven invalid'):
                authenticate(frame)
            query.assert_called_once_with('cli', 'rpc', 'ember-test', agent.node_id)
        state = self.trust_state()
        state['models']['ember-test']['publication'] = {'invalid': True}
        self.run_join(serve, [state])


if __name__ == '__main__':
    unittest.main()
