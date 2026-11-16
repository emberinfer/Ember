"""Two-step audit: validate prior signed openings, then compare forced-prefix replay."""
import hashlib
import math

from ember_core import verify, verify_chunks
from evidence import canonical, encode_step
from engine import Step
from .numeric import LEGACY, compare, legacy_distance, score_map


distance = legacy_distance


def audit_detailed(prompt, seed, receipt, openings, engine, tolerance, *, metric=LEGACY,
          required_layers=None, required_top_k=None, verify_input_tokens=False, content_policy=None, tokenizer_version=None):
    verify(receipt)
    if verify_input_tokens:
        from tokenization import count, TokenizerUnavailable
        try:
            recomputed, _ = count(engine,prompt,tokenizer_version)
        except TokenizerUnavailable:
            return 'inconclusive', math.inf, 'tokenizer unavailable', None
        if recomputed != receipt.input_tokens:
            return "reject", math.inf, "input token count mismatch", None
    verify_chunks(receipt.chunk_roots, receipt.request_id, receipt.output_tokens)
    if len(openings) != receipt.output_tokens:
        return 'reject', math.inf, 'opening count', min(len(openings), receipt.output_tokens)
    reconstructed = []
    for index, item in enumerate(openings):
        try:
            reconstructed.append(Step(item['token'], '', item['topk'],
                                      {k: bytes.fromhex(v) for k, v in item['activations'].items()},
                                      item.get('score_kind', 'raw-logits')))
        except (KeyError, ValueError, TypeError):
            return 'reject', math.inf, 'invalid opening', index
    for index, root in enumerate(receipt.chunk_roots):
        group = reconstructed[index * 16:root.upto_step]
        if hashlib.sha256(canonical([encode_step(s) for s in group])).digest() != root.merkle_root:
            return 'reject', math.inf, 'commitment mismatch', index * 16
    if hashlib.sha256(canonical([encode_step(s) for s in reconstructed])).digest() != receipt.output_commitment:
        return 'reject', math.inf, 'output commitment mismatch', None
    from content_policy import refusal_steps
    expected = refusal_steps(content_policy, prompt)
    if receipt.output_category or expected is not None:
        if (receipt.output_category == 'policy-refusal' and expected is not None
                and canonical([encode_step(s) for s in expected]) ==
                    canonical([encode_step(s) for s in reconstructed])):
            return 'accept', 0.0, 'profile-policy-compliant-refusal', None
        position = None
        if expected is not None:
            position = next((i for i, (a, b) in enumerate(zip(reconstructed, expected)) if a.token != b.token), None)
            if position is None and len(reconstructed) != len(expected):
                position = min(len(reconstructed), len(expected))
        return 'reject', math.inf, 'content policy mismatch', position
    if any(s.score_kind != 'raw-logits' for s in reconstructed):
        return 'inconclusive', math.inf, 'raw-logit profile cannot audit logprobs', None
    if required_layers is not None and any(set(s.activations) != set(required_layers) for s in reconstructed):
        return 'reject', math.inf, 'required layers missing', next(i for i, s in enumerate(reconstructed) if set(s.activations) != set(required_layers))
    if required_top_k is not None and any(len(s.topk) != required_top_k for s in reconstructed):
        return 'reject', math.inf, 'required topk count', next(i for i, s in enumerate(reconstructed) if len(s.topk) != required_top_k)
    if required_top_k is not None:
        try:
            for index, step in enumerate(reconstructed):
                scores = score_map(step.topk)
                if type(step.token) is not int or step.token not in scores or scores[step.token] != max(scores.values()):
                    raise ValueError('invalid greedy token')
        except (ValueError, TypeError):
            return 'reject', math.inf, 'invalid committed logit evidence', index
    reference = engine.replay(prompt, seed, [s.token for s in reconstructed])
    max_distance, details = compare(reconstructed, reference, metric)
    divergence = next((i for i, value in enumerate(details['position_values'])
                       if not math.isfinite(value) or value > tolerance), None)
    return ('accept' if max_distance <= tolerance else 'dispute'), max_distance, details['reason'], divergence


def audit(prompt, seed, receipt, openings, engine, tolerance, **comparison):
    return audit_detailed(prompt, seed, receipt, openings, engine, tolerance, **comparison)[:3]


def confirm(prompt, seed, receipt, openings, first, second, tolerance, **comparison):
    first_result = audit(prompt, seed, receipt, openings, first, tolerance, **comparison)
    if first_result[0] == 'reject':
        return first_result
    if first_result[0] != 'dispute':
        return first_result
    second_result = audit(prompt, seed, receipt, openings, second, tolerance, **comparison)
    return ('reject', max(first_result[1], second_result[1]), 'two reference executions') if second_result[0] == 'dispute' else ('inconclusive', first_result[1], 'reference disagreement')
