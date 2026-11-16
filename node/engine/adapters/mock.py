import hashlib
import random
from ..spi import EngineAdapter, EngineCapabilities, Step


class MockEngine(EngineAdapter):
    device = 'mock'
    tokenizer_version = 'mock-whitespace-v1'
    def count_input_tokens(self, prompt):
        return len(prompt.split())

    def _generate(self, prompt, seed, count):
        rng = random.Random(int.from_bytes(hashlib.sha256((prompt + str(seed)).encode()).digest(), 'big'))
        for _ in range(count):
            token = rng.randrange(97, 123)
            value = hashlib.sha256(f'{prompt}:{seed}:{token}'.encode()).digest()
            yield Step(token, chr(token), [(token, 1.0)], {'mock.layer': value})

    def replay(self, prompt, seed, tokens):
        return list(self.generate(prompt, seed, len(tokens)))

    def score_forced(self, prompt, seed, tokens):
        # The mock distribution is prefix-independent, so the scored steps
        # are exactly the steps generate() yields at the same positions.
        return list(self.generate(prompt, seed, len(tokens)))


    def capabilities(self):
        return EngineCapabilities('mock', '1', 'mock', 'synthetic', ('mock',),
                                  'raw-logits', 1, True)
