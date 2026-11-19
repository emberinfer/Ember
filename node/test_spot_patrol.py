"""Patrol uses authentic gateway evidence and the ordinary signed tx envelope."""
import base64
import hashlib
import json
from pathlib import Path
from types import SimpleNamespace as NS
from unittest.mock import patch

import pytest
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat
from ember_core import node_id
from agent import Agent
from auditor.spot_patrol import Patrol, broadcast, submit_received


def fixture(tmp_path):
    key = Ed25519PrivateKey.generate()
    gateway = Ed25519PrivateKey.generate()
    public = gateway.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)
    agent = Agent('auditor', None, key=key)
    agent.chain_args = NS(state_dir=str(tmp_path), spot_profile_config=str(tmp_path/'pins.json'),
                          spot_spool_dir=str(tmp_path/'spool'), spot_gateway_url='http://pinned',
                          chain_rpc='http://chain', ledger_chain_id='fixture')
    Path(agent.chain_args.spot_spool_dir).mkdir()
    (tmp_path/'pins.json').write_text(json.dumps(dict(gateway_public=public.hex())))
    c = dict(request_id='ab'*32, challenge_height=10, round='1', step_set=[2],
             deadline=130, auditor=agent.node_id, received=False)
    att = dict(request_id=c['request_id'], challenge_height=10,
               step_set_hash=base64.b64encode(hashlib.sha256((2).to_bytes(8,'big')).digest()).decode(),
               gateway=node_id(public), received=False, height=131)
    att['signature'] = base64.b64encode(gateway.sign(b'EMBER-SPOT-SPOOL-V1\0' +
        json.dumps(att, separators=(',', ':')).encode())).decode()
    snapshot = dict(height=131, holds={c['request_id']: dict(auditor=agent.node_id,
        settle_height=5010, pipeline=dict(spot=c))})
    return agent, snapshot, att


class Response:
    def __init__(self, value): self.value = value
    def __enter__(self): return self
    def __exit__(self, *args): pass
    def read(self): return json.dumps(self.value).encode()


def verify_envelope(agent, payload, kind):
    raw = base64.b64decode(json.loads(payload)['envelope'])
    envelope = json.loads(raw)
    signature = base64.b64decode(envelope['signature'])
    envelope['signature'] = None
    agent.key.public_key().verify(signature, b'ember/chain-tx/v1\0' +
        json.dumps(envelope, separators=(',', ':')).encode())
    assert envelope['action']['type'] == kind
    assert envelope['nonce'] == 8
    return envelope


def test_patrol_auto_sign_submit_and_persistent_dedup(tmp_path):
    agent, snapshot, att = fixture(tmp_path)
    submitted = []
    query = lambda path: snapshot
    with patch('auditor.spot_patrol.urlopen', return_value=Response(att)) as http, \
         patch('auditor.worker._fresh_audit_nonce', return_value=7):
        patrol = Patrol(agent, query, submitted.append)
        patrol.poll()
        Patrol(agent, query, submitted.append).poll()
    assert len(submitted) == 1
    envelope = verify_envelope(agent, submitted[0], 'spot_unopened')
    assert envelope['action']['spool'] == att
    assert 'challenge_height=10' in http.call_args.args[0]
    assert (tmp_path/'spot-evidence'/('ab'*32)/'unopened.json').exists()


@pytest.mark.parametrize('failure', ['unreachable', 'bad_signature', 'local_delivery', 'uncertain_submit'])
def test_patrol_no_fabrication_and_retry(tmp_path, caplog, failure):
    agent, snapshot, att = fixture(tmp_path)
    if failure == 'bad_signature': att['signature'] = base64.b64encode(bytes(64)).decode()
    if failure == 'local_delivery':
        (tmp_path/'spool'/'opening.json').write_text(json.dumps(dict(kind='opening',
            request_id='ab'*32, challenge_height=10, packages=[dict(step_index=2)])))
    submitted = []
    def submit(payload):
        if failure == 'uncertain_submit': raise TimeoutError()
        submitted.append(payload)
    with patch('auditor.spot_patrol.urlopen', side_effect=OSError() if failure == 'unreachable' else None,
               return_value=Response(att)), patch('auditor.worker._fresh_audit_nonce', return_value=7):
        patrol = Patrol(agent, lambda path: snapshot, submit)
        patrol.poll(); patrol.poll()
    assert not submitted and not patrol.done
    assert not (tmp_path/'spot-patrol.json').exists()
    assert len([r for r in caplog.records if 'unavailable' in r.message]) == 2


def test_patrol_discovers_expired_history_after_auto_resend(tmp_path):
    agent, snapshot, att = fixture(tmp_path)
    hold = snapshot['holds']['ab'*32]
    old = hold['pipeline']['spot']
    hold['pipeline']['spot'] = dict(old, challenge_height=131, deadline=251, round='2', history=[old])
    submitted = []
    with patch('auditor.spot_patrol.urlopen', return_value=Response(att)), \
         patch('auditor.worker._fresh_audit_nonce', return_value=7):
        Patrol(agent, lambda path: snapshot, submitted.append).poll()
    assert len(submitted) == 1
    assert verify_envelope(agent, submitted[0], 'spot_unopened')['action']['spool']['challenge_height'] == 10


def test_received_direct_signed_broadcast_and_rpc_rejection(tmp_path):
    agent, _, _ = fixture(tmp_path)
    accepted = dict(result=dict(check_tx=dict(code=0), tx_result=dict(code=0)))
    with patch('auditor.spot_patrol.urlopen', return_value=Response(accepted)) as http, \
         patch('auditor.worker._fresh_audit_nonce', return_value=7):
        submit_received(agent, 'ab'*32)
    request = json.loads(http.call_args.args[0].data)
    assert request['method'] == 'broadcast_tx_commit'
    verify_envelope(agent, json.dumps(dict(envelope=request['params']['tx'])), 'spot_received')
    with patch('auditor.spot_patrol.urlopen', return_value=Response(dict(result=dict(check_tx=dict(code=1))))):
        with pytest.raises(ValueError): broadcast(agent, json.dumps(dict(envelope=request['params']['tx'])))


def test_multihome_starts_one_independent_patrol(tmp_path):
    from agent.join import join
    args = NS(state_dir=str(tmp_path), coordinator=['a:1', 'b:2'], tls_fingerprint=None,
              chain_rpc='http://chain', spot_gateway_url='http://pinned', spot_spool_dir=str(tmp_path/'spool'))
    with patch('agent.join.threading.Thread') as thread, \
         patch('auditor.spot_patrol.start') as start:
        join(args)
    assert start.call_count == 1 and thread.call_count == 2
    assert thread.return_value.start.call_count == 2
    assert thread.return_value.join.call_count == 2
    assert start.call_args.args[0].chain_args is args


def test_not_due_received_and_opted_out_do_not_accuse(tmp_path):
    agent, snapshot, _ = fixture(tmp_path)
    submitted = []
    patrol = Patrol(agent, lambda path: snapshot, submitted.append)
    with patch('auditor.spot_patrol.urlopen') as http:
        snapshot['height'] = 130; patrol.poll()
        snapshot['height'] = 131; snapshot['holds']['ab'*32]['pipeline']['spot']['received'] = True
        patrol.poll()
        agent.chain_args.no_audit = True; patrol.poll()
    assert not submitted and not http.called


def test_delivery_receipt_is_verified_and_durable_before_confirmation_and_replay(tmp_path):
    from auditor.spot_worker import execute
    agent, snapshot, att = fixture(tmp_path)
    # Reuse the fixture pin, replacing it with a key held by this test gateway.
    gateway = Ed25519PrivateKey.generate()
    public = gateway.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)
    att.update(gateway=node_id(public), received=True, height=20)
    att.pop('signature')
    att['signature'] = base64.b64encode(gateway.sign(b'EMBER-SPOT-SPOOL-V1\0' +
        json.dumps(att, separators=(',', ':')).encode())).decode()
    context = dict(request_id='ab'*32, challenge_height=10, steps=[2], round='1', gateway_public=public.hex())
    delivery = dict(receipt=att, request_id='ab'*32, challenge_height=10,
                    steps=[2], height=20, packages=[dict(step_index=2)])
    events = []
    def received(*args):
        assert list((tmp_path/'spool').glob('*.json'))
        events.append('received')
    def replay(*args, **kwargs):
        events.append('replay')
        raise RuntimeError('stop after replay entry')
    with patch('auditor.spot_worker.context_for', return_value=context), \
         patch('auditor.spot_patrol.submit_received', side_effect=received), \
         patch('auditor.spot_worker.audit', side_effect=replay):
        with pytest.raises(RuntimeError, match='replay entry'):
            execute(agent, NS(request_id='ab'*32), dict(spot_delivery=delivery),
                    snapshot['holds']['ab'*32], lambda path: dict(height=20))
    assert events == ['received', 'replay']
