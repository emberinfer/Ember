"""Real-client pipe probe; no model downloads, stage calls, or production access.

Run with PYTHONPATH=core-py:proto:node <python-with-dependencies> -m unittest
node.test_pipeline_serve_protocol -v. Uses the actual imported client and cache.
"""
import json
import os
import selectors
import subprocess
import sys
import time
import unittest


class ServeProtocolTest(unittest.TestCase):
    def test_real_serve_without_stdin_eof(self):
        started = time.monotonic()
        with subprocess.Popen(
            [sys.executable, '-m', 'pipeline_client', '--serve'],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, text=True, env=os.environ.copy(),
        ) as process:
            try:
                with selectors.DefaultSelector() as selector:
                    selector.register(process.stdout, selectors.EVENT_READ)
                    for index in range(2):
                        process.stdin.write(json.dumps({
                            'model_path': '/nonexistent/gwrobust-local-only',
                        }) + '\n')
                        process.stdin.flush()
                        self.assertTrue(selector.select(20), 'no frame with stdin open')
                        frame = json.loads(process.stdout.readline())
                        self.assertEqual(frame['type'], 'job_end')
                        self.assertFalse(frame['ok'])
                        self.assertIn('gwrobust-local-only', frame['error'])
                        print(f'job={index} stdin=open elapsed={time.monotonic()-started:.3f}s frame={frame}', flush=True)
                process.stdin.close()
                self.assertEqual(process.wait(timeout=5), 0)
                self.assertEqual(process.stderr.read(), '')
            finally:
                if process.poll() is None:
                    process.kill()
                    process.wait(timeout=5)
