"""Canonical signed recompute and third-auditor commitment bindings."""
import base64
import hashlib
import json
import math
import struct


def tolerance_identifier(offense=None, *, equality=False, band=None):
    if offense is not None:
        kind = getattr(offense, 'class')
        if kind == 'objective':
            return 'exact'
        if kind in ('token', 'numerical') and offense.band:
            return offense.band
        raise ValueError('invalid offense tolerance')
    if equality:
        return 'token-equality-v1'
    return hashlib.sha256(band.SerializeToString(deterministic=True)).hexdigest()


def recompute_ref(verdict):
    if len(verdict.challenge_ref) != 32 or len(verdict.evidence_ref) != 32:
        raise ValueError('invalid recompute references')
    if any(not math.isfinite(d) for d in verdict.distances):
        raise ValueError('non-finite recompute distance')
    position = verdict.first_divergence if verdict.HasField('first_divergence') else -1
    preimage = (b'ember/recompute/v1\x00' + verdict.auditor_id.encode() + b'\x00' +
                verdict.challenge_ref + verdict.evidence_ref + verdict.verdict.encode() + b'\x00' +
                struct.pack('>q', position) + verdict.tolerance.encode() + b'\x00' +
                struct.pack('>II', verdict.n_out_billed, len(verdict.distances)) +
                b''.join(struct.pack('>d', d) for d in verdict.distances))
    return hashlib.sha256(preimage).digest()


def third_commitment(verdict, evidence):
    position = evidence.get('first_divergence')
    fields = [verdict, evidence['auditor'], evidence['result_commitment'],
              evidence['replay_record'], str(-1 if position is None else position),
              evidence['tolerance'], evidence['cause']]
    return hashlib.sha256(b'ember/third/v1\x00' + '\x00'.join(fields).encode()).hexdigest()


def signed_envelope(agent, action, nonce, public):
    envelope = dict(chain_id=agent.chain_args.ledger_chain_id,
                    public_key=base64.b64encode(public).decode(), nonce=nonce,
                    action=action, signature=None)
    raw = json.dumps(envelope, separators=(',', ':'), ensure_ascii=False).encode()
    envelope['signature'] = base64.b64encode(agent.key.sign(b'ember/chain-tx/v1\x00' + raw)).decode()
    return base64.b64encode(json.dumps(envelope, separators=(',', ':')).encode()).decode()


def audit_envelopes(agent, hold, signed, action, nonce):
    """Round three commits with nonce+1, then reveals with nonce+2."""
    result = {}
    if hold.get('audit_round') == 3:
        grade = hold.get('first_grade') or {}
        evidence = dict(auditor=agent.node_id, result_commitment=hold['receipt_ref'],
                        replay_record=signed.recompute_ref.hex(),
                        first_divergence=signed.first_divergence if signed.HasField('first_divergence') else -1,
                        tolerance=signed.tolerance,
                        cause=':'.join(grade.get(k, '') for k in ('class', 'fault', 'band')))
        # Match services/chain.Action field order used by signingBytes.
        commit = dict(adjudication_commitment=third_commitment(signed.verdict, evidence),
                      type='commit_third_adjudication', hold_id=action['hold_id'])
        result['pre_envelope'] = signed_envelope(agent, commit, nonce+1, bytes(signed.signer))
        nonce += 1
    result['envelope'] = signed_envelope(agent, action, nonce+1, bytes(signed.signer))
    return result
