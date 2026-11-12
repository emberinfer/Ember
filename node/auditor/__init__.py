"""Two-step audit: validate prior signed openings, then compare forced-prefix replay."""
import hashlib
import math

from ember_core import verify, verify_chunks
from evidence import canonical, encode_step
from engine import Step
from .numeric import LEGACY, compare, legacy_distance, score_map


distance = legacy_distance


def audit(prompt, seed, receipt, openings, engine, tolerance, *, metric=LEGACY,
          required_layers=None, required_top_k=None, verify_input_tokens=False):
    verify(receipt)
    if verify_input_tokens and engine.count_input_tokens(prompt) != receipt.input_tokens:
        return "reject", math.inf, "input token count mismatch"
    verify_chunks(receipt.chunk_roots, receipt.request_id, receipt.output_tokens)
    if len(openings) != receipt.output_tokens:
        return 'reject', math.inf, 'opening count'
    reconstructed = []
    for item in openings:
        try:
            reconstructed.append(Step(item['token'], '', item['topk'],
                                      {k: bytes.fromhex(v) for k, v in item['activations'].items()},
                                      item.get('score_kind', 'raw-logits')))
        except (KeyError, ValueError, TypeError):
            return 'reject', math.inf, 'invalid opening'
    for index, root in enumerate(receipt.chunk_roots):
        group = reconstructed[index * 16:root.upto_step]
        if hashlib.sha256(canonical([encode_step(s) for s in group])).digest() != root.merkle_root:
            return 'reject', math.inf, 'commitment mismatch'
    if hashlib.sha256(canonical([encode_step(s) for s in reconstructed])).digest() != receipt.output_commitment:
        return 'reject', math.inf, 'output commitment mismatch'
    if any(s.score_kind != 'raw-logits' for s in reconstructed):
        return 'inconclusive', math.inf, 'raw-logit profile cannot audit logprobs'
    if required_layers is not None and any(set(s.activations) != set(required_layers) for s in reconstructed):
        return 'reject', math.inf, 'required layers missing'
    if required_top_k is not None and any(len(s.topk) != required_top_k for s in reconstructed):
        return 'reject', math.inf, 'required topk count'
    if required_top_k is not None:
        try:
            for step in reconstructed:
                scores = score_map(step.topk)
                if type(step.token) is not int or step.token not in scores or scores[step.token] != max(scores.values()):
                    raise ValueError('invalid greedy token')
        except (ValueError, TypeError):
            return 'reject', math.inf, 'invalid committed logit evidence'
    reference = engine.replay(prompt, seed, [s.token for s in reconstructed])
    max_distance, details = compare(reconstructed, reference, metric)
    return ('accept' if max_distance <= tolerance else 'dispute'), max_distance, details['reason']


def confirm(prompt, seed, receipt, openings, first, second, tolerance, **comparison):
    first_result = audit(prompt, seed, receipt, openings, first, tolerance, **comparison)
    if first_result[0] == 'reject':
        return first_result
    if first_result[0] != 'dispute':
        return first_result
    second_result = audit(prompt, seed, receipt, openings, second, tolerance, **comparison)
    return ('reject', max(first_result[1], second_result[1]), 'two reference executions') if second_result[0] == 'dispute' else ('inconclusive', first_result[1], 'reference disagreement')
