"""Derive equal input/output prices from saved replay costs without remeasurement.

Writes a local accounting scenario, not a signed or deployable genesis profile.
The audit share must cover ceil(margin * ceil(elapsed_ns * rho / 1e9))
for EACH observed request. Context price remains unchanged (must be zero).
"""
import argparse
import copy
import hashlib
import json
from pathlib import Path


def ceildiv(n, d):
    return (n + d - 1) // d


def calibrate(load, benchmark):
    policy = benchmark['audit_comp']
    rho = benchmark['subunits_per_second']
    margin, share = policy['margin_bps'], policy['audit_share_bps']
    if rho <= 0 or margin <= 0 or not 0 < share <= 10000:
        raise ValueError('invalid conversion, margin or share')
    if int(load['fee_profile']['genesis'].get('price_context', 0)) != 0:
        raise ValueError('calibration requires zero context price')
    rows = []
    for row in load['rows']:
        tokens = row['n_in'] + row['n_out_billed']
        if tokens <= 0 or row['elapsed_ns'] < 0:
            raise ValueError('positive token count and nonnegative elapsed time required')
        cost = ceildiv(row['elapsed_ns'] * rho, 1000000000)
        target = ceildiv(cost * margin, 10000)
        minimum_fee = ceildiv(target * 10000, share)
        price = ceildiv(minimum_fee, tokens)
        rows.append(dict(request_id=row['request_id'], workload=row['workload'],
                         tokens=tokens, cost=cost, target=target,
                         minimum_fee=minimum_fee, minimum_equal_price=price))
    price = max(row['minimum_equal_price'] for row in rows)
    # Verify both funding and minimality using exact integer budget arithmetic.
    assert all(price * r['tokens'] * share // 10000 >= r['target'] for r in rows)
    assert any((price-1) * r['tokens'] * share // 10000 < r['target'] for r in rows)
    scenario = copy.deepcopy(load)
    for key in ('price_input', 'price_output'):
        scenario['fee_profile']['genesis'][key] = str(price)
    return scenario, dict(equal_token_price=price, rho=rho, margin_bps=margin,
                          audit_share_bps=share, rows=rows,
                          context='Saved measurements; counterfactual fee table only. '
                          'Source profile metadata identifies the original profile, '
                          'not a signature or hash of this derived price scenario.')


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--load', required=True)
    p.add_argument('--benchmark', default='services/chain/testdata/audit_replay_gb10.json')
    p.add_argument('--output', required=True)
    p.add_argument('--derivation', required=True)
    a = p.parse_args()
    raw = Path(a.load).read_bytes()
    benchmark_raw = Path(a.benchmark).read_bytes()
    scenario, derivation = calibrate(json.loads(raw), json.loads(benchmark_raw))
    derivation['source_load_sha256'] = hashlib.sha256(raw).hexdigest()
    derivation['source_benchmark_sha256'] = hashlib.sha256(benchmark_raw).hexdigest()
    Path(a.output).write_text(json.dumps(scenario, indent=2)+'\n')
    Path(a.derivation).write_text(json.dumps(derivation, indent=2)+'\n')
    print(json.dumps({k: v for k, v in derivation.items() if k != 'rows'}, indent=2))


if __name__ == '__main__':
    main()
