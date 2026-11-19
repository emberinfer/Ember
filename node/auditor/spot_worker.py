"""Registered audit entry point; finalized chain assignment always wins."""
import base64
import hashlib
import json
from pathlib import Path

from ember_core import digest, sign
from ember_pb2 import AuditChallenge
from .binding import signed_envelope
from .spot import (DirectorySpool, audit, canonical, durable_json,
                   SPOT_FIRST_VERDICT_BLOCKS, SPOT_SUBSTITUTE_BLOCKS, SPOT_ESCALATION_BLOCKS)
from .spot_wire import SpotVerdict


def context_for(agent, frame, hold, job=None, query=None):
    """Load locally pinned profile/formats and payer/output token anchors.

    The local record is provisioned from the request's authenticated execution
    archive; never accept operations, epsilon, anchors or file paths in a job.
    Assignment/root/deadlines are replaced with finalized chain state below.
    """
    args = agent.chain_args
    root = Path(getattr(args, 'spot_context_dir', None) or Path(args.state_dir) / 'spot-context')
    request_id = frame.request_id
    if len(bytes.fromhex(request_id)) != 32 or Path(request_id).name != request_id:
        raise ValueError('invalid spot request identity')
    path = root / (request_id + '.json')
    derived = False
    try:
        with path.open() as source:
            context = json.load(source)
    except FileNotFoundError:
        from .spot_context import derive_context
        if job is None or query is None:
            raise
        context = derive_context(agent, frame, job, hold, query)
        derived = True
    pipeline = hold['pipeline']
    challenge = pipeline['spot']
    if context['request_id'] != request_id or challenge['request_id'] != request_id:
        raise ValueError('spot context differs from finalized request')
    if context['total_steps'] != pipeline['spot_step_count']:
        raise ValueError('profile step count differs from finalized model')
    context.update(root=base64.b64decode(pipeline['commitment_root']).hex(),
                   steps=challenge['step_set'], round=challenge['round'],
                   challenge_height=challenge['challenge_height'],
                   open_deadline=challenge['deadline'], challenge_record=challenge,
                   history_heights=[c['challenge_height'] for c in challenge.get('history', [])],
                   verdict_deadline=challenge.get('verdict_deadline') or (challenge['deadline']
                       if challenge['round'] == 'esc' else challenge['deadline'] + SPOT_FIRST_VERDICT_BLOCKS))
    context['segment_deadlines'] = dict(open=context['open_deadline'],
        first_verdict=context['verdict_deadline'],
        substitute_blocks=SPOT_SUBSTITUTE_BLOCKS, escalation_blocks=SPOT_ESCALATION_BLOCKS)
    if derived:
        # All required fields must be available before the first durable write.
        durable_json(path, context)
    return context


def envelope_result(agent, action, summary):
    # Same 2F sign-time nonce path and ordinary envelope template. Cached
    # results are re-signed by worker.resend_verdict without recomputation.
    from .worker import _fresh_audit_nonce
    from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat
    public = agent.key.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)
    return json.dumps(dict(summary, envelope=signed_envelope(
        agent, action, _fresh_audit_nonce(agent) + 1, public))).encode()


def execute(agent, frame, job, hold, query):
    if hold.get('auditor') != agent.node_id or hold['pipeline']['spot']['auditor'] != agent.node_id:
        raise ValueError('not finalized spot auditor')
    context = context_for(agent, frame, hold, job, query)
    equality = query('/parameters').get('audit_equality', {})
    if equality.get('enabled'):
        context['epsilon'] = equality['epsilon']
    args = agent.chain_args
    spool_root = getattr(args, 'spot_spool_dir', None)
    spool = getattr(agent, 'spot_spool', None)
    if spool is None:
        if not spool_root:
            raise ValueError('spot audit requires configured durable gateway spool mirror')
        attestor = None
        gateway_url = getattr(args, 'spot_gateway_url', None)
        if gateway_url:
            def attestor(request_id, challenge_height, steps, deadline, height):
                from urllib.request import urlopen
                with urlopen(gateway_url.rstrip('/') + '/v1/spot/' + request_id + '/absence', timeout=30) as response:
                    return json.load(response)
        spool = DirectorySpool(spool_root, attestor=attestor)
    delivery = job.get('spot_delivery')
    if delivery is not None:
        # Transport authenticates the job; roots/tokens still come exclusively
        # from finalized chain facts and the locally provisioned context.
        from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey
        from ember_core import node_id
        att = delivery['receipt']
        public = bytes.fromhex(context['gateway_public'])
        ordered = {k: att[k] for k in ('request_id', 'challenge_height', 'step_set_hash', 'gateway', 'received', 'height')}
        expected = hashlib.sha256(b''.join(int(i).to_bytes(8, 'big') for i in context['steps'])).digest()
        if (att['gateway'] != node_id(public) or not att['received']
                or att['request_id'] != context['request_id']
                or att['challenge_height'] != context['challenge_height']
                or base64.b64decode(att['step_set_hash'], validate=True) != expected
                or delivery['steps'] != context['steps']
                or delivery['challenge_height'] != context['challenge_height']
                or delivery['request_id'] != context['request_id']
                or delivery['height'] != att['height']
                or not context['challenge_height'] <= att['height'] <= query('/state')['height']
                or [p['step_index'] for p in delivery['packages']] != context['steps']):
            raise ValueError('delivered spot challenge mismatch')
        Ed25519PublicKey.from_public_bytes(public).verify(base64.b64decode(att['signature'], validate=True),
            b'EMBER-SPOT-SPOOL-V1\0' + json.dumps(ordered, separators=(',', ':')).encode())
        if not isinstance(spool, DirectorySpool):
            raise ValueError('transferred opening requires configured durable mirror')
        durable_json(spool.root / (hashlib.sha256(canonical(delivery)).hexdigest()+'.json'), delivery)
        if not hold['pipeline']['spot'].get('received'):
            from .spot_patrol import submit_received
            submit_received(agent, frame.request_id)
    mode = job.get('mode', 'normal')
    if mode == 'unopened':
        return unopened_action(agent, context, spool, bytes.fromhex(context['gateway_public']),
                               job.get('spool_attestation') or spool.absence(context, query('/state')['height']), query('/state')['height'])
    if (mode == 'escalation') != (context['round'] == 'esc'):
        raise ValueError('mode differs from finalized escalation assignment')
    if mode == 'escalation':
        if not job.get('pipeline_full') and context.get('kind') == 'pipeline':
            raise ValueError('pipeline escalation requires the existing full package')
        if context.get('kind') == 'pipeline':
            spool.pipeline_full_job = job
    evidence_dir = Path(args.state_dir) / 'spot-evidence' / frame.request_id
    result = audit(context, spool, agent.engine, agent.key, evidence_dir, mode=mode,
                   height=lambda: query('/state')['height'])
    # Persist the complete evidence index before signing its content hash.
    index = dict(context=context, result=result)
    durable_json(evidence_dir / 'verdict.json', index)
    challenge = AuditChallenge(receipt_ref=bytes.fromhex(hold['receipt_ref']),
                               positions=context['steps'], vrf_proof=bytes.fromhex(hold['audit_proof']))
    sign(challenge, agent.key)
    signed = SpotVerdict(challenge_ref=digest(challenge), auditor_id=agent.node_id,
        verdict='reject' if 'reject' in result['verdict_kind'] else 'accept',
        evidence_ref=hashlib.sha256(canonical(index)).digest(),
        distances=[result['verdict_distance']], verdict_distance=result['verdict_distance'],
        verdict_kind=result['verdict_kind'])
    for item in result['per_step']:
        signed.per_step.add(**{k: item[k] for k in ('step', 'outcome', 'first_divergence_distance')})
    sign(signed, agent.key)
    action = dict(type='audit', hold_id=frame.request_id,
                  challenge=base64.b64encode(challenge.SerializeToString(deterministic=True)).decode(),
                  verdict=base64.b64encode(signed.SerializeToString(deterministic=True)).decode())
    return envelope_result(agent, action, result)


def unopened_action(agent, context, spool, gateway_public, chain_attestation, height):
    """Require both gateway spool facts and the chain-format gateway signature.

    Never translate/re-sign a gateway statement under an auditor identity.
    work-b must provide the chain attestation alongside its detailed spool log.
    """
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey
    from .spot import unopened_claim
    claim = unopened_claim(context, spool, agent.key, gateway_public, height)
    att = chain_attestation
    expected_hash = hashlib.sha256(b''.join(int(i).to_bytes(8, 'big') for i in context['steps'])).digest()
    from ember_core import node_id
    if (att['request_id'] != context['request_id'] or att['challenge_height'] != context['challenge_height']
            or base64.b64decode(att['step_set_hash']) != expected_hash or att['received']
            or att['gateway'] != node_id(gateway_public)
            or not context['open_deadline'] <= att['height'] <= height):
        raise ValueError('chain spool attestation mismatch')
    # Exact Go struct order and omitempty signature (chain SpoolSigningBytes).
    ordered = {k: att[k] for k in ('request_id', 'challenge_height', 'step_set_hash', 'gateway', 'received', 'height')}
    raw = json.dumps(ordered, separators=(',', ':')).encode()
    Ed25519PublicKey.from_public_bytes(gateway_public).verify(
        base64.b64decode(att['signature']), b'EMBER-SPOT-SPOOL-V1\0' + raw)
    durable_json(Path(agent.chain_args.state_dir) / 'spot-evidence' / context['request_id'] / 'unopened.json', claim)
    action = dict(spool=att, type='spot_unopened', hold_id=context['request_id'])
    return envelope_result(agent, action, claim)
