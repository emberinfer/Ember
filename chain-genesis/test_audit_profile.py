import json
import hashlib
from pathlib import Path
import tempfile
import unittest

import importlib.util
spec = importlib.util.spec_from_file_location('build_audit_profile', Path(__file__).with_name('build_audit_profile.py'))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
build, ROOT = module.build, module.ROOT
from ember_pb2 import ChainGenesis, ModelManifest
from ember_core import verify, digest


class AuditProfileTest(unittest.TestCase):
    def test_gpu_v3_signed_matrix_and_origin_lineage(self):
        directory = ROOT/'chain-genesis/ember-qwen2.5-1.5b-gpu-v3'
        g = ChainGenesis.FromString((directory/'genesis.pb').read_bytes())
        m = ModelManifest.FromString((directory/'manifest.pb').read_bytes())
        source = ModelManifest.FromString((ROOT/'chain-genesis/ember-qwen2.5-1.5b-s21-prep-v1/manifest.pb').read_bytes())
        verify(g); verify(m)
        self.assertEqual(g.model_manifest_root, digest(m))
        self.assertEqual(list(m.artifacts), list(source.artifacts))
        self.assertEqual(g.numeric_audit_policy.metric, 'rank-probability-v1')
        self.assertEqual(g.numeric_audit_policy.calibration_ref,
                         hashlib.sha256((directory/'calibration.json').read_bytes()).digest())
        pairs = set()
        for band in g.numeric_audit_policy.bands:
            producer = band.producer_engine.rsplit('gpu=', 1)[1]
            reference = band.reference_engine.rsplit('gpu=', 1)[1]
            pairs.add((producer, reference))
            self.assertEqual((band.producer_device, band.reference_device, band.mode), ('cuda', 'cuda', 'enforce'))
            self.assertEqual(band.tolerance, .001 if producer == reference else .5)
        self.assertEqual(pairs, {(a, b) for a in ['gb10', 'thor', 'orin'] for b in ['gb10', 'thor', 'orin']})

    def test_new_profile_is_immutable_and_source_is_unchanged(self):
        source = ROOT/'chain-genesis/ember-qwen2.5-1.5b'
        before = {p.name: p.read_bytes() for p in source.iterdir() if p.is_file()}
        policy_data = json.loads((ROOT/'chain-genesis/ember-qwen2.5-1.5b-union-v1/genesis.json').read_text())['genesis']['numeric_audit_policy']
        with tempfile.TemporaryDirectory() as directory:
            policy = Path(directory)/'policy.json'
            policy.write_text(json.dumps(policy_data))
            out = Path(directory)/'profile'
            genesis = build(source, out, policy)
            self.assertEqual(genesis.chain_id, 'ember-qwen2.5-1.5b-union-v1')
            first = (out/'genesis.pb').read_bytes()
            build(source, out, policy)
            self.assertEqual(first, (out/'genesis.pb').read_bytes())
            changed = json.loads(policy.read_text()); changed['bands'][0]['tolerance'] = .002
            path = Path(directory)/'changed.json'; path.write_text(json.dumps(changed))
            with self.assertRaisesRegex(ValueError, 'immutable'):
                build(source, out, path)
        self.assertEqual(before, {p.name: p.read_bytes() for p in source.iterdir() if p.is_file()})

    def test_custom_profile_builds_without_changing_default_profile(self):
        source = ROOT/'chain-genesis/ember-qwen2.5-1.5b'
        default = ROOT/'chain-genesis/ember-qwen2.5-1.5b-union-v1'
        before = {p.name: p.read_bytes() for p in default.iterdir() if p.is_file()}
        policy_data = json.loads((ROOT/'chain-genesis/ember-qwen2.5-1.5b-s19-x86-v1/genesis.json').read_text())['genesis']['numeric_audit_policy']
        with tempfile.TemporaryDirectory() as directory:
            policy = Path(directory)/'policy.json'
            policy.write_text(json.dumps(policy_data))
            out = Path(directory)/'profile'
            genesis = build(source, out, policy,
                chain_id='ember-qwen2.5-1.5b-s19-x86-v1',
                execution_profile_ver='transformers-qwen2.5-s19-x86-v1',
                scope='S19 one-sample cloud CPU calibration')
            self.assertEqual(genesis.chain_id, 'ember-qwen2.5-1.5b-s19-x86-v1')
            self.assertEqual(genesis.execution_profile_ver,
                             'transformers-qwen2.5-s19-x86-v1')
            build(source, out, policy,
                chain_id='ember-qwen2.5-1.5b-s19-x86-v1',
                execution_profile_ver='transformers-qwen2.5-s19-x86-v1')
        self.assertEqual(before, {p.name: p.read_bytes() for p in default.iterdir()
                                  if p.is_file()})

    def test_restored_signed_fixtures_match_published_bytes(self):
        # Origin commits are recorded for review; shallow checkouts only need
        # the committed SHA-256 values and self-verifying signed artifacts.
        entries = json.loads((ROOT/'chain-genesis/profile-fixtures.json').read_text())
        directories = set()
        for entry in entries:
            path = ROOT/entry['path']
            self.assertEqual(hashlib.sha256(path.read_bytes()).hexdigest(),entry['sha256'])
            directories.add(path.parent)
        for directory in directories:
            g=ChainGenesis.FromString((directory/'genesis.pb').read_bytes())
            m=ModelManifest.FromString((directory/'manifest.pb').read_bytes())
            verify(g);verify(m)
            self.assertEqual(g.model_manifest_root,digest(m))
