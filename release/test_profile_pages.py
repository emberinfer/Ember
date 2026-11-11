import base64
import copy
import hashlib
import unittest
from pathlib import Path
import sys
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "testdata"))
from profile_fixtures import profiles
FIXTURES = profiles()

sys.path.insert(0, str(Path(__file__).resolve().parent))
from profile_pages import records
from ember_core import digest
from ember_pb2 import ChainGenesis, ModelManifest

ROOT = Path(__file__).resolve().parents[1]
IDENT = 'ember-qwen2.5-1.5b-publisher-cpu-v1'


class ProfilePages(unittest.TestCase):
    def setUp(self):
        source = FIXTURES / IDENT
        graw, mraw = [(source / name).read_bytes() for name in ('genesis.pb', 'manifest.pb')]
        g, m = ChainGenesis.FromString(graw), ModelManifest.FromString(mraw)
        from ember_core import node_id
        self.state = dict(parameters=dict(chain_id='release-test'), height=1000,
            models={IDENT: dict(id=IDENT, genesis_hash=hashlib.sha256(graw).hexdigest(),
                profile_hash=digest(g).hex(), publication=dict(
                    publisher=node_id(g.signer), registered_height=537,
                    manifest_root=digest(m).hex(), genesis=base64.b64encode(graw).decode(),
                    manifest=base64.b64encode(mraw).decode(),
                    origin=dict(repository='https://huggingface.co/Qwen/Qwen2.5-1.5B-Instruct',
                                revision='989aa7980e4cf806f80c7fef2b1adb7bc71aa306')))},
            endorsements={digest(m).hex(): {'alice': dict(account='alice', height=900),
                                           'bob': dict(account='bob', height=901, revoked=True)}})

    def test_signed_release_and_current_endorsements(self):
        r = records(self.state, FIXTURES, 'https://rpc.example.org')[IDENT]
        self.assertEqual(r['registration_height'], 537)
        self.assertEqual([e['account'] for e in r['endorsements']], ['alice'])
        self.assertEqual(len(r['version_history']), 1)
        self.assertIn('profile verify', r['conformance_commands'][0])

    def test_unselected_permissionless_submission_does_not_break_catalog(self):
        extra = copy.deepcopy(self.state["models"][IDENT])
        extra["id"] = "ember-unselected-profile"
        self.state["models"][extra["id"]] = extra
        self.assertEqual(list(records(self.state, FIXTURES, "https://rpc.example.org")), [IDENT])

    def test_artifact_chain_disagreement_fails(self):
        for field in ('genesis_hash', 'profile_hash'):
            state = copy.deepcopy(self.state)
            state['models'][IDENT][field] = '0' * 64
            with self.assertRaisesRegex(ValueError, 'disagree'):
                records(state, FIXTURES, 'https://rpc.example.org')
        state = copy.deepcopy(self.state)
        state['models'][IDENT]['publication']['manifest_root'] = '0' * 64
        with self.assertRaisesRegex(ValueError, 'disagree'):
            records(state, FIXTURES, 'https://rpc.example.org')

    def test_broken_history_fails(self):
        self.state['models'][IDENT]['publication']['previous_profile_hash'] = '0' * 64
        with self.assertRaisesRegex(ValueError, 'missing previous'):
            records(self.state, FIXTURES, 'https://rpc.example.org')


if __name__ == '__main__':
    unittest.main()
