"""Signed consensus grading, including pinned exact input-token evidence."""
import hashlib
import math

from ember_pb2 import OffenseGrade


_OBJECTIVE = {
    'content policy mismatch': 'format',
    'opening count': 'format',
    'invalid opening': 'format',
    'commitment mismatch': 'hash',
    'output commitment mismatch': 'hash',
    'required layers missing': 'format',
    'required topk count': 'format',
    'invalid committed logit evidence': 'format',
}


def offense_grade(verdict, distance, reason, band, hold, *, tokenizer_version=None, recomputed_input_tokens=None):
    if verdict == 'reject':
        fault = _OBJECTIVE.get(reason)
        if reason == 'input token count mismatch':
            import base64
            from ember_pb2 import ExecutionCommitment
            frozen = hold.get('token_fee') or {}
            try:
                c = ExecutionCommitment.FromString(base64.b64decode(frozen['commitment']))
            except Exception:
                return 'inconclusive', None
            if (not c.tokenizer_version or c.tokenizer_version != tokenizer_version
                    or type(recomputed_input_tokens) is not int or not 0 < recomputed_input_tokens <= 0xffffffff
                    or recomputed_input_tokens == c.input_tokens):
                return 'inconclusive', None
            return 'reject', OffenseGrade(**{'class':'objective','fault':'n_in',
                'tokenizer_version':tokenizer_version,'recomputed_input_tokens':recomputed_input_tokens})
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
