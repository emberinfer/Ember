"""Concurrency and routing contracts for a process with multiple gateways."""
import copy
import json
import queue
import threading
from concurrent.futures import ThreadPoolExecutor
from types import SimpleNamespace
from unittest.mock import patch

import grpc
import pytest
from agent import Agent
from agent.multihome import Claims, Registration, coordinators
from agent.network import gateway_coordinators
from ember_pb2 import TunnelFrame
from engine import MockEngine


def test_config_single_multiple_and_pins():
    assert coordinators('old:1', 'a'*64) == [('old:1', 'a'*64)]
    assert coordinators(['a:1', 'b:2']) == [('a:1', None), ('b:2', None)]
    entries = [{'endpoint': 'a:1', 'tls_fingerprint': 'a'*64},
               {'endpoint': 'b:2', 'tls_fingerprint': 'b'*64}]
    assert coordinators(json.dumps(entries)) == [('a:1', 'a'*64), ('b:2', 'b'*64)]
    for value in ([], {}, ['a:1', 'a:1'], [1]):
        with pytest.raises(ValueError):
            coordinators(value)


def test_directory_exact_profile_both_forms_and_missing():
    entry = lambda address: dict(profiles=['exact'], node_tunnel=dict(endpoint=address, tls_fingerprint='a'*64))
    access = dict(gateways=[entry('whole:1'), entry('pipeline:2')])
    assert len(gateway_coordinators(access, 'exact')) == 2
    with pytest.raises(ValueError, match='no node gateways'):
        gateway_coordinators(access, 'other')
    access['private_gateways'] = [dict(id='private', profiles=['exact'])]
    assert len(gateway_coordinators(access, 'exact')) == 2
    with pytest.raises(ValueError, match='private'):
        gateway_coordinators(access, 'exact', same_host_684=True)


def test_claims_race_and_never_evict_failure():
    claims = Claims()
    with ThreadPoolExecutor(16) as pool:
        assert sum(pool.map(lambda _: claims.claim('execution', 'same'), range(100))) == 1
    assert not claims.claim('execution', 'same')
    assert claims.claim('audit', 'same')


@pytest.mark.parametrize('kind', ['run', 'audit', 'audit_commit'])
def test_two_sessions_execute_once_and_duplicate_is_unavailable(kind, tmp_path):
    one = Agent('node', MockEngine())
    two = copy.copy(one)
    one.chain_args = two.chain_args = SimpleNamespace(no_exec=False, no_audit=False)
    q1, q2 = queue.Queue(), queue.Queue()
    frame = TunnelFrame(kind=kind, request_id='hold-or-request', attempt_id='first',
                        payload=json.dumps(dict(prompt='hi', seed=1, max_tokens=1)).encode())
    entered, release = threading.Event(), threading.Event()
    calls = []
    def execute(*args):
        calls.append(1); entered.set(); assert release.wait(5); return b'{"verdict":"accept"}'
    if kind == 'run':
        def generate(*args, **kwargs):
            execute(); return iter(())
        context = patch.object(one.engine, 'generate_for_request', side_effect=generate)
    else:
        context = patch('auditor.worker.execute', side_effect=execute)
    if kind == 'audit_commit':
        payload_path = tmp_path / 'audit.json'
        payload_path.write_bytes(frame.payload)
        one.audit_transfer = SimpleNamespace(finish=lambda _: str(payload_path))
        # Duplicate must be rejected before reassembly, even after original consumed it.
        two.audit_transfer = SimpleNamespace(finish=lambda _: pytest.fail('duplicate reassembled'))
    with context:
        t = threading.Thread(target=one._run, args=(frame,q1)); t.start()
        assert entered.wait(5)
        duplicate = copy.copy(frame); duplicate.attempt_id='second'
        two._run(duplicate,q2)
        reply=q2.get_nowait()
        assert (reply.kind,reply.request_id,reply.attempt_id)==('unavailable',frame.request_id,'second')
        release.set();t.join(5)
    assert len(calls)==1
    assert not t.is_alive()


def test_registration_leader_once_without_lock_across_network():
    registration=Registration(); entered=threading.Event(); release=threading.Event(); calls=[]
    def broadcast():
        assert registration.lock.acquire(blocking=False)
        registration.lock.release()
        calls.append(1); entered.set(); assert release.wait(5)
        return signed_profile
    signed_profile = object()
    with ThreadPoolExecutor(8) as pool:
        futures=[pool.submit(registration.ensure,broadcast) for _ in range(8)]
        assert entered.wait(5);release.set()
        for future in futures:assert future.result(5) is signed_profile
    assert registration.ensure(broadcast) is signed_profile
    assert calls==[1]


def test_failed_leader_can_be_replaced():
    registration=Registration()
    with pytest.raises(OSError):
        registration.ensure(lambda: (_ for _ in ()).throw(OSError()))
    registration.ensure(lambda: None)
    assert registration.confirmed


def test_failed_session_does_not_block_healthy_task_and_stub_is_bound():
    stop=threading.Event(); worked=threading.Event(); failures=[]; stubs=[]
    bad, good=Agent('node',MockEngine()), Agent('node',MockEngine())
    bad.claims=good.claims=Claims()
    class Channel:
        def __init__(self,target): self.target=target
        def __enter__(self):return self
        def __exit__(self,*_):return False
    class Stub:
        def __init__(self,channel):self.target=channel.target
        def Connect(self,messages):
            next(messages)
            if self.target=='bad':
                failures.append(1);raise OSError('isolated failure')
            yield TunnelFrame(kind='run',request_id='unique',payload=b'{}')
            stop.wait(3)
    def work(agent,frame,outgoing):
        stubs.append(agent.evidence_stub.target);worked.set()
    with patch('agent.NodeTunnelStub',Stub),patch.object(Agent,'_run',work):
        threads=[threading.Thread(target=a.serve,args=(target,),kwargs=dict(stop=stop,channel_factory=Channel))
                 for a,target in [(bad,'bad'),(good,'good')]]
        for t in threads:t.start()
        assert worked.wait(3)
        stop.set()
        for t in threads:t.join(3)
    assert failures and stubs==['good']
    assert all(not t.is_alive() for t in threads)


@pytest.mark.parametrize('profile,public,local', [
    ('ember-qwen2.5-1.5b-unified-v1', 3, 2),
    ('ember-qwen3.6-35b-a3b-fp8-unified-v1', 2, 1),
    ('ember-olmoe-1b-7b-moe-v1', 0, 1)])
@pytest.mark.parametrize('same_host', [False, True])
def test_real_directory_context(profile, public, local, same_host, capsys):
    from pathlib import Path
    access = json.loads((Path(__file__).parents[1]/'site/network.json').read_text())['access']
    entries = gateway_coordinators(access, profile, same_host_684=same_host)
    assert len(entries) == public + (local if same_host else 0)
    diagnostic = capsys.readouterr().err
    assert ('excluded=private-684-' in diagnostic) == (not same_host)
    assert all(len(entry['tls_fingerprint']) == 64 for entry in entries)


def test_execution_serialized_while_other_session_audit_progresses():
    one = Agent('node', MockEngine()); two = copy.copy(one)
    entered = threading.Event(); release = threading.Event(); audited = threading.Event()
    calls = []
    def generate(*args, **kwargs):
        calls.append(one.execution_lock.locked()); entered.set()
        assert release.wait(5)
        return iter(())
    def frame(ident):
        return TunnelFrame(kind='run', request_id=ident,
            payload=json.dumps(dict(prompt='hi', seed=1, max_tokens=1)).encode())
    with patch.object(one.engine, 'generate_for_request', side_effect=generate), \
         patch('auditor.worker.execute', side_effect=lambda *a: audited.set() or b'verdict'):
        first = threading.Thread(target=one._run, args=(frame('one'), queue.Queue()))
        second = threading.Thread(target=two._run, args=(frame('two'), queue.Queue()))
        first.start(); assert entered.wait(5); second.start()
        two._run(TunnelFrame(kind='audit', request_id='audit', payload=b'{}'), queue.Queue())
        assert audited.is_set() and len(calls) == 1
        release.set(); first.join(5); second.join(5)
    assert calls == [True, True] and not first.is_alive() and not second.is_alive()


def test_expired_registration_renewal_shares_leader_with_reconnect():
    from agent.join import renew_registration_once
    registration = Registration(); registration.ensure(lambda: None)
    args = SimpleNamespace(chain_cli='cli', chain_rpc='rpc')
    entered = threading.Event(); release = threading.Event(); calls = []
    def register(*args, **kwargs):
        calls.append(1); entered.set(); assert release.wait(5)
    with patch('agent.join._chain_json', return_value={'expires_height': 100}), \
         patch('agent.join._chain_height', return_value=100), \
         patch('agent.join.register_on_chain', side_effect=register):
        with ThreadPoolExecutor(2) as pool:
            renewal = pool.submit(renew_registration_once, args, None, 'id', lambda: None, registration)
            assert entered.wait(5)
            reconnect = pool.submit(registration.ensure, lambda: register())
            release.set(); renewal.result(5); reconnect.result(5)
    assert calls == [1]


def test_expiring_signed_profile_has_one_refresh_leader():
    registration = Registration()
    stale, fresh = SimpleNamespace(expires_unix=10), SimpleNamespace(expires_unix=200)
    registration.ensure(lambda: stale)
    calls = []
    def refresh():
        calls.append(1)
        return fresh
    with ThreadPoolExecutor(8) as pool:
        results = list(pool.map(lambda _: registration.ensure(refresh,
            valid=lambda profile: profile.expires_unix > 100), range(20)))
    assert all(result is fresh for result in results)
    assert calls == [1]
