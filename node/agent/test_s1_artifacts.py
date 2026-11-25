import base64
import copy
import hashlib
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from ember_core import sign, digest, node_id
from ember_pb2 import ChainGenesis, ModelManifest, TunnelFrame
from agent.network import validate_artifacts, fetch_profile, gateway_coordinators, main
from agent.join import ProfileAssertion


def fixture():
    key = Ed25519PrivateKey.generate()
    m = ModelManifest(chain_id='ember-unbundled')
    sign(m, key)
    g = ChainGenesis(chain_id=m.chain_id, model_manifest_root=digest(m), execution_profile_ver='test')
    sign(g, key)
    gr, mr = g.SerializeToString(), m.SerializeToString()
    return dict(id=g.chain_id, genesis_hash=hashlib.sha256(gr).hexdigest(), profile_hash=digest(g).hex(),
                publication=dict(genesis=base64.b64encode(gr).decode(), manifest=base64.b64encode(mr).decode(),
                                 publisher=node_id(g.signer), manifest_root=digest(m).hex()))


class ArtifactsTest(unittest.TestCase):
    def test_validation(self):
        model = fixture()
        validate_artifacts(model, model['id'])
        for field in ('publisher', 'manifest_root', 'genesis', 'manifest'):
            bad = copy.deepcopy(model)
            bad['publication'][field] = 'bad'
            with self.assertRaises(Exception):
                validate_artifacts(bad, bad['id'])
        for field in ('id', 'genesis_hash', 'profile_hash'):
            bad = dict(model, **{field: 'bad'})
            with self.assertRaises(ValueError):
                validate_artifacts(bad, model['id'])
        bad = copy.deepcopy(model)
        bad['publication']['invalid'] = True
        with self.assertRaises(ValueError):
            validate_artifacts(bad, bad['id'])
        legacy = dict(model)
        p = legacy.pop('publication')
        legacy.update(signed_profile=p['genesis'], signed_manifest=p['manifest'],
                      profile_hash=hashlib.sha256(base64.b64decode(p['manifest'])).hexdigest())
        validate_artifacts(legacy, legacy['id'])

    def test_configure_fetch_trust_and_exact_bytes(self):
        model = fixture()
        state = dict(parameters=dict(chain_id='local-ledger'), models={model['id']: model}, governors={})
        document = dict(ledger=dict(chain_id='local-ledger'), access={})
        with tempfile.TemporaryDirectory() as tmp:
            policy = Path(tmp) / 'trust.json'
            policy.write_text(json.dumps(dict(profiles={model['id']: dict(publishers=[model['publication']['publisher']])})))
            with patch('agent.join._chain_json', side_effect=lambda cli, rpc, path: state if path.startswith('/model-trust/') else state['parameters'] if path == '/parameters' else model):
                with self.assertRaises(ValueError):
                    fetch_profile(document, model['id'], tmp, None, 'unused', policy)
                with self.assertRaises(ValueError):
                    fetch_profile(document, model['id'], tmp, 'http://operator-pin', 'unused')
                pins = fetch_profile(document, model['id'], tmp, 'http://operator-pin', 'unused', policy)
                raw = (Path(tmp) / model['id'] / 'genesis.pb').read_bytes()
                self.assertEqual(raw, base64.b64decode(model['publication']['genesis']))
                self.assertEqual(hashlib.sha256(raw).hexdigest(), pins['genesis_sha256'])

    def test_configure_cli_and_explicit_gateway(self):
        model = fixture()
        state = dict(models={model['id']: model}, governors={})
        parameters = dict(chain_id='local-ledger')
        root = Path(__file__).resolve().parents[2]
        document = json.loads((root / 'site/network.json').read_text())
        document['public_testnet'] = 'active'
        document['ledger']['chain_id'] = parameters['chain_id']
        document['access'].update(seed='localhost:1234', rpc='http://old',
            tls_fingerprint='a'*64, scheduler_id='b'*64,
            gateways=[dict(id='explicit', profiles=[], node_tunnel=dict(
                endpoint='localhost:4321', tls_fingerprint='c'*64))], private_gateways=[])
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            network, policy, config = (tmp / name for name in ('network.json', 'trust.json', 'node.env'))
            network.write_text(json.dumps(document))
            policy.write_text(json.dumps(dict(profiles={model['id']: dict(publishers=[model['publication']['publisher']])})))
            def query(cli, rpc, path):
                return parameters if path == '/parameters' else state if path.startswith('/model-trust/') else model
            with patch('agent.join._chain_json', side_effect=query):
                main(['--network-parameters', str(network), '--profile-id', model['id'],
                      '--chain-rpc', 'http://operator-pin', '--trust-policy', str(policy),
                      '--profiles', str(tmp / 'profiles'), '--config', str(config),
                      '--model-path', str(tmp), '--gateway', 'explicit'])
            content = config.read_text()
            self.assertIn('localhost:4321', content)
            self.assertIn('EMBER_CHAIN_RPC=http://operator-pin', content)
            self.assertIn('EMBER_MODEL_CHAIN=' + model['id'], content)
            self.assertIn('EMBER_GENESIS_SHA256=' + model['genesis_hash'], content)

    def test_first_frame(self):
        assertion = ProfileAssertion('ember-unbundled', 'a' * 64)
        frames = [TunnelFrame(kind='join'), TunnelFrame(kind='authenticate')]
        result = assertion.intercept_stream_stream(lambda d, r: list(r), None, iter(frames))
        self.assertEqual(json.loads(result[0].payload)['profile_id'], 'ember-unbundled')
        self.assertEqual(result[1].payload, b'')

    def test_dynamic_directory(self):
        access = dict(gateways=[dict(id='dynamic', profiles=[], dynamic_profiles=True,
                      node_tunnel=dict(endpoint='localhost:1234', tls_fingerprint='a' * 64))])
        self.assertEqual(len(gateway_coordinators(access, 'ember-unbundled')), 1)
        access['gateways'][0]['dynamic_profiles'] = False
        with self.assertRaises(ValueError):
            gateway_coordinators(access, 'ember-unbundled')
