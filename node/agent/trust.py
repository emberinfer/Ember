"""Local profile trust; keep its OR semantics identical to chain.TrustPolicy."""
import json
from pathlib import Path


def check_model_trust(state, model_id, policy_path=None):
    model = state['models'][model_id]
    publication = model.get('publication')
    if publication and publication.get('invalid'):
        raise ValueError('publication is proven invalid')
    policy = json.loads(Path(policy_path).read_text()) if policy_path else {}
    if not isinstance(policy, dict) or set(policy) - {'profiles'}:
        raise ValueError('unknown trust policy field')
    if not isinstance(policy.get('profiles', {}), dict):
        raise ValueError('profiles must be a map')
    rule = policy.get('profiles', {}).get(model_id)
    if rule is None:
        if not publication:
            return
        rule = {'trusted_endorsers': [k for k, v in state['governors'].items() if v]}
    if not isinstance(rule, dict) or set(rule) - {'publishers', 'min_endorsers', 'trusted_endorsers'}:
        raise ValueError('unknown profile trust field')
    threshold = rule.get('min_endorsers', 0)
    if type(threshold) is not int or threshold < 0:
        raise ValueError('invalid endorsement threshold')
    for field in ('publishers', 'trusted_endorsers'):
        values = rule.get(field, [])
        if not isinstance(values, list) or any(not isinstance(v, str) for v in values):
            raise ValueError(field + ' must be an account list')
    if not publication:
        raise ValueError('historical profile has no publisher or endorsements for custom policy')
    if publication['publisher'] in rule.get('publishers', []):
        return
    endorsers = {k for k, v in state.get('endorsements', {}).get(
        publication['manifest_root'], {}).items() if not v.get('revoked')}
    if endorsers.intersection(rule.get('trusted_endorsers', [])):
        return
    if threshold > 0 and len(endorsers) >= threshold:
        return
    raise ValueError('profile does not satisfy local publisher/endorsement policy')
