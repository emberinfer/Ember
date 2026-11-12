"""Replay on the registered reference runtime; retain custody of the audit key."""
import base64
import hashlib
import json
import math
import time

from ember_core import digest, sign
from ember_pb2 import ExecutionReceipt, AuditChallenge, AuditVerdict, ChainGenesis, EvidenceChunk, TunnelFrame
from auditor import audit
from auditor.grading import offense_grade
from evidence import fetch_openings, DEFAULT_MAX_FRAME_TOKENS
from auditor.policy import load_job_policy, select_band, profile_engine_id, grade


class ReplayMeter:
    """Count only the forced-prefix results actually produced for this verdict."""
    def __init__(self, engine):
        self.engine = engine
        self.prefix = 0

    def __getattr__(self, name):
        return getattr(self.engine, name)

    def replay(self, prompt, seed, tokens):
        result = self.engine.replay(prompt, seed, tokens)
        self.prefix = min(len(tokens), len(result))
        return result


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
    replay = ReplayMeter(agent.engine)
    reason = "evidence unavailable"
    evidence_ref = None
    unavailable = False
    if receipt.HasField('evidence_manifest'):
        evidence_ref = receipt.evidence_manifest.merkle_root
        anchored = hold.get('evidence_manifest', {})
        if base64.b64decode(anchored.get('merkle_root', '')) != evidence_ref:
            raise ValueError('evidence manifest differs from finalized hold')
        def fetch(root, index):
            query = EvidenceChunk(manifest_root=root, index=index)
            request = TunnelFrame(node_id=agent.node_id, request_id=frame.request_id,
                kind='fetch_evidence', nonce=time.time_ns(),
                payload=query.SerializeToString(deterministic=True))
            sign(request, agent.key)
            response = agent.evidence_stub.FetchEvidence(request, timeout=30)
            if response.manifest_root != root or response.index != index:
                raise ValueError('evidence response does not match query')
            return response.data
        try:
            limit = state['parameters'].get('evidence', {}).get('max_frame_tokens', 0) or DEFAULT_MAX_FRAME_TOKENS
            job['openings'] = fetch_openings(receipt, fetch, limit)
        except Exception:
            # Transport/integrity failure is service failure, never executor fraud.
            unavailable = True
    if unavailable:
        verdict, distance, result_grade = 'inconclusive', 1e30, 'evidence-unavailable'
    else:
        verdict, distance, reason = audit(job['prompt'], job['seed'], receipt, job['openings'],
                                    replay, band.tolerance, **comparison)
        verdict, result_grade = grade(verdict, distance, band)
    # A mismatch requires another independently assigned reference. Return reject;
    # the chain chooses the second identity rather than replaying twice locally.
    verdict, offense = offense_grade(verdict, distance, reason, band, hold)
    challenge = AuditChallenge(receipt_ref=digest(receipt), positions=list(range(replay.prefix)),
        vrf_proof=bytes.fromhex(hold['audit_proof']))
    sign(challenge, agent.key)
    signed = AuditVerdict(n_out_billed=replay.prefix, challenge_ref=digest(challenge), auditor_id=agent.node_id,
        verdict=verdict, evidence_ref=evidence_ref or hashlib.sha256(json.dumps(job['openings'],
            separators=(',', ':')).encode()).digest(), audit_policy_ref=bytes.fromhex(policy_ref),
        distances=[distance if math.isfinite(distance) else 1e30])
    if offense is not None:
        signed.offense.CopyFrom(offense)
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
