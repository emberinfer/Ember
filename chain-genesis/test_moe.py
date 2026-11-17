"""build_moe genesis tooling: the emitted manifest/genesis must be
self-consistent and match the weights on disk."""
import sys
import tempfile
import unittest
from pathlib import Path

sys.path[:0] = [str(Path(__file__).resolve().parents[1] / 'proto'),
                str(Path(__file__).resolve().parents[1] / 'core-py'),
                str(Path(__file__).resolve().parent),
                str(Path(__file__).resolve().parents[1] / 'node')]

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

from ember_core import digest, verify
from ember_core import moe as moe_core
from ember_pb2 import ChainGenesis, ModelManifest
import build_moe
from test_moe_engine import tiny_model_dir


class BuildMoeTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.model_dir, cls.cfg = tiny_model_dir()
        cls.out = Path(tempfile.mkdtemp(prefix='moe-genesis-'))
        cls.facts = build_moe.build(cls.model_dir, cls.out, 'ember-tiny-moe-test',
                                    [(0, 1), (1, 2)], key=Ed25519PrivateKey.generate())

    def test_artifacts_verify(self):
        manifest = ModelManifest.FromString((self.out / 'manifest.pb').read_bytes())
        genesis = ChainGenesis.FromString((self.out / 'genesis.pb').read_bytes())
        verify(manifest)
        verify(genesis)
        self.assertEqual(genesis.model_manifest_root, digest(manifest))
        layout = manifest.moe_layout
        self.assertEqual(layout.num_layers, 2)
        self.assertEqual(layout.experts_per_layer, 4)
        self.assertEqual(layout.top_k, 2)
        self.assertEqual(layout.expert_param_count, 3 * 64 * 32)
        self.assertEqual(layout.routing_cap_beta, 4.0)
        self.assertEqual(list(layout.routing_universal_ppm), [])
        # Segment bands: both kinds, enforce, calibrated values.
        kinds = {(b.segment_kind, b.producer_device, b.reference_device)
                 for b in genesis.numeric_audit_policy.bands if b.mode == 'enforce'}
        self.assertIn(('expert', 'cpu', 'cpu'), kinds)
        self.assertIn(('attention', 'cuda', 'cpu'), kinds)
        for band in genesis.numeric_audit_policy.bands:
            if band.segment_kind == 'expert':
                self.assertEqual(band.tolerance, 0.008)

    def test_expert_root_and_proofs(self):
        manifest = ModelManifest.FromString((self.out / 'manifest.pb').read_bytes())
        hashes = [bytes.fromhex(h) for h in self.facts['expert_params_hashes']]
        root = moe_core.expert_root(2, 4, hashes)
        self.assertEqual(root, manifest.moe_layout.expert_weights_root)
        for layer in range(2):
            for expert in range(4):
                idx = layer * 4 + expert
                proof = [bytes.fromhex(p) for p in
                         self.facts['expert_proofs'][f'{layer}:{expert}']]
                self.assertTrue(moe_core.verify_expert_proof(
                    root, 2, 4, layer, expert, hashes[idx], proof, idx))

    def test_universal_envelope_roundtrip(self):
        out = Path(tempfile.mkdtemp(prefix='moe-genesis-univ-'))
        table = [0] * 8
        table[0] = 900_000  # (layer 0, expert 0) pinned universal
        facts = build_moe.build(self.model_dir, out, 'ember-tiny-moe-univ',
                                [(0, 1), (1, 2)], universal_ppm=table,
                                key=Ed25519PrivateKey.generate())
        manifest = ModelManifest.FromString((out / 'manifest.pb').read_bytes())
        self.assertEqual(list(manifest.moe_layout.routing_universal_ppm), table)
        self.assertEqual(facts['routing_universal_ppm'], table)
        self.assertEqual(facts['layout']['routing_universal_entries'], 1)
        # Sparse CLI form resolves to the same table.
        self.assertEqual(build_moe.parse_universal_sparse('0:0:900000', 2, 4),
                         table)

    def test_universal_envelope_rejects_bad_tables(self):
        for bad in ([0] * 7, [1_000_001] + [0] * 7):
            out = Path(tempfile.mkdtemp(prefix='moe-genesis-univ-bad-'))
            with self.assertRaises(ValueError):
                build_moe.build(self.model_dir, out, 'ember-tiny-moe-bad',
                                [(0, 1), (1, 2)], universal_ppm=bad,
                                key=Ed25519PrivateKey.generate())

    def test_universal_envelope_from_moe0_counts(self):
        # The MOE-0 OLMoE counts fixture (shared with the Go acceptance
        # test): envelope derivation must reproduce the measured shares.
        counts = (Path(__file__).resolve().parents[1] / 'core-go' /
                  'testdata' / 'moe0_routing_counts.json')
        table = build_moe.universal_table_from_counts(counts, 16, 64)
        self.assertEqual(len(table), 16 * 64)
        self.assertEqual(max(table), 991_380)  # ceil(115/116 in ppm)
        build_moe.validate_universal_table(table, 16, 64, 8, 4.0)

    def test_engine_params_hashes_match(self):
        # The node engines must derive the same commitments as the genesis
        # tool, or registrations/submissions will never bind.
        from engine.moe import AttentionStage, ExpertUnit, MoeWeights
        weights = MoeWeights(self.model_dir)
        manifest = ModelManifest.FromString((self.out / 'manifest.pb').read_bytes())
        unit = ExpertUnit(weights, 1, 2)
        self.assertEqual(unit.params_hash.hex(),
                         self.facts['expert_params_hashes'][1 * 4 + 2])
        stage = AttentionStage(weights, manifest.moe_layout, 0, 1,
                               expert_call=lambda l, e, rows: rows)
        self.assertEqual(stage.params_hash.hex(),
                         self.facts['stages'][0]['params_hash'])


if __name__ == '__main__':
    unittest.main()
