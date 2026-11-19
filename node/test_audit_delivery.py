"""Completed audit retries re-sign without claiming or replaying a second task."""
import base64
import copy
import json
import queue
from types import SimpleNamespace
from unittest.mock import patch

from agent import Agent
from auditor.binding import signed_envelope
from ember_pb2 import TunnelFrame
from engine import MockEngine
from cryptography.hazmat.primitives import serialization


def test_cached_commit_resigns_on_arriving_session_with_fresh_nonce(tmp_path):
    one = Agent('auditor', MockEngine())
    two = copy.copy(one)
    one.chain_args = two.chain_args = SimpleNamespace(ledger_chain_id='test', chain_cli='stub', chain_rpc='stub')
    public = one.key.public_key().public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)
    actions = [dict(type='adjudication_commit', hold_id='hold'), dict(type='audit', hold_id='hold')]
    original = json.dumps(dict(verdict='accept', pre_envelope=signed_envelope(one, actions[0], 1, public),
                               envelope=signed_envelope(one, actions[1], 2, public))).encode()
    path = tmp_path / 'job.json'
    path.write_text('{}')
    one.audit_transfer = SimpleNamespace(finish=lambda _: str(path))
    two.audit_transfer = SimpleNamespace(finish=lambda _: (_ for _ in ()).throw(AssertionError('retry reassembled')))
    first, second = queue.Queue(), queue.Queue()
    with patch('auditor.worker.execute', return_value=original) as replay:
        one._run(TunnelFrame(kind='audit_commit', request_id='hold', attempt_id='first'), first)
        assert first.get_nowait().kind == 'audit_result'
        with patch('agent.join._chain_json', return_value=dict(nonce=40)) as query:
            two._run(TunnelFrame(kind='audit_commit', request_id='hold', attempt_id='second'), second)
        query.assert_called_once_with('stub', 'stub', '/account/auditor')
        assert replay.call_count == 1
    reply = second.get_nowait()
    assert (reply.kind, reply.request_id, reply.attempt_id) == ('audit_result', 'hold', 'second')
    assert first.empty() and second.empty()
    result = json.loads(reply.payload)
    for offset, name in enumerate(('pre_envelope', 'envelope'), 1):
        envelope = json.loads(base64.b64decode(result[name]))
        assert envelope['nonce'] == 40 + offset
        assert envelope['action'] == actions[offset - 1]
        signature = base64.b64decode(envelope['signature'])
        envelope['signature'] = None
        one.key.public_key().verify(signature, b'ember/chain-tx/v1\x00' + json.dumps(
            envelope, separators=(',', ':'), ensure_ascii=False).encode())
    assert not path.exists()
    assert not one.claims.claim('audit', 'hold')
    assert one.audit_verdicts is two.audit_verdicts
    assert b'signature' not in one.audit_verdicts['hold'][1][0][1]


def test_completed_verdict_cache_bound_does_not_allow_evicted_replay(tmp_path):
    agent = Agent('auditor', MockEngine())
    with patch('auditor.worker.execute', return_value=b'{"verdict":"accept"}') as replay:
        for index in range(17):
            path = tmp_path / f'{index}.json'
            path.write_text('{}')
            agent.audit_transfer = SimpleNamespace(finish=lambda _, path=path: str(path))
            outgoing = queue.Queue()
            agent._run(TunnelFrame(kind='audit_commit', request_id=str(index)), outgoing)
            assert outgoing.get_nowait().kind == 'audit_result'
        assert len(agent.audit_verdicts) == 16 and '0' not in agent.audit_verdicts
        outgoing = queue.Queue()
        agent._run(TunnelFrame(kind='audit_commit', request_id='0'), outgoing)
        assert outgoing.get_nowait().kind == 'unavailable'
        assert replay.call_count == 17


def test_failed_commit_releases_claim_for_retransmission(tmp_path):
    agent = Agent('auditor', MockEngine())
    path = tmp_path / 'job.json'
    path.write_text('{}')
    agent.audit_transfer = SimpleNamespace(finish=lambda _: str(path))
    outgoing = queue.Queue()
    with patch('auditor.worker.execute',
               side_effect=ValueError('spot context derivation failed')):
        agent._run(TunnelFrame(kind='audit_commit', request_id='hold'), outgoing)
    assert outgoing.get_nowait().kind == 'error'
    # No verdict was produced: ownership marks are rolled back, so the
    # gateway retransmission is processed instead of duplicate-blocked.
    assert 'hold' not in agent.audit_commits
    path.write_text('{}')
    outgoing = queue.Queue()
    with patch('auditor.worker.execute', return_value=b'{"verdict":"accept"}') as replay:
        agent._run(TunnelFrame(kind='audit_commit', request_id='hold'), outgoing)
    assert outgoing.get_nowait().kind == 'audit_result'
    assert replay.call_count == 1
    # A completed verdict keeps its claim; later retries take the cache path.
    assert not agent.claims.claim('audit', 'hold')


def test_failed_spotcheck_releases_tuple_claim(tmp_path):
    agent = Agent('auditor', MockEngine())
    job = json.dumps(dict(kind='spotcheck', mode='full',
                          spot_delivery=dict(challenge_height=10, round='first'))).encode()
    task_id = ('req', 10, 'first', 'full')
    outgoing = queue.Queue()
    with patch('auditor.worker.execute',
               side_effect=ValueError('spot context derivation failed')):
        agent._run(TunnelFrame(kind='audit', request_id='req', payload=job), outgoing)
    assert outgoing.get_nowait().kind == 'error'
    assert agent.claims.claim('audit', task_id)
