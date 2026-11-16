"""Long-generation timeout configuration preserves deadline enforcement."""
import unittest
from unittest.mock import patch
from engine import MockEngine, EngineTimeout


class GenerationTimeoutTest(unittest.TestCase):
    def test_long_budget_and_default_both_enforce_their_deadline(self):
        for budget in (300, 1200):
            with self.subTest(budget=budget):
                engine = MockEngine()
                if budget != 300:
                    engine.generation_timeout_seconds = budget
                clock = [0.0]
                with patch('engine.spi.time.monotonic', side_effect=lambda: clock[0]):
                    generation = engine.generate('hello', 22, 4)
                    next(generation)
                    clock[0] = budget - 1
                    next(generation)
                    clock[0] = budget + 1
                    with self.assertRaises(EngineTimeout):
                        next(generation)
                    self.assertTrue(engine._session.closed)


if __name__ == '__main__':
    unittest.main()
