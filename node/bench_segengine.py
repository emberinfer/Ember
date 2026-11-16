"""Isolated CPU/TLS stage benchmark; never uses registered/live endpoints."""
import argparse
import json
import multiprocessing as mp
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time

MODEL = '/root/models/Qwen2.5-1.5B-Instruct'


def worker(index, port, cert, key, scheduler, ready, stage_seed, genesis_raw, manifest_raw):
    import grpc
    import torch
    from concurrent.futures import ThreadPoolExecutor
    from engine.shard import Qwen2ShardEngine
    from shard_rpc import StageService
    from ember_pb2 import ShardSpec, ChainGenesis, ModelManifest
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
    from ember_pb2_grpc import add_ShardPipelineServicer_to_server
    torch.set_num_threads(2)
    spec = ShardSpec(index=index, start_layer=index*14, end_layer=(index+1)*14,
                     has_embedding=index == 0, has_lm_head=index == 1)
    engine = Qwen2ShardEngine(MODEL, spec.start_layer, spec.end_layer,
                             spec.has_embedding, spec.has_lm_head, 'cpu')
    server = grpc.server(ThreadPoolExecutor(max_workers=1), options=(('grpc.max_receive_message_length', 67108864), ('grpc.max_send_message_length', 67108864)))
    add_ShardPipelineServicer_to_server(StageService(engine, [scheduler], spec, Ed25519PrivateKey.from_private_bytes(stage_seed), ChainGenesis.FromString(genesis_raw), ModelManifest.FromString(manifest_raw)), server)
    assert server.add_secure_port(f'127.0.0.1:{port}', grpc.ssl_server_credentials(((Path(key).read_bytes(), Path(cert).read_bytes()),)))
    server.start()
    ready.set()
    server.wait_for_termination()


def main():
    import hashlib
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
    from cryptography.hazmat.primitives import serialization
    from ember_core import node_id, sign, digest
    from ember_pb2 import ChainGenesis, ModelManifest, ResourceProfile
    from transformers import AutoTokenizer
    from shard_rpc import StageClient, bytes_tensor
    parser = argparse.ArgumentParser()
    parser.add_argument('--output', required=True)
    args = parser.parse_args()
    load = Path('/proc/loadavg').read_text().strip()
    with tempfile.TemporaryDirectory(dir='.runtime/segengine') as temp:
        cert, key = f'{temp}/cert.pem', f'{temp}/key.pem'
        subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-keyout', key, '-out', cert, '-days', '1', '-subj', '/CN=localhost', '-addext', 'subjectAltName=DNS:localhost,IP:127.0.0.1'], check=True, capture_output=True)
        scheduler = Ed25519PrivateKey.generate()
        public = scheduler.public_key().public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)
        pin = hashlib.sha256(__import__('ssl').PEM_cert_to_DER_cert(Path(cert).read_text())).hexdigest()
        seed_file = f'{temp}/scheduler.key'
        Path(seed_file).write_bytes(scheduler.private_bytes(serialization.Encoding.Raw, serialization.PrivateFormat.Raw, serialization.NoEncryption()))
        Path(seed_file).chmod(0o600)
        manifest = ModelManifest(chain_id='isolated-bench', total_layers=28)
        sign(manifest, scheduler)
        genesis = ChainGenesis(chain_id=manifest.chain_id, model_manifest_root=digest(manifest), execution_profile_ver='bench')
        profiles = []
        processes, clients = [], []
        try:
            for index, port in enumerate((49711, 49712)):
                with socket.socket() as sock:
                    sock.bind(('127.0.0.1', port))
                stage_key = Ed25519PrivateKey.generate()
                stage_seed = stage_key.private_bytes(serialization.Encoding.Raw, serialization.PrivateFormat.Raw, serialization.NoEncryption())
                profile = ResourceProfile(chain_id=manifest.chain_id, manifest_root=digest(manifest), execution_profile_ver='bench',
                    shard_index=index, shard_start_layer=index*14, shard_end_layer=(index+1)*14,
                    shard_total_layers=28, shard_has_embedding=index==0, shard_has_lm_head=index==1,
                    shard_endpoint=f'127.0.0.1:{port}', shard_tls_fingerprint=bytes.fromhex(pin))
                sign(profile, stage_key); profile.node_id = node_id(profile.signer); sign(profile, stage_key)
                profiles.append(profile.SerializeToString(deterministic=True).hex())
                ready = mp.Event()
                proc = mp.Process(target=worker, args=(index, port, cert, key, node_id(public), ready, stage_seed, genesis.SerializeToString(), manifest.SerializeToString()))
                proc.start(); processes.append(proc)
                if not ready.wait(180):
                    raise RuntimeError('stage failed to start')
                clients.append(StageClient(f'127.0.0.1:{port}@{pin}', scheduler, profile=profile))
            tokenizer = AutoTokenizer.from_pretrained(MODEL, local_files_only=True)
            ids = tokenizer('请用两句话介绍长城。').input_ids
            tokens, times, topks = [], [[], []], []
            for step in range(24):
                a = clients[0].forward('bench', step, input_ids=ids)
                b = clients[1].forward('bench', step, hidden=bytes_tensor(a.hidden_bf16, a.hidden_shape, 'cpu'))
                for index, response in enumerate((a, b)):
                    times[index].append(response.server_elapsed_us/1e6)
                tokens.append(b.token_id); topks.append(list(zip(b.topk_ids, b.topk_logits)))
                ids.append(b.token_id)
                if b.token_id == tokenizer.eos_token_id:
                    break
            Path(args.output).write_text(json.dumps(dict(loadavg=load, tokens=tokens, topks=topks, stage_seconds=times, mean_seconds=[sum(t)/len(t) for t in times]), indent=2))
            # Compare interpreter/tokenizer startup through the exact same job
            # path as gateway. Signed profiles/receipts are validated normally.
            job = dict(model_path=MODEL, scheduler_key_file=seed_file,
                prompt='请用两句话介绍长城。', request_id='startup', max_tokens=1,
                timeout=120, stages=[dict(start_layer=i*14, end_layer=(i+1)*14,
                profiles=[profiles[i]]) for i in range(2)])
            import sys
            startup = []
            for resident in (False, True):
                proc = subprocess.Popen([sys.executable, '-m', 'pipeline_client'] + (['--serve'] if resident else []),
                    stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                try:
                    for index in range(2 if resident else 1):
                        job['request_id'] = f'startup-{resident}-{index}'
                        begin = time.monotonic()
                        proc.stdin.write(json.dumps(job) + '\n'); proc.stdin.flush()
                        if not resident: proc.stdin.close()
                        first = None
                        success = False
                        for line in proc.stdout:
                            event = json.loads(line)
                            if first is None and event['type'] == 'stage_start': first = time.monotonic()-begin
                            if event['type'] == 'result': success = True
                            if event['type'] == 'job_end':
                                assert event['ok'], event
                                break
                        assert success, proc.stderr.read()
                        startup.append(first)
                    if resident:
                        proc.stdin.write('{}\n'); proc.stdin.flush()
                        failed = json.loads(proc.stdout.readline())
                        assert failed['type'] == 'job_end' and not failed['ok'], failed
                        job['request_id'] = 'after-failed-job'
                        proc.stdin.write(json.dumps(job) + '\n'); proc.stdin.flush()
                        recovered = False
                        for line in proc.stdout:
                            event = json.loads(line)
                            if event['type'] == 'result': recovered = True
                            if event['type'] == 'job_end':
                                assert event['ok'] and recovered, event
                                break
                finally:
                    proc.terminate(); proc.wait()
            result = dict(startup_seconds=startup, loadavg=load, tokens=tokens, topks=topks, stage_seconds=times, mean_seconds=[sum(t)/len(t) for t in times])
            Path(args.output).write_text(json.dumps(result, indent=2))
            print(json.dumps(result))
        finally:
            for client in clients: client.close()
            for proc in processes: proc.terminate(); proc.join()


if __name__ == '__main__':
    mp.set_start_method('spawn')
    main()
