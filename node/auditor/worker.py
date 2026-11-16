"""Replay on the registered reference runtime; retain custody of the audit key."""
import base64
import hashlib
import json
import math
import logging
import os
import time

from ember_core import digest, sign
from ember_pb2 import ExecutionCommitment, ExecutionReceipt, AuditChallenge, AuditVerdict, ChainGenesis, ModelManifest, EvidenceChunk, TunnelFrame
from auditor import audit
from auditor.grading import offense_grade
from evidence import fetch_openings, DEFAULT_MAX_FRAME_TOKENS
from auditor.policy import load_job_policy, select_band, profile_engine_id, grade


class ReplayMeter:
    """Count only the forced-prefix results actually produced for this verdict."""
    def __init__(self, engine):
        self.engine = engine
        self.prefix = 0
        self.input_count = None

    def __getattr__(self, name):
        return getattr(self.engine, name)

    def count_input_tokens(self,prompt):
        self.input_count = self.engine.count_input_tokens(prompt)
        return self.input_count

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
    if job.get("kind") == "pipeline" and job.get("drawn_stage") is not None:
        return execute_range(agent, frame, job, state, hold)
    receipt = ExecutionReceipt.FromString(bytes.fromhex(job['receipt']))
    if receipt.request_id != frame.request_id or digest(receipt).hex() != hold['receipt_ref']:
        raise ValueError('receipt differs from finalized hold')
    registered_profile = hold.get('producer_profile') or state['nodes'][hold['node']]['profile']
    if bytes.fromhex(job['producer_profile']) != base64.b64decode(registered_profile):
        raise ValueError('producer differs from finalized registration')
    pinned = None
    equality = state['parameters'].get('audit_equality', {})
    equality_enabled = equality.get('enabled', False)
    policy_ref = ''
    band = None
    if not equality_enabled:
        policy, producer, policy_ref = load_job_policy(job, receipt)
        if policy is None:
            raise ValueError('registered replay requires signed numeric policy')
        band = select_band(policy, profile_engine_id(producer), producer.actual_device, agent.engine)
        if band is None or band.mode != 'enforce':
            raise ValueError('no enforced reference route for runtime')
        genesis = ChainGenesis.FromString(bytes.fromhex(job["genesis"]))
        comparison = dict(metric=policy.metric, required_layers=policy.layers, required_top_k=policy.top_k,
            verify_input_tokens=bool(genesis.price_input or genesis.price_context))
        from content_policy import from_manifest
        model = state['models'][genesis.chain_id]
        manifest_raw = ((model.get('publication') or {}).get('manifest') or model.get('signed_manifest'))
        if not manifest_raw:
            raise ValueError('registered replay requires signed manifest')
        comparison['content_policy'] = from_manifest(genesis,
            ModelManifest.FromString(base64.b64decode(manifest_raw)))
        pinned = None
        if comparison['verify_input_tokens']:
            try:
                c = ExecutionCommitment.FromString(base64.b64decode(hold['token_fee']['commitment']))
                pinned = c.tokenizer_version
            except Exception:
                pinned = ''
            comparison['tokenizer_version'] = pinned
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
    if equality_enabled and 'openings' not in job:
        unavailable = True
    # Reconstruct reasoning mode from the payer-frozen signed commitment, never
    # from an auditor job override. Legacy commitments retain their old prompt.
    frozen_commitment = (hold.get('token_fee') or {}).get('commitment')
    if frozen_commitment:
        c = ExecutionCommitment.FromString(base64.b64decode(frozen_commitment))
        if c.sampling_params_version:
            if c.sampling_params_version != 2:
                raise ValueError('unknown sampling parameter version')
            from reasoning import prepare, sampling_hash, Budget
            params = dict(max_tokens=c.max_output_tokens, thinking_enabled=c.thinking_enabled,
                          thinking_budget=c.thinking_budget, answer_budget=c.answer_budget)
            if sampling_hash(params) != c.sampling_params_hash:
                raise ValueError('committed reasoning parameters do not recompute')
            if hashlib.sha256(job['prompt'].encode()).digest() != c.input_commitment:
                raise ValueError('audit prompt differs from committed reasoning input')
            budget = Budget(params)
            for opening in job.get('openings', []):
                budget.add(agent.engine.tokenizer.decode([opening['token']]))
            if (receipt.reasoning_tokens,receipt.content_tokens) != (budget.reasoning,budget.content):
                raise ValueError('reasoning usage differs from evidence')
            job = dict(job, prompt=prepare(agent.engine,job['prompt'],params), seed=c.seed,
                       committed_thinking=params)
    if unavailable:
        verdict, distance, result_grade = 'inconclusive', 1e30, 'evidence-unavailable'
    elif equality_enabled:
        from auditor.equality import compare, compare_teacher_forced
        from engine.spi import UnsupportedCapability
        verdict = None
        audit_path = 'regeneration'
        if os.environ.get('EMBER_AUDIT_TEACHER_FORCING', '1') == '1':
            try:
                # Teacher-forced scoring of the committed sequence; the
                # verdict rule is identical, only the execution differs.
                verdict, distance, offense, replay.prefix = compare_teacher_forced(agent.engine, job['prompt'], job['seed'], receipt, job['openings'], equality['epsilon'])
                audit_path = 'teacher-forcing'
            except UnsupportedCapability:
                verdict = None
        if verdict is None:
            verdict, distance, offense, replay.prefix = compare(agent.engine, job['prompt'], job['seed'], receipt, job['openings'], equality['epsilon'], thinking=job.get('committed_thinking'))
        logging.getLogger(__name__).info('audit request=%s path=%s', frame.request_id, audit_path)
        result_grade = 'token-equality-v1'
    else:
        verdict, distance, reason = audit(job['prompt'], job['seed'], receipt, job['openings'],
                                    replay, band.tolerance, **comparison)
        verdict, result_grade = (('accept', reason) if reason == 'profile-policy-compliant-refusal'
                                else grade(verdict, distance, band))
    # A mismatch requires another independently assigned reference. Return reject;
    # the chain chooses the second identity rather than replaying twice locally.
    if not equality_enabled or unavailable:
        verdict, offense = offense_grade(verdict, distance, reason, band, hold, tokenizer_version=pinned, recomputed_input_tokens=replay.input_count)
    challenge = AuditChallenge(receipt_ref=digest(receipt), positions=list(range(replay.prefix)),
        vrf_proof=bytes.fromhex(hold['audit_proof']))
    sign(challenge, agent.key)
    signed = AuditVerdict(n_out_billed=replay.prefix, challenge_ref=digest(challenge), auditor_id=agent.node_id,
        verdict=verdict, evidence_ref=evidence_ref or hashlib.sha256(json.dumps(job.get('openings', []),
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


def execute_range(agent, frame, job, state, hold):
    """Use the finalized drawn range, never a gateway's unsigned draw override."""
    from ember_pb2 import PipelineExecutionReceipt
    from auditor.pipeline import audit_pipeline, load_pipeline_policy, pipeline_offense
    from evidence import canonical
    receipt_raw = bytes.fromhex(job['pipeline_receipt'])
    receipt = PipelineExecutionReceipt.FromString(receipt_raw)
    if receipt.request_id != frame.request_id or receipt_raw != base64.b64decode(hold['pipeline']['receipt']):
        raise ValueError('pipeline receipt differs from finalized hold')
    receipt_ref = bytes.fromhex(hold['receipt_ref'])
    assignment = hold['assignments'][-1]
    if not assignment.get('drawn_stage'):
        raise ValueError('missing finalized range assignment')
    if job.get('drawn_stage') != assignment['drawn_stage']:
        raise ValueError('job range differs from finalized assignment')
    job['hold'] = hold
    d = job['drawn_stage']
    auth = hold['pipeline']['authorizations'][d['index']]
    # Match producer evidence to the exact frozen registered profile, not a
    # self-signed replacement of a different runtime/device.
    from auditor.range_pipeline import stage_invocations
    for _, profile, _ in stage_invocations(job):
        if profile.SerializeToString(deterministic=True) != base64.b64decode(auth['profile']):
            raise ValueError('range producer differs from frozen registration')
    metric, thresholds, policy_ref = load_pipeline_policy(job, agent.engine)
    verdict, distance, reason, faults = audit_pipeline(job, agent.engine, thresholds, metric)
    prefix = receipt.output_tokens if math.isfinite(distance) else 0
    if verdict == 'dispute':
        verdict = 'reject'  # confirmation is by another chain-assigned identity
    offense = pipeline_offense(verdict, distance, job)
    if verdict == 'reject' and offense is None:
        verdict, faults = 'inconclusive', []  # invalid evidence cannot authorize slashing
    challenge = AuditChallenge(receipt_ref=receipt_ref, positions=list(range(prefix)),
        vrf_proof=bytes.fromhex(hold['audit_proof']))
    sign(challenge, agent.key)
    signed = AuditVerdict(n_out_billed=prefix, challenge_ref=digest(challenge),
        auditor_id=agent.node_id, verdict=verdict, fault_node_ids=faults,
        evidence_ref=hashlib.sha256(canonical(job.get('openings', []))).digest(),
        audit_policy_ref=bytes.fromhex(policy_ref), distances=[distance if math.isfinite(distance) else 1e30])
    if offense is not None:
        signed.offense.CopyFrom(offense)
    sign(signed, agent.key)
    action = dict(type='audit', hold_id=frame.request_id,
        challenge=base64.b64encode(challenge.SerializeToString(deterministic=True)).decode(),
        verdict=base64.b64encode(signed.SerializeToString(deterministic=True)).decode())
    envelope = dict(chain_id=agent.chain_args.ledger_chain_id,
        public_key=base64.b64encode(bytes(signed.signer)).decode(),
        nonce=state['accounts'][agent.node_id]['nonce'] + 1, action=action, signature=None)
    raw = json.dumps(envelope, separators=(',', ':'), ensure_ascii=False).encode()
    envelope['signature'] = base64.b64encode(agent.key.sign(b'ember/chain-tx/v1\x00' + raw)).decode()
    return json.dumps(dict(envelope=base64.b64encode(json.dumps(envelope,
        separators=(',', ':')).encode()).decode(), verdict=verdict,
        distance=distance if math.isfinite(distance) else 1e30, grade='pipeline-range-v2')).encode()
