import hashlib
import unittest
from reasoning import sampling_hash, Budget, prepare
class ReasoningTests(unittest.TestCase):
    def test_hash_version_and_mode(self):
        self.assertEqual(sampling_hash(dict(max_tokens=8)),hashlib.sha256(b'8').digest())
        on=dict(max_tokens=8,thinking_enabled=True,thinking_budget=3,answer_budget=5)
        off=dict(max_tokens=8,thinking_enabled=False,answer_budget=5)
        self.assertNotEqual(sampling_hash(on),sampling_hash(off))
        self.assertEqual(sampling_hash(on),sampling_hash(dict(on)))
    def test_independent_budgets(self):
        b=Budget(dict(max_tokens=8,thinking_enabled=True,thinking_budget=2,answer_budget=5))
        b.add('thought');b.add('</think>');b.add('answer')
        self.assertEqual((b.reasoning,b.content),(2,1))
        b=Budget(dict(max_tokens=8,thinking_enabled=True,thinking_budget=1,answer_budget=7))
        b.add('thought')
        with self.assertRaisesRegex(ValueError,'thinking budget exhausted'):b.add('more')
    def test_off_no_reasoning(self):
        b=Budget(dict(max_tokens=8,thinking_enabled=False,answer_budget=8))
        b.add('<thi')
        with self.assertRaisesRegex(ValueError,'disabled'):b.add('nk>')
    def test_template_mapping(self):
        class Engine:
            def thinking_prompt(self,prompt,enabled):return (prompt,enabled)
        self.assertEqual(prepare(Engine(),'hi',dict(max_tokens=8,thinking_enabled=False,answer_budget=8)),('hi',False))
