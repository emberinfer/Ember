"""Synthetic vLLM wire fixtures; no inference service is started."""
import json
import os
import threading
import time
import unittest
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest.mock import patch, PropertyMock, Mock

from engine import VLLMEngine, SamplingParams, EngineProtocolError, EngineTimeout, EngineUnavailable
from evidence import encode_step, opening, chunks, canonical
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

TOKENS = ['token_id:100', 'token_id:101', 'token_id:102']
ROWS = [{TOKENS[0]: -.1, TOKENS[1]: -2.0},
        {TOKENS[1]: -.2, TOKENS[0]: -1.8},
        {TOKENS[2]: -.3, TOKENS[0]: -1.7}]


def frame(start=0, end=3, text=None, finish=None):
    return {'choices': [{'text': 'Hi!'[start:end] if text is None else text,
            'logprobs': {'tokens': TOKENS[start:end], 'top_logprobs': ROWS[start:end]},
            'finish_reason': finish}]}


@contextmanager
def fixture_server(mode='ok', interval=.15):
    seen, sent = [], threading.Event()
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def do_POST(self):
            data = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
            seen.append(data)
            if self.path == '/tokenize':
                raw = json.dumps({'count': 1, 'tokens': [100]}).encode()
                self.send_response(200)
                self.send_header('Content-Type', 'application/json')
                self.send_header('Content-Length', str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)
                return
            if mode == 'unavailable' or mode in ('rejected', 'stream-unavailable') and data['stream']:
                self.send_error(400 if mode == 'rejected' else 502)
                return
            if mode == 'open-timeout':
                time.sleep(.25)
            buffered = not data['stream'] or mode == 'json'
            self.send_response(200)
            self.send_header('Content-Type', 'application/json' if buffered else
                             'text/plain' if mode == 'other' else 'text/event-stream')
            if buffered:
                self.send_header('Content-Length', str(len(json.dumps(frame(finish='stop')).encode())))
            self.end_headers()
            try:
                if buffered:
                    self.wfile.write(json.dumps(frame(finish='stop')).encode())
                    return
                if mode == 'other':
                    return
                def send(value):
                    self.wfile.write(b'data: ' + (value if isinstance(value, bytes) else json.dumps(value).encode()) + b'\n\n')
                    self.wfile.flush()
                if mode == 'trickle':
                    for _ in range(30):
                        self.wfile.write(b':')
                        self.wfile.flush()
                        time.sleep(.02)
                    return
                self.wfile.write(b': comment\nevent: completion\n\n')
                if mode == 'multi':
                    send(frame(0, 2))
                    send(frame(2, 3, finish='stop'))
                elif mode in ('empty', 'empty-text', 'empty-invalid', 'no-tokens'):
                    if mode != 'no-tokens':
                        send(frame(0, 1, text='' if mode == 'empty-text' else 'H'))
                    send({'choices': [{'text': '', 'finish_reason': None if mode == 'empty-invalid' else
                          'length' if mode == 'empty-text' else 'stop', 'logprobs': None}]})
                elif mode in ('length', 'length-text'):
                    send(frame(text='' if mode == 'length' else 'Hi!', finish='length'))
                else:
                    send(frame(0, 1))
                    if mode == 'drop':
                        return
                    time.sleep(.25 if mode == 'timeout' else interval)
                    if mode == 'bad-json':
                        send(b'not-json')
                        return
                    bad = frame(1, 2)
                    if mode == 'bad-prob':
                        bad['choices'][0]['logprobs']['top_logprobs'] = [{TOKENS[1]: 1}]
                    if mode == 'absent':
                        bad['choices'][0]['logprobs']['tokens'] = ['token_id:999']
                    if mode == 'mismatch':
                        bad['choices'][0]['logprobs']['tokens'].append(TOKENS[0])
                    if mode == 'choices':
                        bad['choices'].append(bad['choices'][0])
                    send(bad)
                    time.sleep(interval)
                    send(frame(2, 3, finish=None if mode == 'done-only' else 'stop'))
                sent.set()
                if mode != 'finish-only':
                    send(b'[DONE]')
            except (BrokenPipeError, ConnectionResetError):
                pass
    server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
    server.daemon_threads = True
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f'http://127.0.0.1:{server.server_port}', seen, sent
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


class HTTPStreamingTest(unittest.TestCase):
    def setUp(self):
        tokenizer = Mock()
        tokenizer.decode.side_effect = lambda ids: {100: 'H', 101: 'i', 102: '!'}[ids[0]]
        patcher = patch.object(VLLMEngine, 'tokenizer', new_callable=PropertyMock, return_value=tokenizer)
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_incremental_and_equivalent_evidence(self):
        with fixture_server() as (url, seen, sent):
            engine = VLLMEngine(url, 'fixture', streaming=True)
            session = engine.prefill('hello', SamplingParams(max_tokens=3))
            start = time.monotonic()
            first = engine.decode(session)
            first_time = time.monotonic() - start
            self.assertFalse(sent.is_set())
            steps = [first]
            while (step := engine.decode(session)) is not None:
                steps.append(step)
            self.assertLess(first_time, (time.monotonic() - start) / 2)
            buffered = list(VLLMEngine(url, 'fixture', streaming=False).generate('hello', 0, 3))
            self.assertEqual([encode_step(s) for s in steps], [encode_step(s) for s in buffered])
            self.assertEqual([opening(s) for s in steps], [opening(s) for s in buffered])
            self.assertEqual(canonical([encode_step(s) for s in steps]), canonical([encode_step(s) for s in buffered]))
            key = Ed25519PrivateKey.generate()
            self.assertEqual(chunks('test', steps, key), chunks('test', buffered, key))
            self.assertEqual(''.join(s.text for s in steps), ''.join(s.text for s in buffered))
            self.assertEqual([s.text for s in steps], ['H', 'i', '!'])

    def test_success_modes(self):
        for mode in ('multi', 'json', 'rejected', 'stream-unavailable', 'other', 'empty', 'finish-only', 'done-only', 'length', 'length-text', 'empty-text'):
            with self.subTest(mode=mode), fixture_server(mode, .001) as (url, seen, _):
                engine = VLLMEngine(url, 'fixture', streaming=True)
                if mode == 'rejected':
                    with self.assertRaises(EngineProtocolError):
                        list(engine.generate('hello', 0, 3))
                    self.assertEqual([r['stream'] for r in seen], [True])
                    continue
                steps = list(engine.generate('hello', 0, 3))
                self.assertEqual(engine.thinking_truncated, mode in ('length', 'empty-text'))
                self.assertEqual([r['stream'] for r in seen], [True, False] if mode in ('stream-unavailable', 'other') else [True])
                if mode == 'multi':
                    self.assertEqual([s.text for s in steps], ['H', 'i', '!'])
                if mode == 'json':
                    self.assertEqual(steps, list(VLLMEngine(url, 'fixture', streaming=False).generate('hello', 0, 3)))

    def test_partial_errors_never_retry(self):
        for mode in ('drop', 'bad-json', 'bad-prob', 'absent', 'mismatch', 'choices', 'empty-invalid'):
            with self.subTest(mode=mode), fixture_server(mode, .001) as (url, seen, _):
                iterator = VLLMEngine(url, 'fixture', streaming=True).generate('hello', 0, 3)
                self.assertEqual(next(iterator).token, 100)
                with self.assertRaises(EngineProtocolError):
                    list(iterator)
                self.assertEqual(len(seen), 1)

    def test_limits_and_no_tokens(self):
        for mode, maximum in [('ok', 2), ('no-tokens', 3)]:
            with self.subTest(mode=mode), fixture_server(mode, .001) as (url, seen, _):
                with self.assertRaises(EngineProtocolError):
                    list(VLLMEngine(url, 'fixture', streaming=True).generate('hello', 0, maximum))
                self.assertEqual(len(seen), 1)
        with fixture_server(interval=.001) as (url, seen, _), patch('engine.adapters.http.MAX_RESPONSE', 20):
            with self.assertRaises(EngineProtocolError):
                list(VLLMEngine(url, 'fixture', streaming=True).generate('hello', 0, 3))
            self.assertEqual(len(seen), 1)

    def test_timeout_during_stream(self):
        with fixture_server('timeout') as (url, seen, _):
            engine = VLLMEngine(url, 'fixture', streaming=True)
            session = engine.prefill('hello', SamplingParams(max_tokens=3, timeout_seconds=.1))
            self.assertEqual(engine.decode(session).token, 100)
            with self.assertRaises(EngineTimeout):
                engine.decode(session)
            self.assertEqual(len(seen), 1)

    def test_open_timeout_and_overall_deadline_never_retry(self):
        for mode in ('open-timeout', 'trickle'):
            with self.subTest(mode=mode), fixture_server(mode) as (url, seen, _):
                engine = VLLMEngine(url, 'fixture', streaming=True)
                session = engine.prefill('hello', SamplingParams(max_tokens=3, timeout_seconds=.1))
                start = time.monotonic()
                with self.assertRaises(EngineTimeout):
                    engine.decode(session)
                self.assertLess(time.monotonic() - start, .4)
                self.assertEqual(len(seen), 1)

    def test_environment_and_override(self):
        with patch.dict(os.environ, {'EMBER_VLLM_STREAM': '0'}), fixture_server(interval=.001) as (url, seen, _):
            list(VLLMEngine(url, 'fixture').generate('hello', 0, 3))
            self.assertIs(seen[-1]['stream'], False)
            list(VLLMEngine(url, 'fixture', streaming=True).generate('hello', 0, 3))
            self.assertIs(seen[-1]['stream'], True)
        with patch.dict(os.environ, {'EMBER_VLLM_STREAM': '1'}):
            self.assertFalse(VLLMEngine('http://localhost', 'fixture', streaming=False).streaming)

    def test_fallback_failure(self):
        with fixture_server('unavailable') as (url, seen, _):
            with self.assertRaises(EngineUnavailable):
                list(VLLMEngine(url, 'fixture', streaming=True).generate('hello', 0, 3))
            self.assertEqual([r['stream'] for r in seen], [True, False])


if __name__ == '__main__':
    unittest.main()
