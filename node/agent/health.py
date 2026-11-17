"""Loopback operations endpoint for an Ember node process."""
import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def start_health_server(agent, address):
    if not address:
        return None
    host, port = address.rsplit(':', 1)
    if host not in ('127.0.0.1', 'localhost', '::1'):
        raise ValueError('node health endpoint must listen on loopback')

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def do_GET(self):
            if self.path == '/healthz':
                return self._send(200, b'ok\n', 'text/plain')
            if self.path == '/readyz':
                code, body = (200, b'ready\n') if agent.ready else (503, b'not ready\n')
                return self._send(code, body, 'text/plain')
            if self.path == '/metrics':
                body = ('# TYPE ember_node_ready gauge\n'
                        f'ember_node_ready {1 if agent.ready else 0}\n'
                        '# TYPE ember_node_requests_total counter\n'
                        f'ember_node_requests_total{{result="success"}} {agent.completed_requests}\n'
                        f'ember_node_requests_total{{result="failure"}} {agent.failed_requests}\n'
                        '# TYPE ember_node_uptime_seconds gauge\n'
                        f'ember_node_uptime_seconds {time.monotonic() - agent.started_at:.3f}\n').encode()
                return self._send(200, body, 'text/plain; version=0.0.4')
            self._send(404, json.dumps({'error': 'not found'}).encode(), 'application/json')

        def _send(self, code, body, content_type):
            self.send_response(code)
            self.send_header('Content-Type', content_type)
            self.send_header('Content-Length', str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    server = ThreadingHTTPServer((host, int(port)), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server
