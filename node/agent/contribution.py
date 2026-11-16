"""Operator contribution policy; profile IDs are model chain IDs."""
import json
import threading
from pathlib import Path
from ember_pb2 import ContributionSignal


def positive(value, name, zero=True):
    if type(value) is not int or value < (0 if zero else 1) or value > 2**32-1:
        raise ValueError('invalid ' + name)
    return value


def resolve(preset, profiles, capacity, memory_bytes, overrides=None):
    """Preset mapping (profiles are explicitly ordered smallest first):
    low: first profile, per-profile/global 1, no memory cap.
    balanced: all profiles, max(1, floor(capacity/2)), floor(memory_bytes/2).
    full: all profiles, per-profile/global 0 (unlimited), memory 0 (unlimited).
    Explicit profiles/global/budget_bytes replace the corresponding preset values.
    """
    if preset not in ('low', 'balanced', 'full') or not profiles or len(set(profiles)) != len(profiles) or any(not isinstance(p,str) or not p for p in profiles):
        raise ValueError('invalid contribution preset or ordered profile catalogue')
    positive(capacity, 'capacity', False)
    if type(memory_bytes) is not int or memory_bytes < 0:
        raise ValueError('invalid measured device capacity')
    limit = 1 if preset == 'low' else max(1, capacity // 2) if preset == 'balanced' else 0
    policy = {'profiles': {p: limit for p in (profiles[:1] if preset == 'low' else profiles)},
              'global': limit, 'budget_bytes': memory_bytes // 2 if preset == 'balanced' else 0}
    if overrides:
        if set(overrides) - set(policy):
            raise ValueError('unknown contribution override')
        policy.update(overrides)
    if not isinstance(policy['profiles'], dict) or any(not isinstance(p,str) or not p for p in policy['profiles']):
        raise ValueError('invalid contribution profile set')
    for limit in policy['profiles'].values():
        positive(limit, 'profile concurrency')
    positive(policy['global'], 'global concurrency')
    if type(policy['budget_bytes']) is not int or not 0 <= policy['budget_bytes'] < 2**64:
        raise ValueError('invalid contribution memory budget')
    if preset == 'balanced' and memory_bytes == 0 and not policy['budget_bytes']:
        raise ValueError('balanced requires measured device capacity or custom budget')
    return policy


class Contribution:
    def __init__(self, policy, profile, measurement=None):
        self.policy, self.profile, self.measurement = policy, profile, measurement
        self.lock = threading.Lock()
        self.active = 0

    def signal(self):
        p, m = self.policy, self.measurement
        limit = p['profiles'].get(self.profile, 0)
        budget = p['budget_bytes']
        reason = ''
        if self.profile not in p['profiles']:
            reason = 'profile excluded by operator'
        elif budget and m is None:
            reason = 'memory measurement unavailable'
        elif budget:
            # Shared weights, one measured KV reservation per concurrent job.
            slots = min(v for v in (limit, p['global']) if v) if limit or p['global'] else None
            if slots is None:
                reason = 'finite memory budget requires finite concurrency'
            elif max(m['peak_bytes'], m['weight_bytes'] + m['kv_bytes'] * slots) > budget:
                reason = 'measured weights, KV or forward peak exceed budget'
        return ContributionSignal(unavailable=bool(reason), reason=reason,
            budget_bytes=budget, max_concurrent=limit, global_concurrent=p['global'],
            **(m or {}))

    def acquire(self):
        with self.lock:
            s = self.signal()
            limit = min((v for v in (s.max_concurrent, s.global_concurrent) if v), default=0)
            if s.unavailable or (limit and self.active >= limit):
                return False
            self.active += 1
            return True

    def release(self):
        with self.lock:
            self.active -= 1


def configure(args, engine, profile):
    config = json.loads(Path(args.contribution_config).read_text()) if getattr(args, 'contribution_config', None) else {}
    if set(config) - {'preset', 'profiles', 'capacity', 'overrides'}:
        raise ValueError('unknown contribution configuration field')
    preset = getattr(args, 'contribution', None) or config.get('preset', 'full')
    total = 0
    if getattr(engine, 'device', '') == 'cuda':
        total = engine.torch.cuda.get_device_properties(0).total_memory if hasattr(engine, 'torch') else 0
    overrides = dict(config.get('overrides') or {})
    budget = getattr(args, 'vram_budget_bytes', None)
    if budget is not None:
        overrides['budget_bytes'] = budget
    policy = resolve(preset, config.get('profiles', [profile]), config.get('capacity', 1), total, overrides)
    budget = getattr(args, 'vram_budget_bytes', None)
    limit = getattr(args, 'contribution_concurrency', None)
    if budget is not None:
        policy['budget_bytes'] = budget
    if limit is not None:
        positive(limit, 'custom concurrency')
        policy['profiles'] = {p: limit for p in policy['profiles']}
        policy['global'] = limit
    measurement = None
    if policy['budget_bytes']:
        probe = getattr(engine, 'measure_contribution', None)
        if probe is not None:
            try:
                measurement = probe(getattr(args, 'contribution_context_tokens', 512))
            except MemoryError:
                measurement = None
            except RuntimeError as exc:
                if 'out of memory' not in str(exc).lower():
                    raise
                measurement = None
    contribution = Contribution(policy, profile, measurement)
    if policy['budget_bytes'] and not contribution.signal().unavailable:
        engine.set_contribution_budget(policy['budget_bytes'])
    return contribution
