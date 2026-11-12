#!/usr/bin/env python3
import argparse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def do_POST(self):
        data = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        base = (sum(data['prompt'].encode()) + int(data['seed'])) % 1000 + 100
        tokens = data.get('tokens') or [base + index for index in range(data['count'])]
        steps = [{'token': token, 'text': chr(65 + index),
                  'topk': [[token, 1.0], [token + 1000, 0.0]],
                  'activations': {'toy.layer': '0000803f'}}
                 for index, token in enumerate(tokens)]
        raw = json.dumps({'steps': steps}).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--listen', default='127.0.0.1:0')
    parser.add_argument('--port-file', required=True)
    args = parser.parse_args()
    host, port = args.listen.rsplit(':', 1)
    server = ThreadingHTTPServer((host, int(port)), Handler)
    with open(args.port_file, 'w', encoding='ascii') as stream:
        stream.write(str(server.server_port))
    server.serve_forever()


if __name__ == '__main__':
    main()
