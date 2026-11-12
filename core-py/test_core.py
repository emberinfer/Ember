import json
import pathlib
import sys
import unittest
sys.path[:0] = [str(pathlib.Path(__file__).resolve().parents[1] / "proto"), str(pathlib.Path(__file__).resolve().parent)]
import ember_pb2 as pb
from ember_core import digest, verify, verify_chunks, within_tolerance, SPARK


class CoreTest(unittest.TestCase):
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
