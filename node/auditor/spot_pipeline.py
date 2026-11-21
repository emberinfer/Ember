"""Pipeline record binding and prefix replay, using the existing numeric comparator."""
import hashlib
import json
import math
from google.protobuf.message import DecodeError

from ember_pb2 import ShardForwardRequest, ShardForwardResponse
from engine import Step
from spotcheck import PIPELINE_RECORD_FORMAT, record_hashes
from .numeric import compare
from .spot_binding import BindingFailure, verify_proof


def verify_pipeline_binding(package, context):
    step, count = package['step_index'], context['total_steps']
    if package.get('kind') != 'pipeline' or package['leaf_count'] != count:
        raise BindingFailure('pipeline package type/count')
    profile = hashlib.sha256(context['profile_id'].encode()).hexdigest()
    fmt = hashlib.sha256(PIPELINE_RECORD_FORMAT.encode()).hexdigest()

    def member(leaf, proof, index):
        if (leaf['step_index'], leaf['request_id'], leaf['profile_id_hash'], leaf['format_hash']) != (
                index, context['request_id'], profile, fmt):
            raise BindingFailure('pipeline leaf identity/format')
        verify_proof(leaf, proof, context['root'], count)

    def first(value, index):
        raw = bytes.fromhex(value['first_request'])
        hashes = value['request_hashes']
        if not hashes or hashlib.sha256(raw).hexdigest() != hashes[0]:
            raise BindingFailure('first request digest')
        req = ShardForwardRequest.FromString(raw)
        if (req.step != index - 1 or req.request_id != context['invocation_request_id']
                or req.hidden_bf16 or req.hidden_shape):
            raise BindingFailure('first request identity/shape')
        tokens = context['request_tokens'] + context['claimed_tokens'][:index-1]
        if list(req.input_ids) != tokens:
            raise BindingFailure('trusted token prefix mismatch')
        incoming = hashlib.sha256(b''.join(bytes.fromhex(h) for h in hashes)).hexdigest()
        if any(len(bytes.fromhex(h)) != 32 for h in hashes):
            raise BindingFailure('invalid request digest')
        return req, incoming

    if len(context['claimed_tokens']) != count:
        raise BindingFailure('trusted output count')
    leaf = package['leaf']
    member(leaf, package['proof'], step)
    req, _ = first(package, step)
    response_raw = bytes.fromhex(package['last_response'])
    opening_raw = bytes.fromhex(package['opening'])
    response_hashes = package['response_hashes']
    if not response_hashes or hashlib.sha256(response_raw).hexdigest() != response_hashes[-1]:
        raise BindingFailure('last response digest')
    ih, oh = record_hashes(package['request_hashes'], response_hashes,
                            hashlib.sha256(opening_raw).hexdigest())
    if (ih, oh) != (leaf['input_hash'], leaf['output_hash']):
        raise BindingFailure('pipeline combined record hash')
    response = ShardForwardResponse.FromString(response_raw)
    opened = json.loads(opening_raw)
    if (response.receipt.step != step-1 or response.receipt.request_id != req.request_id
            or opened['token'] != response.token_id
            or opened['token'] != context['claimed_tokens'][step-1]):
        raise BindingFailure('record token chain/terminal anchor')
    if step < count:
        # Ruling 11: only input is recomputed. Whole next leaf is proven.
        member(package['next_leaf'], package['next_proof'], step+1)
        nxt, next_input = first(package['next'], step+1)
        if (next_input != package['next_leaf']['input_hash'] or
                list(nxt.input_ids) != list(req.input_ids) + [opened['token']]):
            raise BindingFailure('next token boundary')
    return Step(opened['token'], '', [tuple(v) for v in opened['topk']],
                {k: bytes.fromhex(v) for k, v in opened['activations'].items()},
                opened.get('score_kind', 'raw-logits'))


def audit_pipeline_spot(context, spool, engine, key, evidence_dir, *, mode, height):
    from .spot import deadlines, write_evidence, verdict_kind
    steps = list(range(1, context['total_steps']+1)) if mode == 'escalation' and context.get('version', 14) < 15 else context['steps']
    if not steps or steps != sorted(set(steps)):
        raise ValueError('invalid pipeline challenge set')
    packages, actual, results = {}, {}, []
    for step in steps:
        if height is not None:
            deadlines(context, height() if callable(height) else height)
        package = spool.get(context, step)
        if package is None:
            raise ValueError('pipeline opening not yet delivered')
        packages[step] = package
        result = dict(step=step, outcome='match', first_divergence_distance=0.0)
        try:
            actual[step] = verify_pipeline_binding(package, context)
        except (ValueError, KeyError, TypeError, OverflowError, DecodeError) as exc:
            result.update(outcome='binding_fail', reason=str(exc))
        results.append(result)
    # Bind every opening before any model execution. One prefix traversal only.
    if len(actual) == len(steps):
        if mode == 'escalation' and context.get('version', 14) < 15:
            full_job = getattr(spool, 'pipeline_full_job', None)
            if full_job is None:
                raise ValueError('pipeline escalation requires signed full evidence')
            from .pipeline import reconstruct
            receipt, full_actual = reconstruct(full_job)
            if receipt.output_tokens != context['total_steps']:
                raise BindingFailure('full receipt count differs from finalized N')
            from evidence import canonical, opening
            if any(canonical(opening(full_actual[i-1])) != canonical(opening(actual[i])) for i in steps):
                raise BindingFailure('full signed records differ from committed openings')
            actual = {i: full_actual[i-1] for i in steps}
        tokens = context['claimed_tokens'][:max(steps)]
        reference = engine.replay(context['prompt'], context['seed'], tokens)
        if len(reference) != len(tokens):
            raise ValueError('reference returned incomplete prefix')
        for result in results:
            step = result['step']
            distance, details = compare([actual[step]], [reference[step-1]], context['metric'])
            # The comparator remains unchanged; the chain wire requires finite distances.
            if not math.isfinite(distance):
                distance = 1e30
            result.update(first_divergence_distance=distance, reason=details['reason'])
            if distance > context['epsilon'] or reference[step-1].token != actual[step].token:
                result['outcome'] = 'mismatch'
    for result in results:
        if height is not None:
            deadlines(context, height() if callable(height) else height)
        if result['outcome'] != 'match' and key is not None:
            result['evidence'] = write_evidence(evidence_dir, packages[result['step']], context, result.copy(), key)
    bad = any(r['outcome'] != 'match' for r in results)
    kind = verdict_kind(context, results, mode)
    return dict(per_step=results, verdict_kind=kind,
                verdict_distance=max(r['first_divergence_distance'] for r in results),
                escalation_required=bad and mode != 'escalation')
