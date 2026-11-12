"""Signed consensus grading; input-token adjudication is deliberately deferred."""
import hashlib
import math

from ember_pb2 import OffenseGrade


_OBJECTIVE = {
    'opening count': 'format',
    'invalid opening': 'format',
    'commitment mismatch': 'hash',
    'output commitment mismatch': 'hash',
    'required layers missing': 'format',
    'required topk count': 'format',
    'invalid committed logit evidence': 'format',
}


def offense_grade(verdict, distance, reason, band, hold):
    if verdict == 'reject':
        fault = _OBJECTIVE.get(reason)
        # No tokenizer registry or N_in verdict exists in this implementation.
        if fault is None:
            return 'inconclusive', None
        return 'reject', OffenseGrade(**{'class': 'objective', 'fault': fault})
    if verdict != 'dispute':
        return verdict, None
    if not math.isfinite(distance) or distance < 0:
        return 'inconclusive', None
    band_id = hashlib.sha256(band.SerializeToString(deterministic=True)).hexdigest()
    for pair in hold.get('misjudge_thresholds', []):
        if (pair['model'] == hold.get('model_id') and pair['band'] == band_id
                and pair['min_output'] <= hold['audit_output_tokens'] <= pair['max_output']):
            return 'reject', OffenseGrade(**{'class': 'numerical', 'score': distance,
                'band': band_id, 'length_class': pair['length_class']})
    # Unconfigured pairs cannot authorize numerical penalties.
    return 'inconclusive', None
