import json
import pathlib
import sys
import unittest
sys.path[:0] = [str(pathlib.Path(__file__).resolve().parents[1] / "proto"), str(pathlib.Path(__file__).resolve().parent)]
import ember_pb2 as pb
from ember_core import digest, verify, verify_chunks, within_tolerance, SPARK


class CoreTest(unittest.TestCase):
    def test_compute_matches_go_contract(self):
        from ember_core import range_compute, total_compute
        manifest = pb.ModelManifest(total_layers=2)
        self.assertEqual(total_compute(manifest), 2)
        for start, end in [(0, 0), (0, 1), (0, 2), (2, 2)]:
            self.assertEqual(range_compute(manifest, start, end, True, True), max(end-start, 1))
        manifest.layer_flops.extend([1 << 33, 2 << 33])
        manifest.head_flops = 3
        self.assertEqual(range_compute(manifest, 0, 0, True), 1)
        self.assertEqual(total_compute(manifest), (3 << 33) + 4)

    def test_vectors(self):
        vectors = json.loads((pathlib.Path(__file__).resolve().parents[1] / "testdata/vectors/signed.json").read_text())
        for v in vectors:
            obj = getattr(pb, v["type"])()
            obj.ParseFromString(bytes.fromhex(v["wire"]))
            verify(obj)
            self.assertEqual(digest(obj).hex(), v["digest"])
            obj.signature = bytes(64)
            with self.assertRaises(Exception):
                verify(obj)
        chunks = [pb.ChunkRoot.FromString(bytes.fromhex(v["wire"])) for v in vectors if v["type"] == "ChunkRoot"]
        verify_chunks(chunks, "req", 20)
        chunks[1].prev_root = b"bad"
        with self.assertRaises(Exception):
            verify_chunks(chunks, "req", 20)

    def test_numeric(self):
        self.assertEqual(SPARK, 1000000)
        self.assertTrue(within_tolerance([1, 2], [1, 2.01], .01))
        self.assertFalse(within_tolerance([float("nan")], [0], 1))


if __name__ == "__main__":
    unittest.main()


class MoeMirrorTest(unittest.TestCase):
    def test_frames_and_cap(self):
        from ember_core import moe
        self.assertEqual(moe.frames(10, 3, 4), [4, 4, 2, 1, 1])
        self.assertEqual(moe.frame_offsets([4, 4, 2, 1, 1]), [0, 4, 8, 10, 11])

    def test_expert_proof_roundtrip(self):
        import hashlib
        from ember_core import moe
        params = [hashlib.sha256(b"p%d" % i).digest() for i in range(8)]
        root = moe.expert_root(2, 4, params)
        for layer in range(2):
            for expert in range(4):
                idx = layer * 4 + expert
                proof = moe.expert_proof(2, 4, params, layer, expert)
                self.assertTrue(moe.verify_expert_proof(
                    root, 2, 4, layer, expert, params[idx], proof, idx))
                self.assertFalse(moe.verify_expert_proof(
                    root, 2, 4, layer, expert, params[(idx + 1) % 8], proof, idx))

    def test_flops_handcheck(self):
        from ember_core import moe
        from ember_pb2 import MoeLayout
        layout = MoeLayout(num_layers=2, hidden_size=8, experts_per_layer=4,
                           top_k=2, expert_param_count=50, attention_param_count=100,
                           vocab_size=16, routing_cap_beta=4.0, scarcity_max=8,
                           max_frame_positions=4)
        # frame 0: size 3 offset 0, frame 1: size 1 offset 3 (per layer)
        self.assertEqual(moe.attention_flops(layout, 0, 2, 3, 0), 2160)
        self.assertEqual(moe.attention_flops(layout, 0, 2, 1, 3), 848)
        self.assertEqual(moe.head_flops_per_token(layout), 256)
        self.assertEqual(moe.routing_cap(layout, 4), 8)

    def test_expert_routing_cap_exemption(self):
        # Mirror of core-go ExpertRoutingCap: C_e = max(beta term, u_e*T),
        # integer-exact on the exemption term.
        from ember_core import moe
        from ember_pb2 import MoeLayout
        layout = MoeLayout(num_layers=16, experts_per_layer=64, top_k=8,
                           routing_cap_beta=4.0)
        layout.routing_universal_ppm.extend([0] * (16 * 64))
        layout.routing_universal_ppm[6] = 991_379  # layer 0 expert 6
        self.assertEqual(moe.routing_cap(layout, 116), 58)
        self.assertEqual(moe.expert_routing_cap(layout, 0, 6, 116), 115)
        self.assertEqual(moe.expert_routing_cap(layout, 0, 7, 116), 58)
        self.assertEqual(moe.universal_share_ppm(layout, 0, 6), 991_379)
        self.assertEqual(moe.universal_share_ppm(layout, 99, 0), 0)
