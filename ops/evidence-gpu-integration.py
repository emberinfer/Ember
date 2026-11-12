#!/usr/bin/env python3
"""Isolated GB10 evidence-transfer staging: real engine, CometBFT, gateway and assigned auditor.

Run with a CUDA Python environment and PYTHONPATH=core-py:proto:node.
Artifacts and private test keys stay under --output (use an ignored directory).
"""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import time
import urllib.request

from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from ember_core import digest, node_id, sign
from ember_pb2 import ChainGenesis, ModelManifest, AuditVerdict
from auditor.gpu import GPU_PROFILE, gpu_engine_id
from engine.spi import EngineCapabilities


def main():
    cli = argparse.ArgumentParser(description=__doc__)
    cli.add_argument('--model', required=True)
    cli.add_argument('--output', required=True)
    cli.add_argument('--prompt', default=('Output every integer from 1 through 10000, in order, '
        'separated by commas. Do not use ellipses, summaries, or code. '
        'Do not stop until reaching 10000.'))
    args = cli.parse_args()
    root = Path(__file__).resolve().parents[1]
    out = Path(args.output).resolve()
    out.mkdir(parents=True, exist_ok=False)
    model = Path(args.model).resolve()
    processes = []
    def run(*command):
        return subprocess.check_output([str(x) for x in command], text=True).strip()
    def start(name, *command):
        log = open(out / (name + '.log'), 'w')
        p = subprocess.Popen([str(x) for x in command], stdout=log, stderr=subprocess.STDOUT,
                             cwd=root)
        log.close()
        processes.append(p)
    def port():
        with socket.socket() as s:
            s.bind(('127.0.0.1', 0))
            return s.getsockname()[1]
    def wait(check, seconds=120):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            try:
                value = check()
                if value:
                    return value
            except Exception:
                pass
            failed = [p.returncode for p in processes if p.poll() is not None]
            if failed:
                raise RuntimeError(f'staging process exited: {failed}; inspect {out}')
            time.sleep(1)
        raise TimeoutError(f'staging timeout; inspect {out}')
    def save(name, value):
        (out / name).write_text(json.dumps(value, indent=2) + '\n')
    try:
        chain, daemon = out / 'emberchain', out / 'emberd'
        subprocess.run(['go', 'build', '-o', str(chain), './cmd/emberchain'], cwd=root, check=True)
        subprocess.run(['go', 'build', '-o', str(daemon), './cmd/emberd'], cwd=root, check=True)
        identities = {}
        for role in ('payer', 'coordinator', 'auditor', 'producer', 'reference', 'governor'):
            key = Ed25519PrivateKey.generate()
            raw = key.private_bytes(serialization.Encoding.Raw, serialization.PrivateFormat.Raw,
                                    serialization.NoEncryption())
            path = out / (role + '.key')
            if role in ('coordinator', 'auditor'):
                raw += key.public_key().public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)
            path.write_bytes(raw); path.chmod(0o600)
            public = key.public_key().public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)
            identities[role] = (key, node_id(public), path)
        chain_id, model_id = 'ember-evidence-gpu-integration', 'ember-qwen2.5-1.5b-evidence-gpu'
        manifest = ModelManifest(chain_id=model_id, total_layers=28, license='Apache-2.0')
        for path in sorted(model.iterdir()):
            if path.suffix not in ('.json', '.safetensors', '.txt', '.model'):
                continue
            h = hashlib.sha256()
            with path.open('rb') as f:
                for block in iter(lambda: f.read(8 * 1024 * 1024), b''):
                    h.update(block)
            manifest.artifacts.add(name=path.name, size=path.stat().st_size, sha256=h.digest())
        governor = identities['governor'][0]
        sign(manifest, governor)
        genesis = ChainGenesis(chain_id=model_id, model_manifest_root=digest(manifest),
            quant='none', dtype='bfloat16', execution_profile_ver=GPU_PROFILE, price_output=1)
        import torch
        import transformers
        assert torch.cuda.is_available() and torch.cuda.get_device_name(0) == 'NVIDIA GB10'
        caps = EngineCapabilities('transformers', transformers.__version__, 'cuda', 'bfloat16',
                                  ('full',), 'raw-logits', 16, True)
        runtime = dict(actual_device='cuda', torch_version=torch.__version__,
                       cuda_version=torch.version.cuda, accelerator_name=torch.cuda.get_device_name(0))
        fingerprint = gpu_engine_id(caps, runtime)
        policy = genesis.numeric_audit_policy
        policy.metric, policy.top_k = 'rank-probability-v1', 16
        policy.layers.extend(['0', '14', '27'])
        policy.calibration_ref = hashlib.sha256(b'Evidence isolated same-runtime integrity staging').digest()
        policy.bands.add(producer_engine=fingerprint, producer_device='cuda',
            reference_engine=fingerprint, reference_device='cuda', tolerance=.001, mode='enforce')
        sign(genesis, governor)
        for name, msg in [('genesis.pb', genesis), ('manifest.pb', manifest)]:
            (out / name).write_bytes(msg.SerializeToString(deterministic=True))
        app = dict(parameters=dict(chain_id=chain_id, subsidy_cap=0, unbonding_blocks=3,
            slash_multiplier=20, dispute_blocks=1, min_node_stake=100000,
            hold_timeout_blocks=30000, registration_ttl_blocks=100000, registered_auditors=True,
            evidence=dict(max_frame_tokens=512)),
            accounts=[dict(id=ident, balance=1000000, stake=100000 if role in ('producer', 'reference') else 0)
                      for role, (_, ident, _) in identities.items()],
            models=[dict(id=model_id, genesis_hash=hashlib.sha256((out/'genesis.pb').read_bytes()).hexdigest(),
                         profile_hash=hashlib.sha256((out/'manifest.pb').read_bytes()).hexdigest())],
            coordinators=[identities['coordinator'][1]], auditors=[], governors=[identities['governor'][1]])
        save('app-state.json', app)
        home = out / 'validator'
        run(chain, 'init-validator', '--home', home)
        run(chain, 'genesis', '--out', home/'config/genesis.json', '--chain-id', chain_id,
            '--app-state', out/'app-state.json', '--validators', home)
        rpc_port, p2p_port, metrics_port, http_port, grpc_port = [port() for _ in range(5)]
        rpc = f'http://127.0.0.1:{rpc_port}'
        start('validator', chain, 'start', '--home', home, '--rpc', f'tcp://127.0.0.1:{rpc_port}',
              '--p2p', f'tcp://127.0.0.1:{p2p_port}', '--metrics', f'127.0.0.1:{metrics_port}')
        def state():
            return json.loads(run(chain, 'query', '--rpc', rpc, '--path', '/state'))
        wait(lambda: state()['height'] > 0)
        token = 'evidence-isolated-test-token-for-long-evidence-staging'
        save('auth.json', [dict(token=token, key=str(identities['payer'][2]))])
        (out/'auth.json').chmod(0o600)
        start('gateway', daemon, 'all', '--ledger-backend', 'chain', '--chain-rpc', rpc,
            '--chain-id', chain_id, '--chain-coordinator-key', identities['coordinator'][2],
            '--chain-auditor-key', identities['auditor'][2], '--chain-auth-file', out/'auth.json',
            '--scheduler-key', identities['coordinator'][2], '--audit-key', identities['auditor'][2],
            '--http', f'127.0.0.1:{http_port}', '--grpc', f'127.0.0.1:{grpc_port}',
            '--insecure-localhost', '--genesis', out/'genesis.pb', '--manifest', out/'manifest.pb',
            '--engine', 'transformers', '--model-path', model, '--db', out/'gateway.db',
            '--request-timeout', '20m', '--audit-timeout', '20m', '--audit-workers', '1')
        for role in ('producer', 'reference'):
            directory = out/role
            directory.mkdir()
            shutil.copy(identities[role][2], directory/'identity.key')
            start(role, sys.executable, '-m', 'agent', 'join', '--state-dir', directory,
                '--coordinator', f'127.0.0.1:{grpc_port}', '--insecure-localhost', '--chain', model_id,
                '--engine', 'transformers', '--device', 'cuda', '--model-path', model, '--audit-role',
                '--generation-timeout', '1200',
                '--scheduler-id', identities['coordinator'][1], '--chain-rpc', rpc,
                '--chain-cli', chain, '--ledger-chain-id', chain_id, '--health-listen', '',
                '--registration-ttl', '100000', '--genesis-sha256', app['models'][0]['genesis_hash'],
                '--manifest-sha256', app['models'][0]['profile_hash'])
            wait(lambda: 'JOIN READY' in (out/(role+'.log')).read_text(), 180)
        prompt = args.prompt
        save('request.json', dict(model=model_id, messages=[dict(role='user', content=prompt)],
                                  max_tokens=2048, seed=22, stream=True))
        request = urllib.request.Request(f'http://127.0.0.1:{http_port}/v1/chat/completions',
            data=(out/'request.json').read_bytes(), headers={'Authorization':'Bearer '+token,
                                                          'Content-Type':'application/json'})
        started = time.time()
        print(f'STAGING started output={out} runtime={fingerprint}', flush=True)
        content, count = [], 0
        with urllib.request.urlopen(request, timeout=1500) as response:
            for line in response:
                if line.startswith(b'event: error'):
                    raise RuntimeError('execution stream failed; inspect gateway log')
                if not line.startswith(b'data: '):
                    continue
                if line.strip() == b'data: [DONE]':
                    break
                frame = json.loads(line[6:])
                content.append(frame['choices'][0]['delta']['content'])
                count += 1
                if count % 256 == 0:
                    print(f'GENERATED tokens={count} elapsed={time.time()-started:.1f}s', flush=True)
        result = dict(usage=dict(completion_tokens=count), content=''.join(content))
        save('response.json', result)
        assert result['usage']['completion_tokens'] >= 2048, result['usage']
        def settled():
            current = state()
            holds = [h for h in current['holds'].values() if h['state'] == 'settled']
            return current if holds else None
        final = wait(settled, 1500)
        save('final-state.json', final)
        hold = next(h for h in final['holds'].values() if h['state'] == 'settled')
        assert hold['audit_verdict'] == 'accept', hold
        m = hold['evidence_manifest']
        verdict = AuditVerdict.FromString(base64.b64decode(hold['verdict_bytes']))
        assert verdict.evidence_ref == base64.b64decode(m['merkle_root'])
        assert m['total_tokens'] >= 2048 and m['chunk_count'] > 1
        summary = dict(result='PASS', elapsed_seconds=time.time()-started, runtime=fingerprint,
            hold_id=hold['id'], output_tokens=m['total_tokens'], chunks=m['chunk_count'],
            manifest_root=verdict.evidence_ref.hex(), verdict=verdict.verdict,
            state=hold['state'], height=final['height'], app_version=final['version'])
        save('summary.json', summary)
        print(json.dumps(summary), flush=True)
    finally:
        for p in reversed(processes):
            if p.poll() is None:
                p.terminate()
        for p in reversed(processes):
            try:
                p.wait(timeout=15)
            except subprocess.TimeoutExpired:
                p.kill(); p.wait()


if __name__ == '__main__':
    main()
