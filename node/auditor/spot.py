"""Stepwise spot/substitute/escalation review over gateway durable spool.

Only metadata is loaded as JSON. Tensor files are copied and hashed in bounded
blocks, and only the currently replayed step enters the reference runtime.
"""
import copy
import hashlib
import json
import logging
import os
from pathlib import Path
import struct
import uuid

from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey
from .spot_binding import BindingFailure, FormatFailure, verify_binding
from .spot_replay import CPU_REFERENCE, compare_output, replay_step

SPOT_OPEN_ROUND_BLOCKS = 120
SPOT_FIRST_VERDICT_BLOCKS = 60
SPOT_SUBSTITUTE_BLOCKS = 120
SPOT_ESCALATION_BLOCKS = 600


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False).encode()


def durable_json(path, value):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    temporary = path.with_name(path.name + '.' + uuid.uuid4().hex + '.tmp')
    with temporary.open('xb') as stream:
        os.chmod(temporary, 0o600)
        stream.write(canonical(value))
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(temporary, path)
    fd = os.open(path.parent, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def signed(value, key, domain=b'EMBER-SPOT-EVIDENCE-V1\0'):
    public = key.public_key().public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)
    return dict(payload=value, signer=public.hex(), signature=key.sign(domain + canonical(value)).hex())


def verify_signed(document, domain=b'EMBER-SPOT-EVIDENCE-V1\0', public=None):
    signer = bytes.fromhex(document['signer'])
    if public is not None and signer != public:
        raise ValueError('signature is not by registered gateway')
    Ed25519PublicKey.from_public_bytes(signer).verify(bytes.fromhex(document['signature']),
                                                     domain + canonical(document['payload']))
    return document['payload']


class DirectorySpool:
    """Read-only adapter for work-b OpeningService's durable spool directory.

    Root comes from local operator configuration, never the unsigned job. A
    mounted spool or an authenticated streaming mirror may implement this same
    interface. Delivery flags and the previous auditor's progress are ignored.
    """
    def __init__(self, root, attestor=None):
        self.root = Path(root).resolve()
        self.attestor = attestor

    def records(self, request_id):
        for path in sorted(self.root.glob('*.json')):
            with path.open() as source:
                record = json.load(source)
            if record.get('kind') == 'opening' and record.get('request_id') == request_id:
                yield record

    def get(self, context, step):
        heights = {context['challenge_height'], *context.get('history_heights', [])}
        for record in self.records(context['request_id']):
            if record['challenge_height'] not in heights:
                continue
            for package in record['packages']:
                if package['step_index'] == step:
                    value = copy.deepcopy(package)
                    if context.get('kind') == 'pipeline':
                        if value.get('kind') != 'pipeline':
                            raise ValueError('expected pipeline opening')
                        return value
                    if value.get('kind') == 'pipeline':
                        raise ValueError('pipeline opening in tensor task')
                    for fmt in value['tensors'].values():
                        path = Path(fmt['path']).resolve()
                        if not path.is_relative_to(self.root):
                            raise ValueError('tensor reference escapes configured spool')
                        fmt['path'] = str(path)
                    return value
        return None

    def absence(self, context, height):
        # A local miss never substitutes for a gateway-signed attestation.
        if self.attestor is not None:
            return self.attestor(context['request_id'], context['challenge_height'],
                                 context['steps'], context['open_deadline'], height)
        for path in sorted(self.root.glob('*.json')):
            with path.open() as source:
                doc = json.load(source)
            if ('payload' not in doc and doc.get('received') is False
                    and doc.get('request_id') == context['request_id']
                    and doc.get('challenge_height') == context['challenge_height']):
                return doc
            payload = doc.get('payload', {})
            if (payload.get('kind') == 'absence' and payload.get('request_id') == context['request_id']
                    and payload.get('challenge_height') == context['challenge_height']):
                return doc
        raise ValueError('gateway absence attestation required; no accusation submitted')


def validate_profile(opening, specification):
    for direction in ('input', 'output'):
        fmt = opening['tensors'][direction]
        expected = specification[direction]
        if (fmt['dtype'], fmt['shape']) != (expected['dtype'], expected['shape']):
            raise FormatFailure('tensor dtype/shape differs from pinned profile')


def audit_step(opening, context, engine):
    step = opening['step_index']
    result = dict(step=step, outcome='match', first_divergence_distance=0.0)
    try:
        if opening.get('leaf_count', context['total_steps']) != context['total_steps']:
            raise BindingFailure('opened leaf count differs from finalized N')
        verify_binding(opening, context)
    except FormatFailure as exc:
        return dict(result, outcome='format_fail', reason=str(exc))
    except (BindingFailure, KeyError, TypeError, ValueError, OverflowError) as exc:
        return dict(result, outcome='binding_fail', reason=str(exc))
    try:
        spec = context['operations'][str(step)]
        validate_profile(opening, spec)
    except (FormatFailure, KeyError, TypeError) as exc:
        return dict(result, outcome='format_fail', reason=str(exc))
    # Runtime/capability failures propagate as service failures, never fraud.
    reference, tokens = replay_step(engine, opening, spec['operator'])
    if spec.get('output_kind') == 'tokens':
        import torch
        reference = torch.tensor(tokens, dtype=torch.int64).reshape(spec['output']['shape'])
    try:
        outcome, distance, position = compare_output(reference, opening, context['epsilon'])
    except FormatFailure as exc:
        return dict(result, outcome='format_fail', reason=str(exc))
    if step == context['total_steps']:
        from .spot_replay import mapped_tensor
        if tokens != context['claimed_tokens']:
            outcome = 'mismatch'
            result['reason'] = 'terminal output tokens differ exactly'
        if spec.get('output_kind') == 'tokens' and mapped_tensor(
                opening['tensors']['output']['path'], opening['tensors']['output']).reshape(-1).tolist() != context['claimed_tokens']:
            outcome = 'mismatch'
            result['reason'] = 'opened terminal tokens differ exactly'
    result.update(outcome=outcome, first_divergence_distance=distance,
                  first_divergence=position)
    return result


def write_evidence(directory, opening, context, result, key):
    bundle = Path(directory) / uuid.uuid4().hex
    bundle.mkdir(parents=True, mode=0o700)
    value = copy.deepcopy(opening)
    for name, fmt in value.get('tensors', {}).items():
        source = Path(fmt['path'])
        target = bundle / (name + '.raw')
        digest = hashlib.sha256()
        with source.open('rb') as src, target.open('xb') as dst:
            os.chmod(target, 0o600)
            while True:
                block = src.read(1 << 20)
                if not block:
                    break
                dst.write(block)
                digest.update(block)
            dst.flush()
            os.fsync(dst.fileno())
        fmt['path'] = target.name
        fmt['evidence_sha256'] = digest.hexdigest()
    payload = dict(version=1, context=context, opening=value, result=result,
                   cpu_reference=CPU_REFERENCE)
    document = signed(payload, key)
    durable_json(bundle / 'evidence.json', document)
    return dict(path=str(bundle / 'evidence.json'), sha256=hashlib.sha256(canonical(document)).hexdigest())


def reproduce_evidence(path, engine):
    """No gateway/job/previous auditor state is needed; weights remain local."""
    path = Path(path)
    with path.open() as source:
        payload = verify_signed(json.load(source))
    if payload['cpu_reference'] != CPU_REFERENCE:
        raise ValueError('unsupported CPU reference semantics')
    opening = payload['opening']
    for fmt in opening.get('tensors', {}).values():
        local = (path.parent / fmt['path']).resolve()
        if not local.is_relative_to(path.parent.resolve()):
            raise ValueError('evidence tensor escapes bundle')
        digest = hashlib.sha256()
        with local.open('rb') as stream:
            for block in iter(lambda: stream.read(1 << 20), b''):
                digest.update(block)
        if digest.hexdigest() != fmt['evidence_sha256']:
            raise ValueError('signed evidence tensor digest mismatch')
        fmt['path'] = str(local)
    if opening.get('kind') == 'pipeline':
        from .spot_pipeline import audit_pipeline_spot
        class Single:
            def get(self, context, step):
                return opening if step == opening['step_index'] else None
        context = dict(payload['context'], steps=[opening['step_index']])
        return audit_pipeline_spot(context, Single(), engine, None, path.parent, mode='normal', height=None)['per_step'][0]
    return audit_step(opening, payload['context'], engine)


def deadlines(context, height):
    remaining = context['verdict_deadline'] - height
    if remaining <= 10:
        logging.getLogger(__name__).warning(
            'spot verdict deadline approaching request=%s challenge_height=%s deadline=%s remaining_blocks=%s',
            context['request_id'], context['challenge_height'], context['verdict_deadline'], remaining)
    if remaining < 0:
        raise ValueError('finalized verdict deadline expired')


def audit(context, spool, engine, key, evidence_dir, *, mode='normal', height=None):
    if mode not in ('normal', 'substitute', 'escalation'):
        raise ValueError('unknown spot audit mode')
    if context.get('kind') == 'pipeline':
        from .spot_pipeline import audit_pipeline_spot
        return audit_pipeline_spot(context, spool, engine, key, evidence_dir, mode=mode, height=height)
    steps = list(range(1, context['total_steps'] + 1)) if mode == 'escalation' and context.get('version', 14) < 15 else context['steps']
    if not steps or steps != sorted(set(steps)):
        raise ValueError('challenge steps must be sorted and unique')
    invalid_full_count = False
    if mode == 'escalation' and context.get('version', 14) < 15 and hasattr(spool, 'records'):
        heights = {context['challenge_height'], *context.get('history_heights', [])}
        opened = {p['step_index'] for r in spool.records(context['request_id'])
                  if r['challenge_height'] in heights for p in r['packages']}
        # Missing supplement data still waits below. Excess leaves are positive
        # evidence that the disclosed execution does not match finalized N.
        invalid_full_count = bool(opened - set(steps))
    results = []
    for step in steps:
        if height is not None:
            deadlines(context, height() if callable(height) else height)
        opening = spool.get(context, step)
        if opening is None:
            # Escalation caller requests only missing supplement steps through
            # its existing gateway opening channel, then retries. No verdict
            # is built from a partial full review.
            raise ValueError('opening not yet in gateway spool: step ' + str(step))
        if invalid_full_count:
            opening = dict(opening, leaf_count=-1)
        result = audit_step(opening, context, engine)
        if height is not None:
            deadlines(context, height() if callable(height) else height)
        if result['outcome'] != 'match':
            result['evidence'] = write_evidence(evidence_dir, opening, context, result.copy(), key)
        results.append(result)
    bad = any(r['outcome'] != 'match' for r in results)
    kind = verdict_kind(context, results, mode)
    return dict(per_step=results, verdict_distance=max(r['first_divergence_distance'] for r in results),
                verdict_kind=kind, escalation_required=bad and mode != 'escalation')


def unopened_claim(context, spool, key, gateway_public, height):
    if height <= context['open_deadline']:
        raise ValueError('opening deadline not finalized')
    document = spool.absence(context, height)
    if 'payload' in document:
        attestation = verify_signed(document, b'EMBER-SPOOL-V1\0', gateway_public)
    else:
        import base64
        from ember_core import node_id
        expected_hash = hashlib.sha256(b''.join(int(i).to_bytes(8, 'big') for i in context['steps'])).digest()
        ordered = {k: document[k] for k in ('request_id', 'challenge_height', 'step_set_hash', 'gateway', 'received', 'height')}
        if (ordered['gateway'] != node_id(gateway_public) or ordered['received']
                or base64.b64decode(ordered['step_set_hash'], validate=True) != expected_hash):
            raise ValueError('invalid chain-format absence identity or step set')
        Ed25519PublicKey.from_public_bytes(gateway_public).verify(
            base64.b64decode(document['signature'], validate=True),
            b'EMBER-SPOT-SPOOL-V1\0' + json.dumps(ordered, separators=(',', ':')).encode())
        attestation = dict(kind='absence', request_id=ordered['request_id'],
            challenge_height=ordered['challenge_height'], steps=context['steps'],
            deadline=context['open_deadline'], height=ordered['height'])
    expected = dict(kind='absence', request_id=context['request_id'],
                    challenge_height=context['challenge_height'], steps=context['steps'],
                    deadline=context['open_deadline'])
    if any(attestation.get(k) != v for k, v in expected.items()) or not context['open_deadline'] < attestation['height'] <= height:
        raise ValueError('gateway absence attestation challenge/deadline mismatch')
    # Consult actual spool even after an attestation: a completed late delivery
    # or in-flight transfer cannot be accused based on a stale local miss.
    if any(spool.get(context, step) is not None for step in context['steps']):
        raise ValueError('gateway spool contains opening; refusing accusation')
    statement = dict(request_id=context['request_id'], challenge_height=context['challenge_height'],
                     steps=context['steps'], round=context['round'], received=False,
                     attestation_sha256=hashlib.sha256(canonical(document)).hexdigest())
    return dict(verdict_kind='reject_unopened', challenge=context['challenge_record'],
                attestation=document, auditor_statement=signed(statement, key, b'EMBER-SPOT-UNOPENED-V1\0'))


def verdict_kind(context, results, mode):
    bad = any(r['outcome'] != 'match' for r in results)
    if mode != 'escalation':
        return 'reject_step' if bad else 'accept'
    if context.get('version', 14) < 15:
        return 'escalation_full_reject' if bad else 'escalation_full_accept'
    first = context['first_per_step']
    initial = {r['step'] for r in first}
    disputed = {r['step'] for r in first if r['outcome'] != 'match'}
    wrong = {r['step'] for r in results if r['outcome'] != 'match'}
    convicted = bool(wrong & disputed) and bool(wrong - initial)
    return 'escalation_bounded_reject' if convicted else 'escalation_bounded_accept'
