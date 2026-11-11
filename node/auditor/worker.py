"""Replay on the registered reference runtime; retain custody of the audit key."""
import base64
import hashlib
import json
import math
import time

from ember_core import digest, sign
from ember_pb2 import ExecutionReceipt, AuditChallenge, AuditVerdict, ChainGenesis
from auditor import audit
from auditor.policy import load_job_policy, select_band, profile_engine_id, grade


def execute(agent, frame, job):
    from agent.join import _chain_json
    args = agent.chain_args
    if not args.audit_role or not args.chain_rpc:
        raise ValueError('registered audit role required')
    if bytes.fromhex(job['genesis']) != agent.genesis_bytes:
        raise ValueError('audit genesis differs from pinned join anchor')
    state = _chain_json(args.chain_cli, args.chain_rpc, '/state')
    for _ in range(20):
        hold = state['holds'].get(frame.request_id, {})
        if hold.get('auditor') == agent.node_id:
            break
        time.sleep(0.25)
        state = _chain_json(args.chain_cli, args.chain_rpc, '/state')
    if hold.get('auditor') != agent.node_id or hold['state'] != 'dispute':
        raise ValueError('not the finalized assigned auditor')
    receipt = ExecutionReceipt.FromString(bytes.fromhex(job['receipt']))
    if receipt.request_id != frame.request_id or digest(receipt).hex() != hold['receipt_ref']:
        raise ValueError('receipt differs from finalized hold')
    registered_profile = hold.get('producer_profile') or state['nodes'][hold['node']]['profile']
    if bytes.fromhex(job['producer_profile']) != base64.b64decode(registered_profile):
        raise ValueError('producer differs from finalized registration')
    policy, producer, policy_ref = load_job_policy(job, receipt)
    if policy is None:
        raise ValueError('registered replay requires signed numeric policy')
    band = select_band(policy, profile_engine_id(producer), producer.actual_device, agent.engine)
    if band is None or band.mode != 'enforce':
        raise ValueError('no enforced reference route for runtime')
    genesis = ChainGenesis.FromString(bytes.fromhex(job["genesis"]))
    comparison = dict(metric=policy.metric, required_layers=policy.layers, required_top_k=policy.top_k,
        verify_input_tokens=bool(genesis.price_input or genesis.price_context))
    verdict, distance, _ = audit(job['prompt'], job['seed'], receipt, job['openings'],
                                agent.engine, band.tolerance, **comparison)
    verdict, result_grade = grade(verdict, distance, band)
    # A mismatch requires another independently assigned reference. Return reject;
    # the chain chooses the second identity rather than replaying twice locally.
    if verdict == 'dispute':
        verdict = 'reject'
    challenge = AuditChallenge(receipt_ref=digest(receipt),
        vrf_proof=bytes.fromhex(hold['audit_proof']))
    sign(challenge, agent.key)
    signed = AuditVerdict(challenge_ref=digest(challenge), auditor_id=agent.node_id,
        verdict=verdict, evidence_ref=hashlib.sha256(json.dumps(job['openings'],
            separators=(',', ':')).encode()).digest(), audit_policy_ref=bytes.fromhex(policy_ref),
        distances=[distance if math.isfinite(distance) else 1e30])
    sign(signed, agent.key)
    public = bytes(signed.signer)
    action = dict(type='audit', hold_id=frame.request_id,
        challenge=base64.b64encode(challenge.SerializeToString(deterministic=True)).decode(),
        verdict=base64.b64encode(signed.SerializeToString(deterministic=True)).decode())
    envelope = dict(chain_id=args.ledger_chain_id, public_key=base64.b64encode(public).decode(),
        nonce=state['accounts'][agent.node_id]['nonce'] + 1, action=action, signature=None)
    raw = json.dumps(envelope, separators=(',', ':'), ensure_ascii=False).encode()
    envelope['signature'] = base64.b64encode(agent.key.sign(b'ember/chain-tx/v1\x00' + raw)).decode()
    return json.dumps(dict(envelope=base64.b64encode(json.dumps(envelope,
        separators=(',', ':')).encode()).decode(), verdict=verdict, distance=distance,
        grade=result_grade)).encode()
