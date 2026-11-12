"""Strict buffered HTTP adapters. These services expose logprobs, not raw logits.

No server is spawned. Neither adapter claims activations or forced-prefix replay.
"""
import json
import math
import socket
import time
import urllib.error
import urllib.parse
import urllib.request

from ..spi import (EngineAdapter, EngineCapabilities, Step, EngineProtocolError,
                   EngineTimeout, EngineUnavailable)

MAX_RESPONSE = 8 * 1024**2


def token_id(value):
    if type(value) is int and value >= 0:
        return value
    if isinstance(value, str) and value.startswith('token_id:'):
        value = value[len('token_id:'):]
        if value.isascii() and value.isdecimal():
            return int(value)
    raise EngineProtocolError('server did not provide actual token IDs')


def pairs(values, limit):
    if not isinstance(values, list) or not values or len(values) > limit:
        raise EngineProtocolError('missing or oversized probability candidates')
    result = []
    for ident, value in values:
        if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value) or value > 0:
            raise EngineProtocolError('invalid log probability')
        result.append((token_id(ident), float(value)))
    if len({ident for ident, _ in result}) != len(result):
        raise EngineProtocolError('duplicate candidate token')
    return result


class HTTPCompletionEngine(EngineAdapter):
    device = 'remote'
    def __init__(self, endpoint, model, api_key=None, server_version='unknown'):
        parsed = urllib.parse.urlsplit(endpoint)
        if parsed.scheme not in ('http', 'https') or not parsed.netloc or parsed.username or parsed.password or parsed.query or parsed.fragment:
            raise ValueError('invalid completion endpoint')
        if not model:
            raise ValueError('remote model must be explicitly configured')
        self.endpoint, self.model, self.api_key = endpoint.rstrip('/'), model, api_key
        self.server_version = server_version

    def capabilities(self):
        return EngineCapabilities(self.name, self.server_version, 'remote', 'unknown',
                                  ('topk-logprobs',), 'logprobs', 20, False,
                                  native_incremental=False)

    def _post(self, route, payload, timeout):
        headers = {'Content-Type': 'application/json'}
        if self.api_key:
            headers['Authorization'] = 'Bearer ' + self.api_key
        request = urllib.request.Request(self.endpoint + route,
                   json.dumps(payload, allow_nan=False).encode(), headers)
        deadline = time.monotonic() + timeout
        try:
            with urllib.request.urlopen(request, timeout=timeout) as response:
                raw = bytearray()
                while True:
                    if time.monotonic() >= deadline:
                        raise EngineTimeout('HTTP completion deadline expired')
                    block = response.read(min(65536, MAX_RESPONSE + 1 - len(raw)))
                    raw.extend(block)
                    if len(raw) > MAX_RESPONSE:
                        raise EngineProtocolError('completion response too large')
                    if not block:
                        break
            return json.loads(raw)
        except (socket.timeout, TimeoutError) as exc:
            raise EngineTimeout('HTTP completion timed out') from exc
        except (urllib.error.URLError, OSError) as exc:
            raise EngineUnavailable('HTTP completion service unavailable') from exc
        except (ValueError, UnicodeError) as exc:
            raise EngineProtocolError('invalid completion JSON') from exc

    def _generate_request(self, prompt, sampling):
        data = self._post(self.route, self.payload(prompt, sampling), sampling.timeout_seconds)
        try:
            text, ids, rows = self.parse(data, sampling.top_k)
            if not isinstance(text, str) or not isinstance(ids, list) or not ids or len(ids) != len(rows) or len(ids) > sampling.max_tokens:
                raise EngineProtocolError('misaligned completion tokens/probabilities')
            steps = []
            for index, (ident, row) in enumerate(zip(ids, rows)):
                ident = token_id(ident)
                if ident not in {key for key, _ in row}:
                    raise EngineProtocolError('chosen token absent from evidence')
                steps.append(Step(ident, text if index == 0 else '', row, {}, 'logprobs'))
        except (KeyError, TypeError, IndexError, ValueError) as exc:
            raise EngineProtocolError('malformed completion evidence') from exc
        yield from steps


class LlamaCppEngine(HTTPCompletionEngine):
    name, route = 'llama.cpp', '/completion'

    def payload(self, prompt, sampling):
        return {'prompt': prompt, 'n_predict': sampling.max_tokens,
                'seed': sampling.seed, 'temperature': 0, 'n_probs': sampling.top_k,
                'post_sampling_probs': False, 'return_tokens': True, 'stream': False}

    def parse(self, data, limit):
        rows = [pairs([(p['id'], p['logprob']) for p in item['top_logprobs']], limit)
                for item in data['completion_probabilities']]
        return data['content'], data['tokens'], rows


class VLLMEngine(HTTPCompletionEngine):
    name, route = 'vllm', '/v1/completions'

    def payload(self, prompt, sampling):
        return {'model': self.model, 'prompt': prompt, 'max_tokens': sampling.max_tokens,
                'seed': sampling.seed, 'temperature': 0, 'logprobs': sampling.top_k,
                'return_tokens_as_token_ids': True, 'stream': False}

    def parse(self, data, limit):
        if len(data['choices']) != 1:
            raise EngineProtocolError('expected one completion choice')
        choice = data['choices'][0]
        rows = [pairs(list(row.items()), limit) for row in choice['logprobs']['top_logprobs']]
        return choice['text'], choice['logprobs']['tokens'], rows
