import copy
import json
from pathlib import Path
import tempfile
import unittest
from agent.network import node_config, main

ROOT=Path(__file__).resolve().parents[1]

class NetworkParametersTest(unittest.TestCase):
    def artifact(self):
        d=json.loads((ROOT/'site/network.json').read_text())
        d['public_testnet']='active'
        d['access'].update(seed='seed.example:19091',rpc='https://rpc.example',tls_fingerprint='a'*64,scheduler_id='b'*64)
        return d

    def test_bundled_profile_binding_and_safe_config(self):
        d=self.artifact()
        # The current profile's artifacts arrive via the deployment
        # channel, not chain-genesis; bind against a bundled profile.
        d['current_profile'].update(
            id='ember-qwen2.5-1.5b-gpu-v1',
            genesis_sha256='188f1cb4bbff0f90fa3730a1d49fb5df0e235c2a9202cb23512ba3b71db0254c',
            manifest_sha256='4b820e746e59faf067502460594b34de9b64e3f405506cdd253bee52192f137e')
        d['current_profile'].pop('scheme_hash', None)
        output=node_config(d,"/models/a model; literal",ROOT/'chain-genesis')
        self.assertIn("EMBER_MODEL_PATH='/models/a model; literal'",output)
        self.assertIn('EMBER_REGISTRATION_TTL=200000',output)
        d['current_profile']['genesis_sha256']='0'*64
        with self.assertRaisesRegex(ValueError,'differs from bundled'):
            node_config(d,'/models/qwen',ROOT/'chain-genesis')

    def test_incomplete_network_cannot_create_config(self):
        d=json.loads((ROOT/'site/network.json').read_text())
        d['access']['seed']=None
        with tempfile.TemporaryDirectory() as temp:
            source=Path(temp)/'network.json';source.write_text(json.dumps(d));target=Path(temp)/'node.env'
            with self.assertRaisesRegex(ValueError,'not launched'):
                main(['--network-parameters',str(source),'--model-path','/models/qwen','--config',str(target),'--profiles',str(ROOT/'chain-genesis')])
            self.assertFalse(target.exists())
        d=self.artifact();d['current_profile']['id']='../../escape'
        with self.assertRaisesRegex(ValueError,'invalid profile'):
            node_config(d,'/models/qwen',ROOT/'chain-genesis')
