"""Block cadence and RPC retry behavior of the minimal self-proof sender."""
import threading
import unittest
from pathlib import Path
from unittest.mock import patch
from agent.join import send_liveness, liveness_due


class LivenessSenderTest(unittest.TestCase):
    def run_sender(self, heights, interval=240, parameter_failure=False, tx_failure=False):
        stop = threading.Event()
        calls = []
        heights = iter(heights)
        def query(_cli, _rpc, path):
            if path == '/state':
                try:
                    height = next(heights)
                except StopIteration:
                    stop.set()
                    raise OSError('end fixture')
                return {'height': height, 'nodes': {'self': {
                    'last_proof_height': 100, 'registered_at': 1}}}
            if path == '/parameters':
                if parameter_failure:
                    raise OSError('parameters temporarily unavailable')
                return {'liveness': {'proof_interval': interval}}
            return {'nonce': 9}
        def tx(_args, key, nonce, action):
            calls.append((key, nonce, action))
            if tx_failure and len(calls) == 1:
                raise OSError('broadcast failed')
        class Args:
            chain_cli = 'fixture'
            chain_rpc = 'fixture'
        with patch('agent.join._chain_json', side_effect=query), \
                patch('agent.join._chain_tx', side_effect=tx), \
                patch.object(stop, 'wait', return_value=False):
            send_liveness(Args(), Path('/identity'), 'self', stop)
        return calls

    def test_chain_interval_and_identity(self):
        calls = self.run_sender([339, 340], interval=240)
        self.assertEqual(calls, [(Path('/identity/identity.key'), 10,
                                 {'type': 'prove_liveness', 'node': 'self'})])
        self.assertEqual(len(self.run_sender([159, 160], interval=60)), 1)

    def test_parameter_fallback_and_broadcast_retry(self):
        self.assertEqual(len(self.run_sender([339, 340], parameter_failure=True)), 1)
        self.assertEqual(len(self.run_sender([340, 341], tx_failure=True)), 2)


class LivenessGuardianTest(unittest.TestCase):
    def test_strict_boundary(self):
        self.assertTrue(liveness_due(761, 100, 960))
        self.assertFalse(liveness_due(760, 100, 960))
        self.assertFalse(liveness_due(759, 100, 960))
        self.assertTrue(liveness_due(1200, 100, 960))
        self.assertFalse(liveness_due(101, 100, 960, 1000))

    def test_window_guard_before_normal_cadence(self):
        calls = LivenessSenderTest().run_sender([760, 761], interval=900)
        self.assertEqual(len(calls), 1)
