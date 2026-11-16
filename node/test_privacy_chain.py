import json
import unittest
from unittest.mock import patch
from types import SimpleNamespace
from privacy.chain import Chain
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey


class ChainClientTests(unittest.TestCase):
    def test_verified_query_output_and_chain_binding(self):
        chain=Chain('emberchain','http://localhost','ember-unit','genesis',Ed25519PrivateKey.generate(),'key')
        state=dict(parameters=dict(chain_id='ember-unit'))
        with patch('privacy.chain.subprocess.run',return_value=SimpleNamespace(stdout='state_height=3 commit=genesis-verified\n'+json.dumps(state))) as run:
            self.assertEqual(chain.state(),state)
            self.assertIn('--verify-state',run.call_args.args[0])
        with patch('privacy.chain.subprocess.run',return_value=SimpleNamespace(stdout=json.dumps(state))):
            with self.assertRaises(ValueError):chain.state()
        state['parameters']['chain_id']='wrong'
        with patch('privacy.chain.subprocess.run',return_value=SimpleNamespace(stdout='state_height=3 commit=genesis-verified\n'+json.dumps(state))):
            with self.assertRaises(ValueError):chain.state()
