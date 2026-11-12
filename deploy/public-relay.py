#!/usr/bin/env python3
"""Relay Ember public functions to private SSH forwards; runs no Ember roles."""
import argparse
import asyncio
import http.client
import http.server
import json
import ssl
import threading
from urllib.parse import urlsplit

RPC_METHODS = frozenset(('status', 'abci_query', 'broadcast_tx_commit',
                        'broadcast_tx_sync', 'tx', 'commit', 'block', 'header',
                        'validators', 'genesis', 'genesis_chunked'))
HOP_HEADERS = frozenset(('connection', 'keep-alive', 'proxy-authenticate',
                        'proxy-authorization', 'te', 'trailer',
                        'transfer-encoding', 'upgrade', 'host'))


def handler(role, upstream, capture=None):
    class Proxy(http.server.BaseHTTPRequestHandler):
        protocol_version = 'HTTP/1.1'

        def handle_one_request(self):
            self.connection.settimeout(30)
            super().handle_one_request()

        def proxy(self):
            path = urlsplit(self.path).path
            allowed = {'gateway': {'/v1/chat/completions', '/v2/requests', '/healthz', '/readyz'},
                       'faucet': {'/testnet/faucet'},
                       'rpc': {'/' + m for m in RPC_METHODS} | {'/'}}[role]
            if path not in allowed or self.command not in ('GET', 'POST'):
                self.send_error(404)
                return
            try:
                length = int(self.headers.get('Content-Length', '0'))
                if not 0 <= length <= 1048576 or self.headers.get('Transfer-Encoding'):
                    self.send_error(413)
                    return
                body = self.rfile.read(length) if length else None
                if capture and role == 'gateway':
                    capture('request', body or b'')
                if role == 'rpc' and path == '/':
                    message = json.loads(body or b'{}')
                    if not isinstance(message, dict) or message.get('method') not in RPC_METHODS:
                        self.send_error(403)
                        return
                headers = {k: v for k, v in self.headers.items()
                           if k.lower() not in HOP_HEADERS | {'x-forwarded-for', 'forwarded'}}
                # Never trust the external caller's claimed source identity.
                headers['X-Forwarded-For'] = self.client_address[0]
                headers['Connection'] = 'close'
                connection = http.client.HTTPConnection('127.0.0.1', upstream, timeout=950)
                try:
                    connection.request(self.command, self.path, body, headers)
                    response = connection.getresponse()
                    self.send_response(response.status)
                    for key, value in response.getheaders():
                        if key.lower() not in HOP_HEADERS:
                            self.send_header(key, value)
                    self.send_header('Connection', 'close')
                    self.end_headers()
                    while data := response.read1(65536):
                        if capture and role == 'gateway':
                            capture('response', data)
                        self.wfile.write(data)
                        self.wfile.flush()
                finally:
                    connection.close()
            except (ValueError, OSError, http.client.HTTPException):
                self.close_connection = True
            self.close_connection = True

        do_GET = proxy
        do_POST = proxy
    return Proxy


async def seed_client(reader, writer, upstream, upstream_tls=None):
    peer = None
    try:
        tls_args = dict(ssl=upstream_tls, server_hostname='127.0.0.1') if upstream_tls else {}
        other_reader, peer = await asyncio.wait_for(
            asyncio.open_connection('127.0.0.1', upstream, **tls_args), 10)

        async def copy(source, target):
            while data := await source.read(65536):
                target.write(data)
                await target.drain()
        jobs = [asyncio.create_task(copy(reader, peer)), asyncio.create_task(copy(other_reader, writer))]
        try:
            await asyncio.wait(jobs, return_when=asyncio.FIRST_COMPLETED)
        finally:
            for job in jobs:
                job.cancel()
            await asyncio.gather(*jobs, return_exceptions=True)
    except (OSError, asyncio.TimeoutError):
        pass
    finally:
        writer.close()
        if peer:
            peer.close()


async def serve(args):
    public_tls = upstream_tls = None
    if args.seed_cert:
        public_tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        public_tls.load_cert_chain(args.seed_cert, args.seed_key)
        public_tls.set_alpn_protocols(['h2'])
        upstream_tls = ssl.create_default_context(cafile=args.seed_upstream_ca)
        upstream_tls.set_alpn_protocols(['h2'])
    servers = []
    for role in ('rpc', 'gateway', 'faucet'):
        server = http.server.ThreadingHTTPServer((args.bind, getattr(args, role + '_port')),
                                                handler(role, getattr(args, role + '_upstream')))
        server.daemon_threads = True
        servers.append(server)
    for server in servers:
        threading.Thread(target=server.serve_forever, daemon=True).start()
    server = await asyncio.start_server(
        lambda r, w: seed_client(r, w, args.seed_upstream, upstream_tls),
        args.bind, args.seed_port, ssl=public_tls)
    async with server:
        await server.serve_forever()


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--bind', default='127.0.0.1')
    parser.add_argument('--seed-cert', help='public-address seed certificate; omit for TLS passthrough')
    parser.add_argument('--seed-key', help='public seed TLS private key')
    parser.add_argument('--seed-upstream-ca', help='trusted backend certificate for TLS bridging')
    for role in ('seed', 'rpc', 'gateway', 'faucet'):
        parser.add_argument('--' + role + '-port', type=int, required=True)
        parser.add_argument('--' + role + '-upstream', type=int, required=True)
    args = parser.parse_args()
    if any((args.seed_cert, args.seed_key, args.seed_upstream_ca)) and not all(
            (args.seed_cert, args.seed_key, args.seed_upstream_ca)):
        parser.error('TLS bridging requires --seed-cert, --seed-key and --seed-upstream-ca')
    asyncio.run(serve(args))
