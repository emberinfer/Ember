import copy
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from agent.network import node_config, main

ROOT=Path(__file__).resolve().parents[1]

class NetworkParametersTest(unittest.TestCase):
    def artifact(self):
        directory = ROOT/'chain-genesis/ember-smollm2-360m-gpu-v1'
        d = dict(schema_version=1, public_testnet='forthcoming',
                 ledger=dict(chain_id='ember-testnet'), access={},
                 current_profile=dict(id=directory.name,
                    genesis_sha256=hashlib.sha256((directory/'genesis.pb').read_bytes()).hexdigest(),
                    manifest_sha256=hashlib.sha256((directory/'manifest.pb').read_bytes()).hexdigest()))
        d['public_testnet']='active'
        d['access'].update(seed='seed.example.org:19091',rpc='https://rpc.example.org',tls_fingerprint='a'*64,scheduler_id='b'*64)
        return d

    def test_bundled_profile_binding_and_safe_config(self):
        d=self.artifact()
        output=node_config(d,"/models/a model; literal",ROOT/'chain-genesis')
        self.assertIn("EMBER_MODEL_PATH='/models/a model; literal'",output)
        self.assertIn('EMBER_REGISTRATION_TTL=200000',output)
        d['current_profile']['genesis_sha256']='0'*64
        with self.assertRaisesRegex(ValueError,'differs from bundled'):
            node_config(d,'/models/qwen',ROOT/'chain-genesis')

    def test_incomplete_network_cannot_create_config(self):
        d=self.artifact()
        d['access']['seed']=None
        with tempfile.TemporaryDirectory() as temp:
            source=Path(temp)/'network.json';source.write_text(json.dumps(d));target=Path(temp)/'node.env'
            with self.assertRaisesRegex(ValueError,'not launched'):
                main(['--network-parameters',str(source),'--model-path','/models/qwen','--config',str(target),'--profiles',str(ROOT/'chain-genesis')])
            self.assertFalse(target.exists())
        d=self.artifact();d['current_profile']['id']='../../escape'
        with self.assertRaisesRegex(ValueError,'invalid profile'):
            node_config(d,'/models/qwen',ROOT/'chain-genesis')
