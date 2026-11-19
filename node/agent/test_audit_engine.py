"""Real handler claims and shared mock inference under audit contention."""
import copy
import json
import queue
import threading
from unittest.mock import patch

import pytest
from agent import Agent
from agent.audit_engine import AuditEngine
from ember_pb2 import TunnelFrame
from engine import MockEngine


@pytest.mark.parametrize('kind', ['audit', 'audit_commit'])
def test_audit_waits_after_claim_and_duplicate_never_replays(kind, caplog, tmp_path):
    caplog.set_level('INFO')
    one = Agent('node', MockEngine())
    two = copy.copy(one)
    entered, release, audited = (threading.Event() for _ in range(3))
    original = one.engine.generate_for_request
    def generate(*args, **kwargs):
        entered.set()
        assert release.wait(5)
        return original(*args, **kwargs)
    run = TunnelFrame(kind='run', request_id='execution', payload=json.dumps(
        dict(prompt='hello', seed=1, max_tokens=2)).encode())
    audit = TunnelFrame(kind=kind, request_id='hold', payload=b'{}')
    payload_path = tmp_path / 'audit.json'
    payload_path.write_bytes(b'{}')
    two.audit_transfer = type('Transfer', (), {'finish': lambda self, frame: str(payload_path)})()
    q1, q2, duplicate = queue.Queue(), queue.Queue(), queue.Queue()
    def worker(view, frame, job):
        # Exercise the actual engine boundary used by the audit worker.
        result = view.engine.replay('hello', 1, [1, 2])
        audited.set()
        assert len(result) == 2
        return b'{"verdict":"accept"}'
    with patch.object(one.engine, 'generate_for_request', side_effect=generate), \
         patch('auditor.worker.execute', side_effect=worker):
        first = threading.Thread(target=one._run, args=(run, q1))
        second = threading.Thread(target=two._run, args=(audit, q2))
        try:
            first.start(); assert entered.wait(5)
            second.start()
            import time
            deadline = time.monotonic() + 3
            while 'waiting for engine lock' not in caplog.text and time.monotonic() < deadline:
                time.sleep(.01)
            assert 'waiting for engine lock' in caplog.text
            one._run(audit, duplicate)
            assert duplicate.get_nowait().kind == 'unavailable'
            assert not audited.is_set()
        finally:
            release.set(); first.join(5); second.join(5)
    assert not first.is_alive() and not second.is_alive() and audited.is_set()
    replies = list(q1.queue)
    assert len([r for r in replies if r.kind == 'token']) == 2
    assert replies[-1].kind == 'receipt'
    assert q2.get_nowait().payload == b'{"verdict":"accept"}'
    assert 'wait_seconds=' in caplog.text


def test_lazy_iterator_exception_releases_lock():
    agent = Agent('node', MockEngine())
    def broken(*args):
        yield 1
        raise ValueError('inference failed')
    with patch.object(agent.engine, 'generate', side_effect=broken):
        with pytest.raises(ValueError):
            AuditEngine(agent, 'hold').generate('hi', 1, 2)
    assert not agent.execution_lock.locked()


def test_range_weight_lease_released_even_when_worker_fails():
    from agent.audit_engine import execute
    agent = Agent('node', MockEngine())
    agent.engine.model = object()
    def worker(view, frame, job):
        # The range worker queries first, then accesses shared weights directly.
        assert not agent.execution_lock.locked()
        assert view.engine.model is agent.engine.model
        assert agent.execution_lock.locked()
        raise ValueError('range replay failed')
    with patch('auditor.worker.execute', side_effect=worker):
        with pytest.raises(ValueError, match='range replay failed'):
            execute(agent, TunnelFrame(request_id='range'), {})
    assert not agent.execution_lock.locked()


def test_audit_metadata_and_transport_preparation_do_not_wait_for_engine():
    from agent.audit_engine import execute
    agent = Agent('node', MockEngine())
    def worker(view, frame, job):
        assert view.engine.device == agent.engine.device
        assert view.engine.capabilities() == agent.engine.capabilities()
        return b'unavailable evidence; no replay'
    with agent.execution_lock, patch('auditor.worker.execute', side_effect=worker):
        assert execute(agent, TunnelFrame(request_id='transport'), {}) == b'unavailable evidence; no replay'
