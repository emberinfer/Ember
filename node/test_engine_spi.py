"""Wire-contract fixtures are synthetic and never count as backend inference."""
import json
import math
import threading
import time
import unittest
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from engine import (LlamaCppEngine, VLLMEngine, MockEngine, SamplingParams,
                    UnsupportedCapability, EngineProtocolError, EngineTimeout,
                    EngineUnavailable, require_full_evidence)
from evidence import opening, encode_step

FIXED_CASES = [('hello', 7), ('Reply briefly: test 1', 19)]


@contextmanager
def fixture_server(mode='ok'):
    seen = []
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def do_POST(self):
            data = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
            seen.append((self.path, data))
            if mode == 'timeout':
                time.sleep(.1)
            if mode == 'unavailable':
                self.send_error(503)
                return
            rows = [{100: -.1, 101: -2.0}, {101: -.2, 100: -1.8}]
            if self.path == '/completion':
                result = {'content': 'Hi', 'tokens': [100, 101],
                          'completion_probabilities': [{'top_logprobs': [
                              {'id': k, 'logprob': v} for k, v in row.items()]} for row in rows]}
                if mode == 'missing':
                    result.pop('tokens')
                if mode == 'nonfinite':
                    result['completion_probabilities'][0]['top_logprobs'][0]['logprob'] = math.nan
                if mode == 'probabilities':
                    result['completion_probabilities'][0]['top_logprobs'][0]['logprob'] = .5
                if mode == 'duplicate':
                    result['completion_probabilities'][0]['top_logprobs'][1]['id'] = 100
                if mode == 'legacy-probs':
                    result['probs'] = result.pop('completion_probabilities')
            else:
                result = {'choices': [{'text': 'Hi', 'logprobs': {
                    'tokens': ['token_id:100', 'token_id:101'],
                    'top_logprobs': [{f'token_id:{k}': v for k, v in row.items()} for row in rows]}}]}
                if mode == 'missing':
                    result['choices'][0]['logprobs']['tokens'] = ['H', 'i']
                if mode == 'nonfinite':
                    result['choices'][0]['logprobs']['top_logprobs'][0]['token_id:100'] = math.inf
                if mode == 'misaligned':
                    result['choices'][0]['logprobs']['tokens'].pop()
            raw = json.dumps(result).encode() if mode != 'bad-json' else b'no json'
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(raw)))
            self.end_headers()
            try:
                self.wfile.write(raw)
            except (BrokenPipeError, ConnectionResetError):
                pass
    server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f'http://127.0.0.1:{server.server_port}', seen
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


def contract_rows():
    rows = []
    for adapter in (LlamaCppEngine, VLLMEngine):
        with fixture_server() as (endpoint, seen):
            engine = adapter(endpoint, 'fixture-model', server_version='fixture-only')
            for prompt, seed in FIXED_CASES:
                steps = list(engine.generate(prompt, seed, 2))
                assert ''.join(s.text for s in steps) == 'Hi'
                assert [s.token for s in steps] == [100, 101]
                assert all(s.score_kind == 'logprobs' and not s.activations for s in steps)
                assert all(opening(s)['score_kind'] == 'logprobs' for s in steps)
                assert all(encode_step(s)['score_kind'] == 'logprobs' for s in steps)
                assert seen[-1][1]['seed'] == seed and seen[-1][1]['prompt'] == prompt
                rows.append({'engine': engine.name, 'prompt': prompt, 'seed': seed,
                    'text': ''.join(s.text for s in steps), 'tokens': len(steps),
                    'evidence': 'topk-logprobs', 'activations': False, 'replay': False,
                    'source': 'local fake HTTP fixture', 'result': 'PASS'})
            engine.close()
    return rows


class EngineSPITest(unittest.TestCase):
    def test_fixed_prompt_seed_contracts(self):
        self.assertEqual(len(contract_rows()), 4)

    def test_remote_admission_and_replay_are_fail_closed(self):
        for adapter in (LlamaCppEngine, VLLMEngine):
            engine = adapter('http://127.0.0.1:1', 'fixture')
            with self.assertRaises(UnsupportedCapability):
                require_full_evidence(engine)
            with self.assertRaises(UnsupportedCapability):
                engine.replay('hello', 7, [1])
            with self.assertRaises(UnsupportedCapability):
                engine.prefill('hello', SamplingParams(temperature=.5))
            cap = engine.capabilities().to_proto()
            self.assertEqual(cap.score_kind, 'logprobs')
            self.assertEqual(cap.numerical_format, 'unknown')
            self.assertEqual(cap.device_class, 'remote')
            self.assertFalse(cap.forced_prefix_replay)

    def test_malformed_or_missing_evidence(self):
        for adapter, modes in [(LlamaCppEngine, ('missing', 'nonfinite', 'probabilities', 'duplicate', 'bad-json')),
                               (VLLMEngine, ('missing', 'nonfinite', 'misaligned', 'bad-json'))]:
            for mode in modes:
                with self.subTest(adapter=adapter.__name__, mode=mode), fixture_server(mode) as (endpoint, _):
                    with self.assertRaises(EngineProtocolError):
                        list(adapter(endpoint, 'fixture').generate('hello', 7, 2))

    def test_timeouts_and_server_errors(self):
        for mode, error in [('timeout', EngineTimeout), ('unavailable', EngineUnavailable)]:
            with fixture_server(mode) as (endpoint, _):
                engine = VLLMEngine(endpoint, 'fixture')
                session = engine.prefill('hello', SamplingParams(max_tokens=2, timeout_seconds=.01))
                with self.assertRaises(error):
                    engine.decode(session)
                self.assertTrue(session.closed)

    def test_sessions_deadlines_limits_and_legacy_raw_bytes(self):
        engine = MockEngine()
        session = engine.prefill('hello', SamplingParams(max_tokens=2, top_k=1))
        with self.assertRaises(EngineUnavailable):
            engine.prefill('other', SamplingParams(top_k=1))
        self.assertNotIn('score_kind', opening(engine.decode(session)))
        self.assertNotIn('score_kind', encode_step(engine.decode(session)))
        self.assertIsNone(engine.decode(session))
        expired = engine.prefill('hello', SamplingParams(top_k=1))
        expired.deadline = time.monotonic() - 1
        with self.assertRaises(EngineTimeout):
            engine.decode(expired)
        self.assertTrue(expired.closed)
        with self.assertRaises(ValueError):
            engine.prefill('hello', SamplingParams(seed=2**63, top_k=1))


if __name__ == '__main__':
    unittest.main()
