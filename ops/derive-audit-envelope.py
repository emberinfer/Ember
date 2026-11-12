"""Derive a monotone replay-payment envelope from saved measurements.

Raise the output rate first; only uncovered zero-prefix rows raise overhead.
Uses exact integer consensus rounding, never fits a mean or remeasures costs.
The benchmark may retain calibration_start_audit_comp when adopting the result.
"""
import argparse
import hashlib
import json
from pathlib import Path


def ceildiv(n, d):
    return -(-n // d)


def derive(load, benchmark):
    baseline = benchmark.get('calibration_start_audit_comp', benchmark['audit_comp'])
    policy = dict(baseline)
    if (len(load['rows']) != 40 or len(benchmark['rows']) != 36
            or benchmark['subunits_per_second'] != 1000
            or policy['margin_bps'] != 12000 or policy['audit_share_bps'] != 1500):
        raise ValueError('requires frozen 40+36 observations, rho, margin and share')
    rows = []
    for source in ('load', 'benchmark'):
        for index, r in enumerate((load if source == 'load' else benchmark)['rows']):
            prefix = r['n_out_billed'] if source == 'load' else r['n_out']
            if min(r['elapsed_ns'], r['n_in'], prefix) < 0:
                raise ValueError('negative observation')
            cost = ceildiv(r['elapsed_ns'] * 1000, 1000000000)
            target = ceildiv(cost * 10000000, policy['margin_bps'])
            residual = target - 1000 * policy['o_fixed'] - policy['r_in_per_ktok'] * r['n_in']
            rows.append(dict(source=source, index=index, request_id=r.get('request_id'),
                             n_in=r['n_in'], prefix=prefix, elapsed_ns=r['elapsed_ns'],
                             cost=cost, target_numerator=target,
                             required_output_rate=ceildiv(residual, prefix) if prefix else None))
    # Derive from the 40 offered observations; benchmark remains a cross-check.
    policy['r_out_per_ktok'] = max(policy['r_out_per_ktok'],
        *(r['required_output_rate'] for r in rows if r['source'] == 'load' and r['prefix']))
    policy['o_fixed'] = max(policy['o_fixed'],
        *(ceildiv(r['target_numerator'] - policy['r_in_per_ktok'] * r['n_in'], 1000)
          for r in rows if r['source'] == 'load' and not r['prefix']), 0)

    def payment(p, r):
        return ((1000 * p['o_fixed'] + p['r_in_per_ktok'] * r['n_in']
                 + p['r_out_per_ktok'] * r['prefix']) * p['margin_bps'] // 10000000)

    for r in rows:
        r.update(old_payment=payment(baseline, r), payment=payment(policy, r))
        r['headroom'] = r['payment'] - r['cost']
        if r['headroom'] < 0:
            raise ValueError('envelope fails saved load or profile cross-check')
    if policy['r_out_per_ktok'] > baseline['r_out_per_ktok']:
        lower = dict(policy, r_out_per_ktok=policy['r_out_per_ktok'] - 1)
        assert any(payment(lower, r) < r['cost'] for r in rows if r['source'] == 'load')
    return dict(method='monotone-output-first-integer-tail-envelope',
                baseline=baseline, audit_comp=policy, rows=rows)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--load', required=True)
    p.add_argument('--benchmark', default='services/chain/testdata/audit_replay_gb10.json')
    p.add_argument('--output', required=True)
    a = p.parse_args()
    raw, benchmark_raw = Path(a.load).read_bytes(), Path(a.benchmark).read_bytes()
    result = derive(json.loads(raw), json.loads(benchmark_raw))
    result.update(source_load_sha256=hashlib.sha256(raw).hexdigest(),
                  source_benchmark_sha256=hashlib.sha256(benchmark_raw).hexdigest())
    Path(a.output).write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps({k: v for k, v in result.items() if k != 'rows'}, indent=2))


if __name__ == '__main__':
    main()
