"""Minimal HTTP adapter written against ember_spi and the standard library."""
import json
import urllib.request

from ember_spi import EngineAdapter, EngineCapabilities, Step


class ToyHTTPAdapter(EngineAdapter):
    def __init__(self, endpoint):
        self.endpoint = endpoint.rstrip('/')

    def capabilities(self):
        return EngineCapabilities('toy-http', '1', 'cpu', 'float32', ('full',),
                                  'raw-logits', 2, True)

    def _request(self, prompt, seed, count, tokens=None):
        payload = {'prompt': prompt, 'seed': seed, 'count': count, 'tokens': tokens}
        request = urllib.request.Request(self.endpoint + '/generate',
            json.dumps(payload).encode(), {'Content-Type': 'application/json'})
        with urllib.request.urlopen(request, timeout=5) as response:
            data = json.load(response)
        return [Step(item['token'], item['text'], [tuple(pair) for pair in item['topk']],
                     {name: bytes.fromhex(value) for name, value in item['activations'].items()})
                for item in data['steps']]

    def generate(self, prompt, seed, count):
        return iter(self._request(prompt, seed, count))

    def replay(self, prompt, seed, tokens):
        return iter(self._request(prompt, seed, len(tokens), tokens))


def create_adapter(endpoint):
    return ToyHTTPAdapter(endpoint)
