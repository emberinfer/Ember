import unittest
from ember_pb2 import ModelManifest, LayoutScheme
from layout_identity import scheme_hash,select_scheme
class SchemeTests(unittest.TestCase):
    def test_projection_and_isolation(self):
        one=LayoutScheme(total_layers=2,min_stage_layers=1,layer_flops=[1,1],embedding_flops=1,head_flops=1)
        two=LayoutScheme();two.CopyFrom(one);two.layer_flops[:]=[2,2]
        m=ModelManifest(chain_id='one',schemes=[one,two])
        self.assertNotEqual(scheme_hash(one),scheme_hash(two))
        selected=select_scheme(m,scheme_hash(two))
        self.assertTrue(selected.dynamic_ranges)
        self.assertEqual(list(selected.layer_flops),[2,2])
        self.assertEqual(len(m.schemes),2)
        with self.assertRaises(ValueError):select_scheme(m,b'bad')
