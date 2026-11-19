"""Session-local audit engine view; transport and chain queries stay unlocked."""
import copy
import logging
import time
from contextlib import contextmanager


class AuditEngine:
    # These methods consume shared inference state. Iterators must be exhausted
    # before releasing the lock, rather than merely created under it.
    methods = frozenset(('replay', 'replay_boundaries', 'generate', 'score_forced',
                         'count_input_tokens', 'thinking_prompt'))

    def __init__(self, agent, request_id):
        self.agent = agent
        self.request_id = request_id
        self.range_locked = False

    @contextmanager
    def locked(self):
        start = time.monotonic()
        logger = logging.getLogger(__name__)
        logger.warning('audit request=%s waiting for engine lock', self.request_id)
        self.agent.execution_lock.acquire()
        logger.warning('audit request=%s acquired engine lock wait_seconds=%.6f',
                    self.request_id, time.monotonic() - start)
        try:
            yield
        finally:
            self.agent.execution_lock.release()

    @property
    def __class__(self):
        return self.agent.engine.__class__

    def __getattr__(self, name):
        if name in ('model', 'forward', 'lm_head', 'norm') and not self.range_locked:
            # Range replay constructs a shard view of the same weights and
            # bypasses engine methods. Retain its lease through local grading
            # and signing; no network operations follow this boundary.
            self.range_lease = self.locked()
            self.range_lease.__enter__()
            self.range_locked = True
        value = getattr(self.agent.engine, name)
        if name not in self.methods:
            return value
        def call(*args, **kwargs):
            with self.locked():
                result = value(*args, **kwargs)
                return list(result) if name in ('generate', 'score_forced') else result
        return call

    def replay_spot(self, opening, operation):
        """Lease shared weights for one CPU step; spool and chain I/O stay free."""
        from auditor.spot_replay import replay_step
        with self.locked():
            return replay_step(self.agent.engine, opening, operation)

    def close(self):
        if self.range_locked:
            self.range_lease.__exit__(None, None, None)
            self.range_locked = False


def execute(agent, frame, job):
    from auditor.worker import execute as worker_execute
    view = copy.copy(agent)
    engine = AuditEngine(agent, frame.request_id)
    view.engine = engine
    try:
        return worker_execute(view, frame, job)
    finally:
        engine.close()
