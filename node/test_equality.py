import unittest
from types import SimpleNamespace as NS
from auditor.equality import compare
from engine import Step

class EqualityTest(unittest.TestCase):
    def run_case(self, token, scores, expected, epsilon=1, input_count=2):
        engine = NS(count_input_tokens=lambda p: input_count,
                    generate=lambda p,s,n: iter([Step(0, '', scores, {})]))
        result = compare(engine, 'prompt', 0, NS(input_tokens=2, output_tokens=1), [{'token':token}], epsilon)
        self.assertEqual(result[0], expected)
        return result
    def test_equal(self):
        self.assertEqual(self.run_case(0, [], 'accept')[1], 0)
    def test_noise(self):
        self.run_case(1, [(i,-i*.1) for i in range(16)], 'accept')
    def test_far(self):
        r=self.run_case(1, [(i,-i*2) for i in range(16)], 'reject')
        self.assertEqual(getattr(r[2],'class'),'token')
        self.assertEqual(r[2].band,'token-equality-v1')
    def test_bound_far(self):
        self.run_case(100, [(i,-i) for i in range(16)], 'reject')
    def test_bound_near(self):
        self.run_case(100, [(i,-i*.01) for i in range(16)], 'inconclusive')
    def test_input_count(self):
        r=self.run_case(0, [], 'reject',input_count=3)
        self.assertEqual(r[2].fault,'format')
    def test_chain_epsilon(self):
        self.run_case(1, [(i,-i*2) for i in range(16)], 'accept',epsilon=3)
