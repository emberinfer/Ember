"""Outbound gRPC tunnel with retry and attempt-scoped replies."""
import hashlib
import copy
import logging
from collections import OrderedDict
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
from .audit_transfer import AuditTransfer
from evidence import canonical, chunks, encode_step, opening, manifest_chunks, DEFAULT_MAX_FRAME_TOKENS, MAX_FRAME_BYTES


class Agent:
    def __init__(self, node_id, engine, key=None, cheat=False, delay=0,
                 authorized_schedulers=(), scheduler_nonce_path=None):
        self.node_id, self.engine, self.cheat = node_id, engine, cheat
        self.delay = delay
        self.key = key or Ed25519PrivateKey.generate()
        self.sessions = {}
        self.audit_transfer = AuditTransfer()
        self.audit_verdicts = OrderedDict()
        self.audit_verdicts_lock = threading.Lock()
        self.audit_commits = set()
        self.audit_task_ids = {}
        self.authorized_schedulers = set(authorized_schedulers or ())
        self.scheduler_nonce_path = Path(scheduler_nonce_path) if scheduler_nonce_path else None
        self.scheduler_nonces = self._load_scheduler_nonces()
        from .multihome import Claims
        self.claims = Claims()
        self.scheduler_lock = threading.Lock()
        self.execution_lock = threading.Lock()
        self.ready = False
        self.started_at = time.monotonic()
        self.completed_requests = 0
        self.failed_requests = 0
        self._forward_traces = {} if os.environ.get("EMBER_FORWARD_TRACE") == "1" else None

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
            if getattr(self, 'session_status', None):
                self.session_status(False)
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
                        print(f'HEARTBEAT node={self.node_id} peer={address}', flush=True)
                        yield TunnelFrame(node_id=self.node_id, kind='heartbeat',
                            payload=self.profile().SerializeToString(deterministic=True))
                    try:
                        message = outgoing.get(timeout=0.5)
                        if self._forward_traces is not None:
                            trace = self._forward_traces.get(message.request_id)
                            if trace is not None:
                                if message.kind == 'token':
                                    trace.dequeue()
                                elif message.kind in ('receipt', 'error'):
                                    trace.finish(message.kind)
                                    self._forward_traces.pop(message.request_id, None)
                        yield message
                    except queue.Empty:
                        pass
            try:
                with (channel_factory(address) if channel_factory else grpc.insecure_channel(address)) as channel:
                    self.evidence_stub = NodeTunnelStub(channel)
                    # A worker retains this exact connection's stub even if
                    # its parent session reconnects before FetchEvidence ends.
                    connection = copy.copy(self)
                    connection.audit_transfer = AuditTransfer()
                    for frame in self.evidence_stub.Connect(messages()):
                        if authenticate and frame.kind == 'challenge':
                            outgoing.put(authenticate(frame))
                        elif frame.kind == 'ready':
                            self.ready = True
                            print(f'JOIN READY node={self.node_id} peer={address}', flush=True)
                            if getattr(self, 'session_status', None):
                                self.session_status(True)
                        elif frame.kind in ('run', 'audit', 'tokenize', 'audit_begin', 'audit_chunk', 'audit_commit'):
                            # Refresh authenticated runtime fields, not stub.
                            for name in ('engine', 'contribution', 'chain_args', 'genesis_bytes', 'check_trust', 'content_policy'):
                                if hasattr(self, name):
                                    setattr(connection, name, getattr(self, name))
                            connection._run(frame, outgoing)
            except Exception as exc:
                reason = exc.code().name if isinstance(exc, grpc.RpcError) else str(exc)
                print(f'TUNNEL disconnected peer={address} reconnecting: {reason}', flush=True)
                if stop is not None and stop.is_set():
                    break
                time.sleep(0.5)
            finally:
                self.ready = False
                if getattr(self, 'session_status', None):
                    self.session_status(False)
                disconnected.set()
                if 'connection' in locals():
                    connection.audit_transfer.close()
                self.audit_transfer.close()
                if self._forward_traces is not None:
                    for request_id in list(self._forward_traces):
                        trace = self._forward_traces.pop(request_id, None)
                        if trace is not None:
                            trace.finish('disconnected')

    @staticmethod
    def _log_audit_result(request_id, payload):
        result = json.loads(payload)
        summary = {k: v for k, v in result.items() if k not in ('envelope', 'pre_envelope')}
        logging.getLogger(__name__).warning('audit_result request_id=%s summary=%s',
                                          request_id, json.dumps(summary, sort_keys=True))

    def _run(self, frame, outgoing):
        trace = None
        if self._forward_traces is not None and frame.kind == 'run':
            from .forward_trace import ForwardTrace
            trace = ForwardTrace(frame.request_id)
            self._forward_traces[frame.request_id] = trace
        def send(kind, payload):
            if trace is not None and kind == 'token':
                trace.enqueue()
            outgoing.put(TunnelFrame(node_id=self.node_id, request_id=frame.request_id,
                                     attempt_id=frame.attempt_id, kind=kind, payload=payload))
        acquired = False
        executing = False
        try:
            if self.authorized_schedulers:
                verify(frame)
                scheduler = node_id(frame.signer)
                with self.scheduler_lock:
                    previous = self.scheduler_nonces.get(scheduler, 0)
                    if scheduler not in self.authorized_schedulers or frame.nonce <= previous:
                        raise ValueError('unauthorized or replayed scheduler frame')
                    self.scheduler_nonces[scheduler] = frame.nonce
                    self._save_scheduler_nonces()
            if getattr(self, "check_trust", None):
                self.check_trust()
            args = getattr(self, 'chain_args', None)
            if frame.kind not in ('audit', 'audit_begin', 'audit_chunk', 'audit_commit') and args is not None and getattr(args, 'no_exec', False):
                send('unavailable', b'execution opted out')
                return
            if frame.kind in ('audit_begin', 'audit_chunk', 'audit_commit') and args is not None and getattr(args, 'no_audit', False):
                raise ValueError('audit opted out')
            if frame.kind in ('audit_begin', 'audit_chunk'):
                ack = self.audit_transfer.accept(frame)
                send('audit_chunk_ack', ack)
                return
            if frame.kind == 'audit_commit':
                transfer_id = json.loads(frame.payload or b'{}').get('sha256')
                with self.audit_verdicts_lock:
                    previous_task = self.audit_task_ids.get(transfer_id, frame.request_id)
                    cached = self.audit_verdicts.get(previous_task)
                    committed = previous_task in self.audit_commits
                if cached is not None:
                    from auditor.worker import resend_verdict
                    result = resend_verdict(self, cached)
                    send('audit_result', result)
                    self._log_audit_result(frame.request_id, result)
                    return
                if committed:
                    send('unavailable', b'duplicate task: already claimed in this process')
                    return
            # Parse a completed transfer before choosing task ownership. Spot
            # rounds share a hold ID but are distinct finalized challenges.
            if frame.kind == 'audit_commit':
                payload_path = self.audit_transfer.finish(frame)
                try:
                    with open(payload_path, encoding='utf-8') as payload_file:
                        job = json.load(payload_file)
                finally:
                    os.unlink(payload_path)
            else:
                job = json.loads(frame.payload)
            task_id = frame.request_id
            if frame.kind in ('audit', 'audit_commit') and job.get('kind') == 'spotcheck':
                delivery = job.get('spot_delivery') or {}
                task_id = (frame.request_id, delivery.get('challenge_height'), delivery.get('round'), job.get('mode'))
            if frame.kind == 'audit_commit' or (frame.kind == 'audit' and job.get('kind') == 'spotcheck'):
                with self.audit_verdicts_lock:
                    cached = self.audit_verdicts.get(task_id)
                if cached is not None:
                    from auditor.worker import resend_verdict
                    result = resend_verdict(self, cached)
                    send('audit_result', result)
                    self._log_audit_result(frame.request_id, result)
                    return
            if frame.kind in ('run', 'audit', 'audit_commit'):
                kind = 'audit' if frame.kind in ('audit', 'audit_commit') else 'execution'
                if not self.claims.claim(kind, task_id):
                    send('unavailable', b'duplicate task: already claimed in this process')
                    return
            if frame.kind in ('run', 'tokenize'):
                self.execution_lock.acquire()
                executing = True
            if frame.kind == 'audit_commit':
                with self.audit_verdicts_lock:
                    if task_id in self.audit_commits:
                        send('unavailable', b'duplicate task: already claimed in this process')
                        return
                    self.audit_commits.add(task_id)
                    if job.get('kind') == 'spotcheck':
                        self.audit_task_ids[transfer_id] = task_id
            contribution = getattr(self, 'contribution', None)
            if contribution is not None and frame.kind != 'tokenize':
                acquired = contribution.acquire()
                if not acquired:
                    send('unavailable', b'contribution cap or qualification; no work accepted')
                    return
            if frame.kind in ('audit', 'audit_commit'):
                from .audit_engine import execute
                result = execute(self, frame, job)
                if frame.kind == 'audit_commit' or job.get('kind') == 'spotcheck':
                    from auditor.worker import cache_verdict
                    cached = cache_verdict(result)
                    with self.audit_verdicts_lock:
                        self.audit_verdicts[task_id] = cached
                        while len(self.audit_verdicts) > 16:
                            self.audit_verdicts.popitem(last=False)
                send('audit_result', result)
                self._log_audit_result(frame.request_id, result)
                return
            prompt, seed = job['prompt'], job['seed']
            from reasoning import prepare
            prepared_prompt = prepare(self.engine, prompt, job)
            commitment = ExecutionCommitment(request_id=frame.request_id,
                chain_id=job.get('chain', 'ember-mock'), input_commitment=hashlib.sha256(prompt.encode()).digest(),
                profile_ver=job.get('profile', 'v1'), sampling_params_hash=__import__('reasoning').sampling_hash(job), seed=seed)
            if 'thinking_enabled' in job:
                commitment.sampling_params_version = 2
                commitment.thinking_enabled = job['thinking_enabled']
                commitment.thinking_budget = job.get('thinking_budget', 0)
                commitment.answer_budget = job['answer_budget']
            if job.get("pricing"):
                from tokenization import count
                commitment.input_tokens, commitment.tokenizer_version = count(self.engine,prepared_prompt,job.get("tokenizer_version"))
                commitment.max_output_tokens = job["max_tokens"]
            else:
                from tokenization import count
                count(self.engine,prepared_prompt,job.get("tokenizer_version"))
            sign(commitment, self.key)
            if frame.kind == "tokenize":
                send("tokenization", commitment.SerializeToString(deterministic=True))
                return
            steps = []
            started = time.monotonic()
            from content_policy import refusal_steps
            refusal = refusal_steps(getattr(self, 'content_policy', None), prompt)
            if refusal is not None and len(refusal) > job['max_tokens']:
                raise ValueError('max_tokens cannot fit profile refusal')
            from reasoning import prepare, Budget
            generation_prompt = prepared_prompt
            budget = Budget(job)
            thinking_kwargs = {}
            if 'thinking_enabled' in job:
                thinking_kwargs = dict(enable_thinking=job['thinking_enabled'],
                                       thinking_budget=job.get('thinking_budget', 0),
                                       answer_budget=job.get('answer_budget', 0))
            generator = (iter(refusal) if refusal is not None else
                         self.engine.generate_for_request(generation_prompt, seed, job['max_tokens'], frame.request_id, **thinking_kwargs))
            for step in generator:
                if trace is not None:
                    trace.engine_token()
                budget.add(step.text)
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
                reasoning_tokens=budget.reasoning, content_tokens=budget.content,
                commitment_ref=hashlib.sha256(commitment.SerializeToString(deterministic=True)).digest(),
                chunk_roots=roots, output_commitment=hashlib.sha256(canonical([encode_step(s) for s in steps])).digest(),
                input_tokens=commitment.input_tokens if job.get("pricing") else len(prompt.split()), output_tokens=len(steps),
                output_category=('policy-refusal' if refusal is not None else
                                 'thinking-truncated' if getattr(self.engine, 'thinking_truncated', False) else ''),
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
        finally:
            if executing:
                self.execution_lock.release()
            if acquired:
                self.contribution.release()
