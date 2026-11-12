import hashlib
import base64
import os
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

sys.path[:0] = [str(Path(__file__).resolve().parents[1] / 'proto'), str(Path(__file__).resolve().parents[1] / 'core-py'), str(Path(__file__).resolve().parent)]
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from ember_core import verify_chunks
from ember_pb2 import Artifact, ChainGenesis, ModelManifest
from engine import MockEngine, select_device
from engine.shard import Qwen2ShardEngine
from evidence import chunks
from fetcher import fetch
from fetcher import verify_local
from shard_rpc import read_scheduler_key
from agent import Agent
from agent.join import identity, pinned_channel
from auditor import audit, confirm
from auditor.worker import ReplayMeter
from evidence import opening
from ember_pb2 import TunnelFrame, ExecutionReceipt
from ember_core import digest, node_id, sign, verify
import json
import queue
import threading
import urllib.error
import urllib.request

from agent.health import start_health_server


class NodeTest(unittest.TestCase):
    def test_node_health_readiness_and_metrics(self):
        agent = Agent('node1', MockEngine())
        server = start_health_server(agent, '127.0.0.1:0')
        base = f'http://127.0.0.1:{server.server_port}'
        try:
            self.assertEqual(urllib.request.urlopen(base + '/healthz').read(), b'ok\n')
            with self.assertRaises(urllib.error.HTTPError) as error:
                urllib.request.urlopen(base + '/readyz')
            self.assertEqual(error.exception.code, 503)
            agent.ready = True
            agent.completed_requests = 3
            agent.failed_requests = 1
            self.assertEqual(urllib.request.urlopen(base + '/readyz').read(), b'ready\n')
            metrics = urllib.request.urlopen(base + '/metrics').read().decode()
            self.assertIn('ember_node_ready 1', metrics)
            self.assertIn('result="success"} 3', metrics)
            self.assertIn('result="failure"} 1', metrics)
        finally:
            server.shutdown()
            server.server_close()

    def test_scheduler_key_requires_private_permissions(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'scheduler.key'
            path.write_bytes(b'7' * 32)
            path.chmod(0o600)
            self.assertEqual(len(read_scheduler_key(path).sign(b'test')), 64)
            path.chmod(0o644)
            with self.assertRaisesRegex(ValueError, 'mode 0600'):
                read_scheduler_key(path)

    def test_qwen_shards_match_full_model(self):
        import torch
        from transformers import Qwen2Config, Qwen2ForCausalLM

        torch.manual_seed(7)
        config = Qwen2Config(vocab_size=64, hidden_size=32,
                            intermediate_size=64, num_hidden_layers=2,
                            num_attention_heads=4, num_key_value_heads=2,
                            max_position_embeddings=32, tie_word_embeddings=True)
        model = Qwen2ForCausalLM(config).to(torch.bfloat16).eval()
        with tempfile.TemporaryDirectory() as directory:
            model.save_pretrained(directory, safe_serialization=True)
            first = Qwen2ShardEngine(directory, 0, 1, True, False, 'cpu')
            last = Qwen2ShardEngine(directory, 1, 2, False, True, 'cpu')
            ids = torch.tensor([[1, 2, 3, 4]])
            with torch.inference_mode():
                expected = model(ids, use_cache=False).logits.float()
                actual = last.forward(hidden_states=first.forward(input_ids=ids))
            self.assertTrue(torch.allclose(actual, expected, atol=2e-3, rtol=2e-3),
                            (actual - expected).abs().max().item())
            self.assertTrue(all('layers.1.' not in key for key in first.loaded_keys))
            self.assertTrue(all('layers.0.' not in key for key in last.loaded_keys))

    def test_gpu_first_device_policy(self):
        gib = 1024 ** 3
        cuda = SimpleNamespace(is_available=lambda: True)
        no_cuda = SimpleNamespace(is_available=lambda: False)
        self.assertEqual(select_device(SimpleNamespace(cuda=cuda), 'auto', 16 * gib)[0], 'cuda')
        self.assertEqual(select_device(SimpleNamespace(cuda=no_cuda), 'auto', 32 * gib)[0], 'cpu')
        self.assertEqual(select_device(SimpleNamespace(cuda=cuda), 'auto', 15 * gib)[0], 'cpu')
        with self.assertRaisesRegex(RuntimeError, 'CUDA requested'):
            select_device(SimpleNamespace(cuda=no_cuda), 'cuda', 32 * gib)
        with self.assertRaisesRegex(MemoryError, 'less than 16 GiB'):
            select_device(SimpleNamespace(cuda=cuda), 'cuda', 15 * gib)

    def test_orin_memory_admission_and_reserve(self):
        from engine.device import memory_budget, check_memory
        gib = 1024 ** 3
        torch = SimpleNamespace(cuda=SimpleNamespace(
            is_available=lambda: True, get_device_name=lambda _index: 'Orin'))
        budget = memory_budget(torch)
        self.assertEqual(budget, (7 * gib, 4 * gib, 6 * gib, 3 * gib))
        self.assertEqual(select_device(torch, 'cuda', 7 * gib)[0], 'cuda')
        with self.assertRaisesRegex(MemoryError, 'less than 7 GiB'):
            select_device(torch, 'cuda', 7 * gib - 1)
        self.assertEqual(select_device(torch, 'auto', 7 * gib - 1)[0], 'cpu')
        check_memory(budget, 6 * gib, 3 * gib)
        with self.assertRaisesRegex(MemoryError, 'RSS'):
            check_memory(budget, 6 * gib + 1, 3 * gib)
        with self.assertRaisesRegex(MemoryError, 'reserve'):
            check_memory(budget, 4 * gib, 3 * gib - 1)
        torch.cuda.get_device_name = lambda _index: 'unmeasured GPU'
        with self.assertRaisesRegex(MemoryError, 'less than 16 GiB'):
            select_device(torch, 'cuda', 7 * gib)

    def test_rejoin_reuses_loaded_model(self):
        signer = Ed25519PrivateKey.generate()
        manifest = ModelManifest(chain_id='ember-test')
        sign(manifest, signer)
        genesis = ChainGenesis(chain_id='ember-test', model_manifest_root=digest(manifest),
                               execution_profile_ver='v1')
        sign(genesis, signer)
        payload = json.dumps({
            'genesis': base64.b64encode(genesis.SerializeToString()).decode(),
            'manifest': base64.b64encode(manifest.SerializeToString()).decode(),
            'nonce': base64.b64encode(os.urandom(32)).decode(),
            'questions': ['hello'],
        }).encode()
        other_signer = Ed25519PrivateKey.generate()
        other_manifest = ModelManifest(chain_id='ember-test')
        sign(other_manifest, other_signer)
        other_genesis = ChainGenesis(chain_id='ember-test', model_manifest_root=digest(other_manifest),
                                     execution_profile_ver='v1')
        sign(other_genesis, other_signer)
        changed = json.dumps({
            'genesis': base64.b64encode(other_genesis.SerializeToString()).decode(),
            'manifest': base64.b64encode(other_manifest.SerializeToString()).decode(),
            'nonce': base64.b64encode(os.urandom(32)).decode(),
            'questions': ['hello'],
        }).encode()
        def serve(_agent, _address, channel_factory, authenticate):
            first = authenticate(TunnelFrame(payload=payload))
            second = authenticate(TunnelFrame(payload=payload))
            self.assertEqual(first.kind, 'authenticate')
            self.assertEqual(second.kind, 'authenticate')
            with self.assertRaisesRegex(ValueError, 'seed genesis hash differs'):
                authenticate(TunnelFrame(payload=changed))

        with tempfile.TemporaryDirectory() as directory:
            args = SimpleNamespace(state_dir=directory, coordinator='127.0.0.1:19191',
                                   tls_fingerprint='a' * 64, chain='ember-test', engine='transformers',
                                   model_path=directory, device='cpu', insecure_localhost=False,
                                   genesis_sha256=hashlib.sha256(genesis.SerializeToString(
                                       deterministic=True)).hexdigest(),
                                   manifest_sha256=hashlib.sha256(manifest.SerializeToString(
                                       deterministic=True)).hexdigest())
            with patch('agent.join.Agent.serve', serve), patch('agent.join.load_engine', return_value=MockEngine()) as loader:
                from agent.join import join
                join(args)
                loader.assert_called_once_with('transformers', model_path=directory, device='cpu', tamper=False)

    def test_mock_commitments(self):
        engine = MockEngine()
        a, b = list(engine.generate('hello', 7, 33)), list(engine.generate('hello', 7, 33))
        self.assertEqual(a, b)
        roots = chunks('r', a, Ed25519PrivateKey.generate())
        self.assertEqual([x.upto_step for x in roots], [16, 32, 33])
        verify_chunks(roots, 'r', 33)
        roots[0].merkle_root = b'0' * 32
        with self.assertRaises(Exception):
            verify_chunks(roots, 'r', 33)

    def test_fetch_local(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'piece').write_bytes(b'weights')
            artifact = Artifact(name='piece', size=7, sha256=hashlib.sha256(b'weights').digest())
            self.assertEqual(fetch(artifact, root / 'out', root).read_bytes(), b'weights')
            artifact.sha256 = b'0' * 32
            with self.assertRaises(ValueError):
                verify_local(artifact, root)
            with self.assertRaises(ValueError):
                fetch(artifact, root / 'bad', root)
            artifact.name = '../piece'
            with self.assertRaises(ValueError):
                fetch(artifact, root / 'bad', root)

    def test_persistent_identity(self):
        with tempfile.TemporaryDirectory() as directory:
            key, first = identity(directory)
            self.assertEqual(first, identity(directory)[1])
            self.assertEqual((Path(directory) / 'identity.key').stat().st_mode & 0o777, 0o600)
            (Path(directory) / 'identity.key').chmod(0o644)
            with self.assertRaises(ValueError):
                identity(directory)

    def test_connection_refused_during_join_does_not_crash(self):
        stop = threading.Event()
        def refuse(_address):
            stop.set()
            raise ConnectionRefusedError('coordinator restarting')
        Agent('node1', MockEngine()).serve('127.0.0.1:1', stop=stop, channel_factory=refuse)

    def test_agent_receipt(self):
        agent = Agent('node1', MockEngine())
        outgoing = queue.Queue()
        agent._run(TunnelFrame(request_id='r1', attempt_id='a1',
                 payload=json.dumps({'prompt': 'hello', 'seed': 3, 'max_tokens': 20}).encode()), outgoing)
        frames = [outgoing.get_nowait() for _ in range(outgoing.qsize())]
        self.assertEqual(len([f for f in frames if f.kind == 'token']), 20)
        receipt = ExecutionReceipt.FromString(frames[-1].payload)
        verify(receipt)
        verify_chunks(receipt.chunk_roots, 'r1', 20)
        self.assertEqual(receipt.node_id, 'node1')
        _, steps, _, _ = agent.sessions['r1']
        opened = [opening(s) for s in steps]
        replay = ReplayMeter(MockEngine())
        self.assertEqual(audit('hello', 3, receipt, opened, replay, 0)[0], 'accept')
        self.assertEqual(replay.prefix, 20)
        opened[0]['activations']['mock.layer'] = 'ff' * 32
        replay = ReplayMeter(MockEngine())
        self.assertEqual(audit('hello', 3, receipt, opened, replay, 0)[0], 'reject')
        self.assertEqual(replay.prefix, 0)  # deterministic failure did not replay

    def test_signed_scheduler_frame_replay_is_rejected(self):
        scheduler = Ed25519PrivateKey.generate()
        public = scheduler.public_key().public_bytes_raw()
        with tempfile.TemporaryDirectory() as directory:
            nonce_path = Path(directory) / 'scheduler-nonces.json'
            agent = Agent('node1', MockEngine(),
                          authorized_schedulers=[node_id(public)],
                          scheduler_nonce_path=nonce_path)
            frame = TunnelFrame(request_id='signed', attempt_id='a1', nonce=1,
                payload=json.dumps({'prompt': 'hello', 'seed': 3,
                                    'max_tokens': 1}).encode())
            sign(frame, scheduler)
            outgoing = queue.Queue()
            agent._run(frame, outgoing)
            self.assertIn('signed', agent.sessions)
            self.assertEqual(nonce_path.stat().st_mode & 0o777, 0o600)
            restarted = Agent('node1', MockEngine(),
                authorized_schedulers=[node_id(public)],
                scheduler_nonce_path=nonce_path)
            replay = queue.Queue()
            restarted._run(frame, replay)
            errors = [replay.get_nowait() for _ in range(replay.qsize())]
            self.assertEqual(errors[-1].kind, 'error')
            self.assertIn(b'replayed', errors[-1].payload)

    def test_cheating_requires_second_reference(self):
        agent = Agent('node1', MockEngine(), cheat=True)
        outgoing = queue.Queue()
        agent._run(TunnelFrame(request_id='r2', attempt_id='a2',
                 payload=json.dumps({'prompt': 'hello', 'seed': 3, 'max_tokens': 20}).encode()), outgoing)
        _, steps, _, receipt = agent.sessions['r2']
        opened = [opening(s) for s in steps]
        self.assertEqual(confirm('hello', 3, receipt, opened, MockEngine(), MockEngine(), 0)[0], 'reject')


if __name__ == '__main__':
    unittest.main()
