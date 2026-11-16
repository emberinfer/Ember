"""Discrete greedy output adjudication using first-divergence evidence."""
import math
from ember_pb2 import OffenseGrade
from auditor.grading import offense_grade


def _precheck(engine, prompt, receipt, openings):
    if engine.count_input_tokens(prompt) != receipt.input_tokens or len(openings) != receipt.output_tokens:
        verdict, offense = offense_grade('reject', 0, 'opening count', None, {})
        return verdict, 0, offense, 0, (min(len(openings), receipt.output_tokens) if len(openings) != receipt.output_tokens else 0)
    return None


def _adjudicate(steps, openings, epsilon):
    """First-divergence rule shared by both audit execution paths.

    steps[i] is the auditor-side distribution at output position i: its
    token is the greedy token under the prefix the path conditions on and
    its topk carries that position's scores. Regeneration conditions on the
    auditor's own generated prefix; teacher forcing conditions on the
    committed prefix. The prefixes agree at every position up to the first
    divergence, and this rule never looks past the first divergence, so in
    exact arithmetic both paths return the same verdict, margin and count.
    """
    count = len(steps)
    for index, (step, opening) in enumerate(zip(steps, openings)):
        if step.token == opening['token']:
            continue
        scores = dict(step.topk[:16])
        if len(scores) != 16 or any(not math.isfinite(v) for v in scores.values()) or step.token not in scores:
            return 'inconclusive', 0, None, count, index
        exact = opening['token'] in scores
        margin = scores[step.token] - (scores[opening['token']] if exact else min(scores.values()))
        if margin < 0:
            return 'inconclusive', 0, None, count, index
        if margin > epsilon:
            return 'reject', margin, OffenseGrade(**{'class': 'token', 'score': margin, 'band': 'token-equality-v1'}), count, index
        return ('accept' if exact else 'inconclusive'), margin, None, count, (None if exact else index)
    if count != len(openings):
        return 'inconclusive', 0, None, count, None
    return 'accept', 0, None, count, None


def compare_detailed(engine, prompt, seed, receipt, openings, epsilon, thinking=None):
    if not math.isfinite(epsilon) or epsilon <= 0:
        raise ValueError('invalid chain equality epsilon')
    failed = _precheck(engine, prompt, receipt, openings)
    if failed is not None:
        return failed
    # Replay regenerates under the same committed reasoning parameters
    # the executor was given (worker reconstructs them from the signed
    # commitment); legacy commitments pass thinking=None and keep the
    # historical defaults.
    kwargs = {}
    if thinking is not None:
        kwargs = dict(enable_thinking=thinking['thinking_enabled'],
                      thinking_budget=thinking['thinking_budget'],
                      answer_budget=thinking['answer_budget'])
    steps = list(engine.generate(prompt, seed, len(openings), **kwargs))
    return _adjudicate(steps, openings, epsilon)


def compare_teacher_forced_detailed(engine, prompt, seed, receipt, openings, epsilon):
    """Same verdict rule, executed by scoring the committed sequence.

    The auditor evaluates the model once over prompt+committed tokens and
    adjudicates the resulting per-position distributions instead of
    regenerating the output token by token. Raises UnsupportedCapability
    (from the engine) when the adapter cannot score a forced prefix.
    """
    if not math.isfinite(epsilon) or epsilon <= 0:
        raise ValueError('invalid chain equality epsilon')
    failed = _precheck(engine, prompt, receipt, openings)
    if failed is not None:
        return failed
    steps = list(engine.score_forced(prompt, seed, [opening['token'] for opening in openings]))
    return _adjudicate(steps, openings, epsilon)


def compare(engine, prompt, seed, receipt, openings, epsilon, thinking=None):
    return compare_detailed(engine, prompt, seed, receipt, openings, epsilon, thinking)[:4]


def compare_teacher_forced(engine, prompt, seed, receipt, openings, epsilon):
    return compare_teacher_forced_detailed(engine, prompt, seed, receipt, openings, epsilon)[:4]
