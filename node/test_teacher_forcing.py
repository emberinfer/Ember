import unittest
from types import SimpleNamespace as NS

from auditor.equality import compare, compare_teacher_forced
from engine import Step
from engine.spi import EngineAdapter, UnsupportedCapability

EOS = 77


def dist(argmax, extra=None, step=0.4):
    """Top-16 score list: argmax at 0.0, extra {token: score}, filler below."""
    scores = {argmax: 0.0}
    if extra:
        scores.update(extra)
    filler = 900
    value = -max(1.0, max(-v for v in scores.values()) + 1.0)
    while len(scores) < 16:
        if filler not in scores:
            scores[filler] = value
            value -= step
        filler += 1
    return sorted(scores.items(), key=lambda kv: -kv[1])[:16]


class ScriptedEngine:
    """Distributions keyed by the output prefix they condition on.

    generate() follows its own argmax path; score_forced() follows the
    committed prefix. Both stop after the step whose token is EOS,
    mirroring the real adapters.
    """

    def __init__(self, table):
        self.table = table

    def count_input_tokens(self, prompt):
        return 2

    def _row(self, prefix):
        return self.table.get(tuple(prefix), dist(42))

    def generate(self, prompt, seed, count):
        prefix, steps = [], []
        for _ in range(count):
            row = self._row(prefix)
            token = row[0][0]
            steps.append(Step(token, '', row, {}))
            prefix.append(token)
            if token == EOS:
                break
        return iter(steps)

    def score_forced(self, prompt, seed, tokens):
        prefix, steps = [], []
        for token in tokens:
            row = self._row(prefix)
            steps.append(Step(row[0][0], '', row, {}))
            prefix.append(token)
            if row[0][0] == EOS:
                break
        return steps


class TeacherForcingTest(unittest.TestCase):
    def run_both(self, table, committed, epsilon=1.0, output_tokens=None):
        engine = ScriptedEngine(table)
        receipt = NS(input_tokens=2, output_tokens=len(committed) if output_tokens is None else output_tokens)
        openings = [{'token': t} for t in committed]
        regen = compare(engine, 'prompt', 0, receipt, openings, epsilon)
        forced = compare_teacher_forced(engine, 'prompt', 0, receipt, openings, epsilon)
        self.assertEqual(regen[0], forced[0])
        self.assertEqual(regen[1], forced[1])
        self.assertEqual(regen[3], forced[3])
        self.assertEqual(getattr(regen[2], 'class', None), getattr(forced[2], 'class', None))
        return regen

    def test_honest_accept(self):
        table = {(): dist(10), (10,): dist(20), (10, 20): dist(30)}
        r = self.run_both(table, [10, 20, 30])
        self.assertEqual((r[0], r[1], r[3]), ('accept', 0, 3))

    def test_near_tie_absorbed(self):
        table = {(): dist(10), (10,): dist(20, {21: -0.5})}
        r = self.run_both(table, [10, 21])
        self.assertEqual((r[0], r[1]), ('accept', 0.5))

    def test_far_divergence_rejects_with_same_margin(self):
        table = {(): dist(10), (10,): dist(20, {21: -3.0})}
        r = self.run_both(table, [10, 21])
        self.assertEqual((r[0], r[1]), ('reject', 3.0))
        self.assertEqual(r[2].band, 'token-equality-v1')

    def test_absent_token_far_rejects(self):
        row = [(20 + i, -0.4 * i) for i in range(16)]
        r = self.run_both({(): dist(10), (10,): row}, [10, 99])
        self.assertEqual((r[0], r[1]), ('reject', 6.0))

    def test_absent_token_near_inconclusive(self):
        row = [(20 + i, -0.005 * i) for i in range(16)]
        r = self.run_both({(): dist(10), (10,): row}, [10, 99])
        self.assertEqual(r[0], 'inconclusive')

    def test_eos_truncation_inconclusive(self):
        table = {(): dist(10), (10,): dist(EOS)}
        r = self.run_both(table, [10, EOS, 55])
        self.assertEqual((r[0], r[3]), ('inconclusive', 2))

    def test_first_divergence_decides_alone(self):
        # Position 0 absorbs (margin 0.5); the huge later divergence under
        # either prefix must never be reached by the rule.
        table = {(): dist(10, {11: -0.5}), (10,): dist(20, {999: -50.0}),
                 (11,): dist(20, {999: -50.0})}
        r = self.run_both(table, [11, 999])
        self.assertEqual((r[0], r[1]), ('accept', 0.5))

    def test_opening_count_precheck(self):
        r = self.run_both({(): dist(10)}, [10], output_tokens=2)
        self.assertEqual(r[0], 'reject')
        self.assertEqual(r[2].fault, 'format')

    def test_unsupported_engine_propagates(self):
        class Bare(EngineAdapter):
            def count_input_tokens(self, prompt):
                return 2

        receipt = NS(input_tokens=2, output_tokens=1)
        with self.assertRaises(UnsupportedCapability):
            compare_teacher_forced(Bare(), 'prompt', 0, receipt, [{'token': 1}], 1.0)


if __name__ == '__main__':
    unittest.main()
