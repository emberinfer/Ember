"""Loopback-only D1 material distribution acceptance; no production endpoints.
Run after building .runtime/s1/{emberchain,emberd}, with core-py:proto:node on PYTHONPATH.
The isolated ledger genesis contains a signed tiny publication; the gateway
uses chain enrollment with locally funded fixture identities only.
"""
import base64
import hashlib
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request

import grpc
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from ember_core import digest, sign, node_id
from ember_pb2 import ChainGenesis, ModelManifest, TunnelFrame
from ember_pb2_grpc import NodeTunnelStub
from agent.join import ProfileAssertion, identity
from cryptography.hazmat.primitives import serialization

ROOT = Path(__file__).resolve().parents[3]
OUT = ROOT / 'services/ledger/testdata/s1-output'


def port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


def main():
    OUT.mkdir(exist_ok=True)
    children = []
    logs = []
    chain, daemon = (str(ROOT / '.runtime/s1' / name) for name in ('emberchain', 'emberd'))
    with tempfile.TemporaryDirectory(prefix='s1-local-') as tmp:
        tmp = Path(tmp)
        def run(*args):
            try:
                return subprocess.check_output(args, text=True, stderr=subprocess.STDOUT)
            except subprocess.CalledProcessError as error:
                print(error.output, flush=True)
                raise
        def start(name, *args):
            log = (OUT / (name + '.log')).open('w')
            logs.append(log)
            process = subprocess.Popen(args, stdout=log, stderr=subprocess.STDOUT, cwd=tmp)
            children.append(process)
            return process
        def wait_for(predicate, timeout=35):
            end = time.monotonic() + timeout
            while time.monotonic() < end:
                try:
                    if predicate():
                        return
                except (OSError, ValueError, grpc.RpcError):
                    pass
                time.sleep(.2)
            raise RuntimeError('local acceptance timeout; inspect s1-output logs')
        try:
            ident = 'ember-s1-not-bundled'
            ledger = 'ember-s1-isolated'
            key = Ed25519PrivateKey.generate()
            manifest = ModelManifest(chain_id=ident)
            sign(manifest, key)
            genesis = ChainGenesis(chain_id=ident, execution_profile_ver='mock-s1-v1',
                                   model_manifest_root=digest(manifest), price_output=1)
            sign(genesis, key)
            gr, mr = genesis.SerializeToString(), manifest.SerializeToString()
            model = dict(id=ident, genesis_hash=hashlib.sha256(gr).hexdigest(), profile_hash=digest(genesis).hex(),
                         publication=dict(genesis=base64.b64encode(gr).decode(), manifest=base64.b64encode(mr).decode(),
                                          publisher=node_id(genesis.signer), manifest_root=digest(manifest).hex()))
            authority = tmp / 'authority.key'
            authority.write_bytes(key.private_bytes(serialization.Encoding.Raw, serialization.PrivateFormat.Raw, serialization.NoEncryption()))
            authority.write_bytes(authority.read_bytes() + genesis.signer)
            authority.chmod(0o600)
            _, client_id = identity(tmp / 'node')
            auth = tmp / 'auth.json'
            auth.write_text(json.dumps([dict(token='s1-local-only-token-' + 'x'*32, address=client_id)]))
            auth.chmod(0o600)
            home = tmp / 'validator' 
            print(run(chain, 'init-validator', '--home', str(home)), flush=True)
            # Explicit isolated-node config also avoids quoted numeric defaults
            # in CometBFT's template being rejected by the concurrent D2 parser.
            (home / 'config/config.toml').write_text('[statesync]\nenable = false\n')
            app = tmp / 'app.json'
            app.write_text(json.dumps(dict(parameters=dict(chain_id=ledger, unbonding_blocks=3,
                           slash_multiplier=20, dispute_blocks=1), models=[model],
                           accounts=[dict(id=node_id(genesis.signer), balance=1000000), dict(id=client_id, balance=1000000, stake=5000)],
                           coordinators=[node_id(genesis.signer)], auditors=[node_id(genesis.signer)], governors=[node_id(genesis.signer)])))
            print(run(chain, 'genesis', '--out', str(home / 'config/genesis.json'), '--chain-id', ledger,
                      '--app-state', str(app), '--validators', str(home)), flush=True)
            rpcport, p2p, tunnel, http = [port() for _ in range(4)]
            rpc = f'http://127.0.0.1:{rpcport}'
            start('integration-chain', chain, 'start', '--home', str(home), '--rpc', f'tcp://127.0.0.1:{rpcport}',
                  '--p2p', f'tcp://127.0.0.1:{p2p}')
            def height():
                with urllib.request.urlopen(rpc + '/status', timeout=1) as response:
                    return int(json.load(response)['result']['sync_info']['latest_block_height']) > 0
            wait_for(height)
            start('integration-gateway', daemon, 'all', '--profile-id', ident, '--chain-rpc', rpc,
                  '--chain-id', ledger, '--ledger-backend', 'chain', '--chain-coordinator-key', str(authority),
                  '--chain-auditor-key', str(authority), '--chain-auth-file', str(auth), '--http', f'127.0.0.1:{http}',
                  '--grpc', f'127.0.0.1:{tunnel}', '--insecure-localhost')
            endpoint = f'127.0.0.1:{tunnel}'
            channel = grpc.insecure_channel(endpoint)
            grpc.channel_ready_future(channel).result(timeout=15)
            document = json.loads((ROOT / 'site/network.json').read_text())
            document.update(public_testnet='active')
            document['ledger']['chain_id'] = ledger
            document['access'].update(seed=endpoint, tls_fingerprint='a'*64, scheduler_id='b'*64,
                                      gateways=[dict(id='local', profiles=[], dynamic_profiles=True,
                                      node_tunnel=dict(endpoint=endpoint, tls_fingerprint='a'*64))], private_gateways=[])
            network = tmp / 'network.json'
            network.write_text(json.dumps(document))
            policy = tmp / 'trust.json'
            policy.write_text(json.dumps(dict(profiles={ident: dict(publishers=[node_id(genesis.signer)])})))
            print(run(sys.executable, '-m', 'agent', 'configure', '--network-parameters', str(network),
                      '--profile-id', ident, '--chain-rpc', rpc, '--chain-cli', chain,
                      '--trust-policy', str(policy), '--profiles', str(tmp / 'profiles'),
                      '--model-path', str(tmp), '--config', str(tmp / 'node.env')), flush=True)
            assert (tmp / 'profiles' / ident / 'genesis.pb').read_bytes() == gr
            start('integration-client', sys.executable, '-u', '-m', 'agent', 'join', '--state-dir', str(tmp / 'node'),
                  '--chain', ident, '--coordinator', endpoint, '--insecure-localhost', '--engine', 'mock',
                  '--genesis-sha256', hashlib.sha256(gr).hexdigest(), '--manifest-sha256', hashlib.sha256(mr).hexdigest(),
                  '--chain-rpc', rpc, '--chain-cli', chain, '--ledger-chain-id', ledger, '--trust-policy', str(policy), '--registration-ttl', '100')
            wait_for(lambda: 'JOIN READY' in (OUT / 'integration-client.log').read_text())
            print('PASS: local ledger publication -> emberd --profile-id -> configure exact bytes -> asserted JOIN READY', flush=True)
            bad = grpc.intercept_channel(channel, ProfileAssertion('ember-s1-other'))
            stream = NodeTunnelStub(bad).Connect(iter([TunnelFrame(kind='join', node_id='wrong-profile')]), timeout=5)
            try:
                next(stream)
                raise AssertionError('wrong profile received challenge')
            except grpc.RpcError as error:
                assert 'requested profile ID' in error.details(), error.details()
                print('PASS: different profile fails closed before challenge:', error.details(), flush=True)
        finally:
            for child in reversed(children):
                child.terminate()
            for child in reversed(children):
                try:
                    child.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    child.kill(); child.wait()
            for log in logs:
                log.close()


if __name__ == '__main__':
    main()
