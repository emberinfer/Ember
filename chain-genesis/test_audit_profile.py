import json
import hashlib
from pathlib import Path
import sys
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "testdata"))
from profile_fixtures import profiles
FIXTURES = profiles()
import tempfile
import unittest

from build_audit_profile import build, ROOT
from ember_pb2 import ChainGenesis, ModelManifest
from ember_core import verify, digest


class AuditProfileTest(unittest.TestCase):
    def test_gpu_v3_signed_matrix_and_origin_lineage(self):
        directory = FIXTURES/'ember-qwen2.5-1.5b-gpu-v3'
        g = ChainGenesis.FromString((directory/'genesis.pb').read_bytes())
        m = ModelManifest.FromString((directory/'manifest.pb').read_bytes())
        source = ModelManifest.FromString((FIXTURES/'ember-qwen2.5-1.5b-cpu-preview-v1/manifest.pb').read_bytes())
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
        source = FIXTURES/'ember-qwen2.5-1.5b'
        before = {p.name: p.read_bytes() for p in source.iterdir() if p.is_file()}
        policy_data = json.loads((FIXTURES/'ember-qwen2.5-1.5b-union-v1/genesis.json').read_text())['genesis']['numeric_audit_policy']
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
        source = FIXTURES/'ember-qwen2.5-1.5b'
        default = FIXTURES/'ember-qwen2.5-1.5b-union-v1'
        before = {p.name: p.read_bytes() for p in default.iterdir() if p.is_file()}
        policy_data = json.loads((FIXTURES/'ember-qwen2.5-1.5b-cpu-strict-v1/genesis.json').read_text())['genesis']['numeric_audit_policy']
        with tempfile.TemporaryDirectory() as directory:
            policy = Path(directory)/'policy.json'
            policy.write_text(json.dumps(policy_data))
            out = Path(directory)/'profile'
            genesis = build(source, out, policy,
                chain_id='ember-qwen2.5-1.5b-cpu-strict-v1',
                execution_profile_ver='transformers-qwen2.5-cpu-strict-v1',
                scope='CPU strict one-sample generic CPU calibration')
            self.assertEqual(genesis.chain_id, 'ember-qwen2.5-1.5b-cpu-strict-v1')
            self.assertEqual(genesis.execution_profile_ver,
                             'transformers-qwen2.5-cpu-strict-v1')
            build(source, out, policy,
                chain_id='ember-qwen2.5-1.5b-cpu-strict-v1',
                execution_profile_ver='transformers-qwen2.5-cpu-strict-v1')
        self.assertEqual(before, {p.name: p.read_bytes() for p in default.iterdir()
                                  if p.is_file()})
