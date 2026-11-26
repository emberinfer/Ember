import copy
import hashlib
import json
from pathlib import Path
import shlex
import tempfile
import unittest
from unittest.mock import patch
from agent.network import gateway_coordinators, node_config, main

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

    def test_configure_with_public_view_entries_and_dynamic_router(self):
        router = dict(id='router', dynamic_profiles=True, node_tunnel=dict(
            endpoint='router.example:443', tls_fingerprint='a'*64))
        public = dict(url='https://public.example', relay='public.example', serving=True)
        for profile, dynamic in [('ember-qwen2.5-1.5b-unified-v1', False), ('ember-dynamic-v1', True)]:
            with self.subTest(profile=profile), tempfile.TemporaryDirectory() as temp:
                root = Path(temp)
                profiles = ROOT/'chain-genesis' if not dynamic else root/'profiles'
                document = self.artifact()
                document['access'].update(gateways=[public, router], private_gateways=[])

                def bind_profile(target, ident):
                    path = profiles/ident
                    if dynamic:
                        path.mkdir(parents=True)
                        (path/'genesis.pb').write_bytes(b'dynamic genesis')
                        (path/'manifest.pb').write_bytes(b'dynamic manifest')
                    target['current_profile'] = dict(id=ident, **{
                        name+'_sha256': hashlib.sha256((path/(name+'.pb')).read_bytes()).hexdigest()
                        for name in ('genesis', 'manifest')})

                if not dynamic:
                    bind_profile(document, profile)
                source = root/'network.json'
                source.write_text(json.dumps(document))
                target = root/'node.env'
                args = ['--network-parameters', str(source), '--model-path', '/models/qwen',
                        '--config', str(target), '--profiles', str(profiles)]
                if dynamic:
                    args += ['--profile-id', profile, '--chain-rpc', 'https://rpc.example']
                with patch('agent.network.fetch_profile', side_effect=lambda doc, ident, *_: bind_profile(doc, ident)) as fetch, \
                        patch('agent.network.verify_model_weights'), \
                        patch('sys.stderr') as stderr:
                    main(args)
                self.assertEqual(fetch.call_count, int(dynamic))
                diagnostics = ''.join(call.args[0] for call in stderr.write.call_args_list)
                self.assertEqual(diagnostics.count('https://public.example skipped: no profile membership declared'), 1)
                fields = dict(word.split('=', 1) for word in shlex.split(target.read_text()))
                self.assertEqual(fields['EMBER_MODEL_CHAIN'], profile)
                self.assertEqual(json.loads(fields['EMBER_COORDINATOR']), [router['node_tunnel']])

    def test_gateway_directory_membership_diagnostics(self):
        public = dict(url='https://public.example', relay='public.example', serving=True)
        access = dict(gateways=[public, dict(id='other', profiles=['other'])])
        with self.assertRaisesRegex(ValueError, r'checked 2 entries, skipped 1.*network\.json.*--gateway'):
            gateway_coordinators(access, 'ember-missing')
        with self.assertRaisesRegex(ValueError, 'lacks exact profile membership: broken'):
            gateway_coordinators(dict(gateways=[dict(id='broken', node_tunnel={})]), 'ember-missing')
