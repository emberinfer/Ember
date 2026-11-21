"""Versioned numerical comparisons; sparse missing logits remain censored.

Steps committed as logprobs are unified with raw-logits references via exact
peak-relative gaps (the representations differ only by a logsumexp constant)
before the relative-L2 comparison; unknown score kinds fail closed.

The union metric compares singleton values with (-inf, kth_score] intervals.
Its numerator is the minimum possible L2 gap between those intervals. The scale
uses observed scores with kth-score censoring, not invented measured logits.
This is a screening statistic, not the unknown dense-vocabulary L2 distance.
"""
import math
import struct

LEGACY = 'legacy-ranked-v1'
UNION = 'union-interval-v1'
RANK_PROB = 'rank-probability-v1'
RELATIVE_L2 = 'relative-l2-v1'

# The two committed score representations. raw-logits are LM-head outputs;
# logprobs are the same distribution after log-softmax, i.e. they differ only
# by the per-step logsumexp constant. Anything else stays unrecognized and
# comparisons fail closed.
SCORE_KINDS = ('raw-logits', 'logprobs')


def peak_relative(values):
    """Peak-relative scores: each value minus the list maximum.

    Log-softmax gaps are representation-free: for raw logits l and their
    logprobs l - logsumexp(l), v - max(v) is identical. Expressing both sides
    this way therefore unifies the representations exactly, with no estimated
    constant, and the relative-L2 distance is measured on the gaps.
    """
    peak = max(values)
    return [v - peak for v in values]


def topk_statistics(left, right):
    """Rank and probability comparisons of the observed sparse candidates.

    Spearman ranks use average ranks for ties and a tied last rank for absent
    candidates. TV is between *conditional top-k* softmax distributions, with
    zero mass outside each reported set; it is not full-vocabulary TV.
    """
    left, right = score_map(left), score_map(right)
    tokens = sorted(left.keys() | right.keys())

    def ranks(scores):
        ordered = sorted(scores, key=lambda t: -scores[t])
        result, start = {}, 0
        while start < len(ordered):
            end = start+1
            while end < len(ordered) and scores[ordered[end]] == scores[ordered[start]]:
                end += 1
            for token in ordered[start:end]:
                result[token] = (start+1+end)/2
            start = end
        absent_rank = (len(scores)+1+len(tokens))/2
        return [result.get(t, absent_rank) for t in tokens]

    a, b = ranks(left), ranks(right)
    mean_a, mean_b = sum(a)/len(a), sum(b)/len(b)
    da, db = [x-mean_a for x in a], [x-mean_b for x in b]
    scale = math.hypot(*da)*math.hypot(*db)
    rho = (sum(x*y for x, y in zip(da, db))/scale if scale else
           (1.0 if a == b else 0.0))

    def probabilities(scores):
        peak = max(scores.values())
        values = {t: math.exp(v-peak) for t, v in scores.items()}
        total = sum(values.values())
        return {t: v/total for t, v in values.items()}

    pa, pb = probabilities(left), probabilities(right)
    tv = sum(abs(pa.get(t, 0)-pb.get(t, 0)) for t in tokens)/2
    maxima_a = {t for t, v in left.items() if v == max(left.values())}
    maxima_b = {t for t, v in right.items() if v == max(right.values())}
    return {'rank_distance': max(0.0, min(1.0, (1-rho)/2)),
            'probability_tv': tv,
            'topk_overlap': len(left.keys() & right.keys())/max(len(left), len(right)),
            'argmax_agreement': bool(maxima_a & maxima_b)}


def relative_distance(left, right):
    if len(left) != len(right) or not left:
        return math.inf
    if not all(math.isfinite(v) for v in [*left, *right]):
        return math.inf
    return math.hypot(*(a-b for a, b in zip(left, right))) / max(math.hypot(*right), 1e-12)


def legacy_distance(left, right):
    if len(left) != len(right) or not left:
        return math.inf
    return math.sqrt(sum((a-b)**2 for a, b in zip(left, right))) / max(
        math.sqrt(sum(b*b for b in right)), 1e-12)


def score_map(pairs):
    if not pairs or len(pairs) > 1024:
        raise ValueError('invalid topk count')
    result = {}
    for token, value in pairs:
        if type(token) is not int or token < 0 or token in result:
            raise ValueError('invalid or duplicate topk token')
        if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value) or abs(value) > 3.4028234663852886e38:
            raise ValueError('non-finite or invalid float32 logit')
        result[token] = float(value)
    return result


def union_distance(left, right):
    left, right = score_map(left), score_map(right)
    union = sorted(left.keys() | right.keys())
    left_floor, right_floor = min(left.values()), min(right.values())
    gaps, left_scale, right_scale = [], [], []
    for token in union:
        a, b = left.get(token, left_floor), right.get(token, right_floor)
        left_scale.append(a)
        right_scale.append(b)
        if token in left and token in right:
            gaps.append(a-b)
        elif token in left:
            gaps.append(max(0.0, a-right_floor))
        else:
            gaps.append(max(0.0, b-left_floor))
    value = math.hypot(*gaps) / max(math.hypot(*left_scale), math.hypot(*right_scale), 1e-12)
    return (value if math.isfinite(value) else math.inf), len(left.keys() ^ right.keys()), len(union)


def compare(actual, expected, metric=LEGACY):
    if len(actual) != len(expected) or not actual:
        return math.inf, {'reason': 'reference count', 'position_values': [0.0] * min(len(actual), len(expected)) + [math.inf], 'failure_index': min(len(actual), len(expected))}
    logit_max, activation_max, missing, union_count = 0.0, 0.0, 0, 0
    activation_by_layer = {}
    rank_max, probability_tv_max, overlap_min, argmax_disagreements = 0.0, 0.0, 1.0, 0
    position_values = []
    def failure(reason, index):
        return math.inf, {'reason': reason, 'position_values': position_values + [math.inf], 'failure_index': index}
    for index, (a, b) in enumerate(zip(actual, expected)):
        position_activation = 0.0
        if a.score_kind not in SCORE_KINDS or b.score_kind not in SCORE_KINDS:
            return failure('score kind', index)
        try:
            a_scores = score_map(a.topk); b_scores = score_map(b.topk)
        except (ValueError, TypeError):
            return failure('invalid logit evidence', index)
        # Every layer the reference measured must be present in the committed
        # evidence; extra committed layers (stage boundaries a full-model
        # reference does not hook) are bound by the commitment, not compared.
        if not set(b.activations) <= set(a.activations):
            return failure('layer mismatch', index)
        # A logprobs side and a raw-logits side are the same computation in
        # two representations; unify them exactly via peak-relative gaps.
        # Two raw-logits sides keep the signed ordered comparison unchanged.
        normalized = a.score_kind == 'logprobs' or b.score_kind == 'logprobs'
        if metric in (LEGACY, RELATIVE_L2):
            if normalized:
                if set(a_scores) != set(b_scores):
                    return failure('topk/layer mismatch', index)
                tokens = [t for t, _ in b.topk]
                logits = legacy_distance(
                    peak_relative([a_scores[t] for t in tokens]),
                    peak_relative([b_scores[t] for t in tokens]))
            else:
                if [t[0] for t in a.topk] != [t[0] for t in b.topk]:
                    return failure('topk/layer mismatch', index)
                logits = legacy_distance([v for _, v in a.topk], [v for _, v in b.topk])
        elif metric in (UNION, RANK_PROB):
            # Interval/rank statistics are representation-bound; no signed
            # calibration covers mixed representations, so stay fail-closed.
            if a.score_kind != b.score_kind:
                return failure('score kind', index)
            logits, absent, total = union_distance(a.topk, b.topk)
            missing += absent
            union_count += total
            if metric == RANK_PROB:
                stats = topk_statistics(a.topk, b.topk)
                rank_max = max(rank_max, stats['rank_distance'])
                probability_tv_max = max(probability_tv_max, stats['probability_tv'])
                overlap_min = min(overlap_min, stats['topk_overlap'])
                argmax_disagreements += not stats['argmax_agreement']
        else:
            raise ValueError('unknown numeric metric')
        logit_max = max(logit_max, logits)
        for layer, other in b.activations.items():
            raw = a.activations[layer]
            if metric == LEGACY and raw == other:
                continue  # preserves old exact-byte synthetic mock behavior
            if len(raw) != len(other) or not raw or len(raw) % 4:
                return failure('activation shape', index)
            values = struct.unpack('<' + 'f' * (len(raw) // 4), raw)
            reference = struct.unpack('<' + 'f' * (len(other) // 4), other)
            if not all(math.isfinite(v) for v in (*values, *reference)):
                return failure('non-finite activation', index)
            value = (legacy_distance(values, reference) if metric == LEGACY else
                     relative_distance(values, reference))
            if not math.isfinite(value):
                return failure('non-finite activation', index)
            position_activation = max(position_activation, value)
            activation_max = max(activation_max, value)
            activation_by_layer[layer] = max(activation_by_layer.get(layer, 0.0), value)
        position_value = max(logits, position_activation)
        if metric == RANK_PROB:
            position_value = max(stats['rank_distance']/.4, stats['probability_tv']/.5,
                                 logits/.15, position_activation)
        position_values.append(position_value)
    # The value checks constrain transformations that preserve rank/softmax
    # (e.g. arbitrary additive logit shifts). Layer evidence is still required.
    value = max(logit_max, activation_max)
    if metric == RANK_PROB:
        # Versioned component scales separate loose layer-value tolerance
        # from stricter output/rank checks. One signed request threshold still
        # controls the conjunction; a band cannot silently change these scales.
        value = max(rank_max/.4, probability_tv_max/.5,
                    logit_max/.15, activation_max)
    return value, {'reason': 'numerical comparison', 'position_values': position_values,
        'logit_max': logit_max, 'activation_max': activation_max,
        'activation_by_layer': activation_by_layer,
        'censored_union_entries': missing, 'union_entries': union_count,
        'rank_max': rank_max, 'probability_tv_max': probability_tv_max,
        'topk_overlap_min': overlap_min, 'argmax_disagreements': argmax_disagreements}
