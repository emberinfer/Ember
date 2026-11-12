"""HTTP role adapters. Run behind TLS/authenticated operator ingress.

Executor plaintext never crosses the gateway API. Bundles are deliberately
public ciphertext, while custodians release shares only to assigned auditors.
"""
import argparse
import json
from pathlib import Path
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from .crypto import canonical, digest, unb64
from .envelope import validate


def handler(executor=None, custodian=None, capture=None):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self,*args):
            pass  # No paths, attacker text, prompts, or outputs in access logs.

        def reply(self,status,value):
            raw=canonical(value)
            self.send_response(status)
            self.send_header('Content-Type','application/json')
            self.send_header('Content-Length',str(len(raw)))
            self.end_headers();self.wfile.write(raw)

        def do_POST(self):
            try:
                self.connection.settimeout(30)
                size=int(self.headers.get('Content-Length','0'))
                limit=64*1024*1024 if executor and self.path=='/v2/resume' else 1048576
                if not 0<size<=limit or self.headers.get('Transfer-Encoding'):
                    raise ValueError('invalid body size')
                raw=self.rfile.read(size)
                if len(raw)!=size: raise ValueError('short body')
                if executor and self.path=='/v2/requests':
                    self.reply(200,executor.execute(raw))
                elif executor and self.path=='/v2/tokenize':
                    self.reply(200,executor.prepare_pricing(raw))
                elif executor and self.path=='/v2/resume':
                    from .crypto import b64
                    value=json.loads(raw)
                    result=executor.execute(unb64(value['transport']),value.get('checkpoint_tokens'),
                        [unb64(c) for c in value.get('prior_ciphertexts',[])])
                    self.reply(200,result)
                elif executor and self.path=='/bundles/access':
                    from .crypto import b64
                    self.reply(200,dict(ciphertext=b64(executor.bundle(json.loads(raw)))))
                elif custodian and self.path=='/escrow/deposit':
                    # A deposit carries the same consumer-signed transport as execution.
                    e,_=validate(raw)
                    record=custodian.chain.request(e['request_id'])
                    if record['envelope_hash']!=digest(e): raise ValueError('unregistered envelope')
                    actor=custodian.chain.actor
                    custodian.receive(e['request_id'],unb64(e['wrapped_keys']['shares'][actor]))
                    self.reply(200,dict(deposited=True))
                elif custodian and self.path=='/escrow/release':
                    from .crypto import b64
                    self.reply(200,dict(share=b64(custodian.release(json.loads(raw)))))
                else:
                    self.reply(404,dict(error='NOT_FOUND'))
            except Exception:
                self.reply(400,dict(error='REQUEST_REFUSED'))

        def do_GET(self):
            try:
                if not executor or not self.path.startswith('/bundles/'):
                    self.reply(404,dict(error='NOT_FOUND'));return
                ref=self.path.removeprefix('/bundles/')
                if not executor.chain.public_bundle(ref): raise ValueError('authenticated access required')
                raw=executor.store.get(ref)
                self.send_response(200)
                self.send_header('Content-Type','application/octet-stream')
                self.send_header('Content-Length',str(len(raw)))
                self.end_headers();self.wfile.write(raw)
            except (ValueError,FileNotFoundError):
                self.reply(404,dict(error='NOT_FOUND'))
    return Handler


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('role',choices=('executor','custodian'))
    p.add_argument('--listen',default='127.0.0.1:19120')
    p.add_argument('--chain-cli',required=True);p.add_argument('--rpc',required=True)
    p.add_argument('--chain-id',required=True);p.add_argument('--genesis',required=True)
    p.add_argument('--signing-key',required=True);p.add_argument('--wrapping-key',required=True)
    p.add_argument('--storage',required=True);p.add_argument('--epoch')
    p.add_argument('--model-path');p.add_argument('--model-id');p.add_argument('--device',choices=('cuda','cpu','auto'),default='cuda')
    a=p.parse_args()
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
    from cryptography.hazmat.primitives.asymmetric.x25519 import X25519PrivateKey
    from .chain import Chain
    from .crypto import public,b64
    for path in (a.signing_key,a.wrapping_key):
        if Path(path).stat().st_mode & 0o077: raise ValueError('key file permissions must be 0600')
    raw=Path(a.signing_key).read_bytes()
    if len(raw) not in (32,64): raw=bytes.fromhex(raw.decode().strip())
    signing=Ed25519PrivateKey.from_private_bytes(raw[:32])
    wrapping=X25519PrivateKey.from_private_bytes(bytes.fromhex(Path(a.wrapping_key).read_text().strip()))
    chain=Chain(a.chain_cli,a.rpc,a.chain_id,a.genesis,signing,a.signing_key)
    if chain.operator(chain.actor)['wrap_key']!=b64(public(wrapping)):
        raise ValueError('wrapping key differs from chain registration')
    if a.role=='executor':
        from engine import TransformersEngine
        from .executor import Executor
        from .evidence import BundleStore
        if not a.model_path or not a.model_id: p.error('--model-path and --model-id required')
        instance=Executor(chain,signing,wrapping,TransformersEngine(a.model_path,a.device),BundleStore(a.storage),a.model_id)
        http_handler=handler(executor=instance)
    else:
        from .escrow import Custodian
        if not a.epoch:p.error('--epoch required')
        if chain.actor not in chain.epoch(a.epoch)['members']:raise ValueError('not epoch member')
        http_handler=handler(custodian=Custodian(chain,a.epoch,wrapping,a.storage))
    host,port=a.listen.rsplit(':',1)
    server=ThreadingHTTPServer((host,int(port)),http_handler)
    server.daemon_threads=True
    server.serve_forever()

if __name__=='__main__':
    main()
