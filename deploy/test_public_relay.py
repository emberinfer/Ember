"""Exercise state-sync read methods through the actual HTTP allowlist."""
import http.client
import http.server
import importlib.util
import json
from pathlib import Path
import threading
import unittest

spec = importlib.util.spec_from_file_location('public_relay', Path(__file__).with_name('public-relay.py'))
relay = importlib.util.module_from_spec(spec)
spec.loader.exec_module(relay)


class RelayStateSyncTest(unittest.TestCase):
    def test_read_methods_and_admin_rejection(self):
        class Upstream(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200)
                self.end_headers()
                self.wfile.write(b'{"result":{}}')
            do_POST = do_GET
            def log_message(self, *args):
                pass

        upstream = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Upstream)
        proxy = http.server.ThreadingHTTPServer(('127.0.0.1', 0), relay.handler('rpc', upstream.server_port))
        workers = [threading.Thread(target=s.serve_forever, daemon=True) for s in (upstream, proxy)]
        for thread in workers:
            thread.start()
        try:
            for method in ('status', 'commit', 'block', 'header', 'validators', 'consensus_params', 'genesis', 'genesis_chunked'):
                for verb, path, body in (('GET', '/' + method, None), ('POST', '/', json.dumps({'jsonrpc': '2.0', 'id': 1, 'method': method}))):
                    client = http.client.HTTPConnection('127.0.0.1', proxy.server_port, timeout=5)
                    client.request(verb, path, body)
                    response = client.getresponse()
                    self.assertEqual(response.status, 200, method)
                    response.read()
                    client.close()
            for method in ('unsafe_flush_mempool', 'dial_peers', 'consensus_state'):
                for verb, path, body, expected in (('GET', '/' + method, None, 404), ('POST', '/', json.dumps({'method': method}), 403)):
                    client = http.client.HTTPConnection('127.0.0.1', proxy.server_port, timeout=5)
                    client.request(verb, path, body)
                    response = client.getresponse()
                    self.assertEqual(response.status, expected)
                    response.read()
                    client.close()
        finally:
            for server in (upstream, proxy):
                server.shutdown()
                server.server_close()
            for thread in workers:
                thread.join()


if __name__ == '__main__':
    unittest.main(verbosity=2)
