"""Outbound gRPC tunnel with retry and attempt-scoped replies."""
import hashlib
import json
import os
from pathlib import Path
import queue
import threading
import time

import grpc
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

from ember_core import node_id, sign, verify
from ember_pb2 import ExecutionCommitment, ExecutionReceipt, TunnelFrame
from ember_pb2_grpc import NodeTunnelStub
from evidence import canonical, chunks, encode_step, opening, manifest_chunks, DEFAULT_MAX_FRAME_TOKENS, MAX_FRAME_BYTES


class Agent:
    def __init__(self, node_id, engine, key=None, cheat=False, delay=0,
                 authorized_schedulers=(), scheduler_nonce_path=None):
        self.node_id, self.engine, self.cheat = node_id, engine, cheat
        self.delay = delay
        self.key = key or Ed25519PrivateKey.generate()
        self.sessions = {}
        self.authorized_schedulers = set(authorized_schedulers or ())
        self.scheduler_nonce_path = Path(scheduler_nonce_path) if scheduler_nonce_path else None
        self.scheduler_nonces = self._load_scheduler_nonces()
        self.ready = False
        self.started_at = time.monotonic()
        self.completed_requests = 0
        self.failed_requests = 0

    def _load_scheduler_nonces(self):
        if self.scheduler_nonce_path is None or not self.scheduler_nonce_path.exists():
            return {}
        if self.scheduler_nonce_path.stat().st_mode & 0o077:
            raise ValueError('scheduler nonce state must have mode 0600')
        values = json.loads(self.scheduler_nonce_path.read_text(encoding='ascii'))
        if not isinstance(values, dict) or any(not isinstance(k, str) or
                not isinstance(v, int) or v < 0 for k, v in values.items()):
            raise ValueError('invalid scheduler nonce state')
        return values

    def _save_scheduler_nonces(self):
        if self.scheduler_nonce_path is None:
            return
        self.scheduler_nonce_path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        temporary = self.scheduler_nonce_path.with_suffix('.tmp')
        fd = os.open(temporary, os.O_CREAT | os.O_TRUNC | os.O_WRONLY, 0o600)
        with os.fdopen(fd, 'w', encoding='ascii') as stream:
            json.dump(self.scheduler_nonces, stream, sort_keys=True,
                      separators=(',', ':'))
            stream.flush(); os.fsync(stream.fileno())
        os.replace(temporary, self.scheduler_nonce_path)
        self.scheduler_nonce_path.chmod(0o600)

    def serve(self, address, stop=None, channel_factory=None, authenticate=None):
        while stop is None or not stop.is_set():
            self.ready = False
            outgoing = queue.Queue()
            disconnected = threading.Event()
            outgoing.put(TunnelFrame(node_id=self.node_id, kind='join' if authenticate else 'hello'))
            def messages(outgoing=outgoing, disconnected=disconnected):
                # Bind this connection's queue: a cancelled gRPC iterator can
                # outlive reconnect and must never consume the replacement's frames.
                last_heartbeat = time.monotonic()
                while not disconnected.is_set() and (stop is None or not stop.is_set()):
                    if self.ready and time.monotonic() - last_heartbeat >= 30:
                        last_heartbeat = time.monotonic()
                        yield TunnelFrame(node_id=self.node_id, kind='heartbeat',
                            payload=self.profile().SerializeToString(deterministic=True))
                    try:
                        yield outgoing.get(timeout=0.5)
                    except queue.Empty:
                        pass
            try:
                with (channel_factory(address) if channel_factory else grpc.insecure_channel(address)) as channel:
                    self.evidence_stub = NodeTunnelStub(channel)
                    for frame in self.evidence_stub.Connect(messages()):
                        if authenticate and frame.kind == 'challenge':
                            outgoing.put(authenticate(frame))
                        elif frame.kind == 'ready':
                            self.ready = True
                            print(f'JOIN READY node={self.node_id}', flush=True)
                        elif frame.kind in ('run', 'audit', 'tokenize'):
                            self._run(frame, outgoing)
            except (grpc.RpcError, OSError) as exc:
                reason = exc.code().name if isinstance(exc, grpc.RpcError) else str(exc)
                print(f'TUNNEL disconnected: {reason}', flush=True)
                if stop is not None and stop.is_set():
                    break
                time.sleep(0.5)
            finally:
                disconnected.set()

    def _run(self, frame, outgoing):
        def send(kind, payload):
            outgoing.put(TunnelFrame(node_id=self.node_id, request_id=frame.request_id,
                                     attempt_id=frame.attempt_id, kind=kind, payload=payload))
        try:
            if self.authorized_schedulers:
                verify(frame)
                scheduler = node_id(frame.signer)
                previous = self.scheduler_nonces.get(scheduler, 0)
                if scheduler not in self.authorized_schedulers or frame.nonce <= previous:
                    raise ValueError('unauthorized or replayed scheduler frame')
                self.scheduler_nonces[scheduler] = frame.nonce
                self._save_scheduler_nonces()
            if getattr(self, "check_trust", None):
                self.check_trust()
            job = json.loads(frame.payload)
            if frame.kind == 'audit':
                from auditor.worker import execute
                send('audit_result', execute(self, frame, job))
                return
            prompt, seed = job['prompt'], job['seed']
            commitment = ExecutionCommitment(request_id=frame.request_id,
                chain_id=job.get('chain', 'ember-mock'), input_commitment=hashlib.sha256(prompt.encode()).digest(),
                profile_ver=job.get('profile', 'v1'), sampling_params_hash=hashlib.sha256(str(job['max_tokens']).encode()).digest(), seed=seed)
            if job.get("pricing"):
                commitment.input_tokens = self.engine.count_input_tokens(prompt)
                commitment.max_output_tokens = job["max_tokens"]
            sign(commitment, self.key)
            if frame.kind == "tokenize":
                send("tokenization", commitment.SerializeToString(deterministic=True))
                return
            steps = []
            started = time.monotonic()
            generator = self.engine.generate_for_request(prompt, seed, job['max_tokens'], frame.request_id)
            for step in generator:
                steps.append(step)
                if len(steps) == 1:
                    send('timing', str(int((time.monotonic() - started) * 1000)).encode())
                send('token', step.text.encode())
                if self.delay:
                    time.sleep(self.delay)
            if self.cheat and steps:
                steps[0].activations = {'mock.layer': b'\xff' * 32}
            roots = chunks(frame.request_id, steps, self.key)
            receipt = ExecutionReceipt(request_id=frame.request_id, node_id=self.node_id,
                commitment_ref=hashlib.sha256(commitment.SerializeToString(deterministic=True)).digest(),
                chunk_roots=roots, output_commitment=hashlib.sha256(canonical([encode_step(s) for s in steps])).digest(),
                input_tokens=commitment.input_tokens if job.get("pricing") else len(prompt.split()), output_tokens=len(steps),
                elapsed_ms=max(1, int((time.monotonic() - started) * 1000)))
            openings = [opening(s) for s in steps]
            limit = job.get('evidence_max_frame_tokens', DEFAULT_MAX_FRAME_TOKENS)
            if type(limit) is not int or limit < 1:
                raise ValueError('invalid evidence.max_frame_tokens')
            if len(steps) > limit or len(canonical(openings)) > MAX_FRAME_BYTES:
                manifest, data = manifest_chunks(openings, limit)
                receipt.evidence_manifest.CopyFrom(manifest)
            else:
                data = None
            sign(receipt, self.key)
            self.sessions[frame.request_id] = (job, steps, commitment, receipt)
            if data is None:
                send('evidence', canonical(openings))
            else:
                from ember_pb2 import EvidenceChunk
                for index, chunk in enumerate(data):
                    send('evidence_chunk', EvidenceChunk(manifest_root=manifest.merkle_root,
                         index=index, data=chunk).SerializeToString(deterministic=True))
            send('receipt', receipt.SerializeToString(deterministic=True))
            self.completed_requests += 1
        except Exception as exc:
            self.failed_requests += 1
            send('error', str(exc).encode())
