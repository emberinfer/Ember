"""Versioned numerical comparisons; sparse missing logits remain censored.

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
        return math.inf, {'reason': 'reference count'}
    logit_max, activation_max, missing, union_count = 0.0, 0.0, 0, 0
    activation_by_layer = {}
    rank_max, probability_tv_max, overlap_min, argmax_disagreements = 0.0, 0.0, 1.0, 0
    for a, b in zip(actual, expected):
        if a.score_kind != 'raw-logits' or b.score_kind != 'raw-logits':
            return math.inf, {'reason': 'score kind'}
        try:
            score_map(a.topk); score_map(b.topk)
        except (ValueError, TypeError):
            return math.inf, {'reason': 'invalid logit evidence'}
        if set(a.activations) != set(b.activations):
            return math.inf, {'reason': 'layer mismatch'}
        if metric == LEGACY:
            if [t[0] for t in a.topk] != [t[0] for t in b.topk]:
                return math.inf, {'reason': 'topk/layer mismatch'}
            logits = legacy_distance([v for _, v in a.topk], [v for _, v in b.topk])
        elif metric in (UNION, RANK_PROB):
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
        for layer, raw in a.activations.items():
            other = b.activations[layer]
            if metric == LEGACY and raw == other:
                continue  # preserves old exact-byte synthetic mock behavior
            if len(raw) != len(other) or not raw or len(raw) % 4:
                return math.inf, {'reason': 'activation shape'}
            values = struct.unpack('<' + 'f' * (len(raw) // 4), raw)
            reference = struct.unpack('<' + 'f' * (len(other) // 4), other)
            if not all(math.isfinite(v) for v in (*values, *reference)):
                return math.inf, {'reason': 'non-finite activation'}
            value = (legacy_distance(values, reference) if metric == LEGACY else
                     relative_distance(values, reference))
            if not math.isfinite(value):
                return math.inf, {'reason': 'non-finite activation'}
            activation_max = max(activation_max, value)
            activation_by_layer[layer] = max(activation_by_layer.get(layer, 0.0), value)
    # The value checks constrain transformations that preserve rank/softmax
    # (e.g. arbitrary additive logit shifts). Layer evidence is still required.
    value = max(logit_max, activation_max)
    if metric == RANK_PROB:
        # Versioned component scales separate loose layer-value tolerance
        # from stricter output/rank checks. One signed request threshold still
        # controls the conjunction; a band cannot silently change these scales.
        value = max(rank_max/.4, probability_tv_max/.5,
                    logit_max/.15, activation_max)
    return value, {'reason': 'numerical comparison',
        'logit_max': logit_max, 'activation_max': activation_max,
        'activation_by_layer': activation_by_layer,
        'censored_union_entries': missing, 'union_entries': union_count,
        'rank_max': rank_max, 'probability_tv_max': probability_tv_max,
        'topk_overlap_min': overlap_min, 'argmax_disagreements': argmax_disagreements}
