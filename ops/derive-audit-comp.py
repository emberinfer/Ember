"""Fit audit cost from all saved load and benchmark rows, without remeasurement.

Uses exact rational OLS and the pre-registered negative-coefficient floor/refit
rule. Outputs local derivation evidence; does not edit policy or input files.
"""
import argparse
from fractions import Fraction
import hashlib
import json
import math
from pathlib import Path


def solve(matrix, rhs):
    a = [list(map(Fraction, row)) + [Fraction(y)]
         for row, y in zip(matrix, rhs)]
    for i in range(len(a)):
        pivot = next(j for j in range(i, len(a)) if a[j][i])
        a[i], a[pivot] = a[pivot], a[i]
        scale = a[i][i]
        a[i] = [v / scale for v in a[i]]
        for j in range(len(a)):
            if j != i:
                scale = a[j][i]
                a[j] = [v - scale*w for v, w in zip(a[j], a[i])]
    return [row[-1] for row in a]


def fit(rows):
    x = [[1, r['n_in'], r['n_prefix']] for r in rows]
    y = [r['cost'] for r in rows]
    active = [0, 1, 2]
    rounds = []
    while True:
        coeff = solve([[sum(v[i]*v[j] for v in x) for j in active]
                       for i in active],
                      [sum(v[i]*c for v, c in zip(x, y)) for i in active])
        beta = [Fraction(0)]*3
        for i, c in zip(active, coeff):
            beta[i] = c
        rounds.append(dict(active=active[:], coefficients=list(map(float, beta)),
                           exact_coefficients=list(map(str, beta))))
        negative = [i for i in active if beta[i] < 0]
        if not negative:
            break
        active = [i for i in active if i not in negative]
    estimates = [sum(c*v for c, v in zip(beta, row)) for row in x]
    mean = Fraction(sum(y), len(y))
    sse = sum((c-est)**2 for c, est in zip(y, estimates))
    sst = sum((c-mean)**2 for c in y)
    for r, est in zip(rows, estimates):
        r.update(estimate=float(est), residual=float(r['cost']-est))
    residuals = sorted(abs(r['residual']) for r in rows)

    def percentile(p):
        index = (len(residuals)-1)*p
        lo = math.floor(index)
        hi = math.ceil(index)
        return residuals[lo]+(residuals[hi]-residuals[lo])*(index-lo)

    return dict(rounds=rounds, r_squared=float(1-sse/sst),
                absolute_residuals=dict(p50=percentile(.5), p95=percentile(.95),
                                        max=residuals[-1]),
                audit_comp=dict(formula_ver=2, o_fixed=math.ceil(beta[0]),
                                r_in_per_ktok=math.ceil(beta[1]*1000),
                                r_out_per_ktok=math.ceil(beta[2]*1000),
                                margin_bps=12000, audit_share_bps=1500), rows=rows)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--load', required=True)
    p.add_argument('--benchmark', default='services/chain/testdata/audit_replay_gb10.json')
    p.add_argument('--output', required=True)
    a = p.parse_args()
    load_raw = Path(a.load).read_bytes()
    benchmark_raw = Path(a.benchmark).read_bytes()
    load, benchmark = json.loads(load_raw), json.loads(benchmark_raw)
    rho = benchmark['subunits_per_second']
    if rho != 1000 or len(load['rows']) != 40 or len(benchmark['rows']) != 36:
        raise ValueError('pre-registered inputs require rho=1000 and 40+36 rows')
    rows = []
    for source, data in [('load', load['rows']), ('benchmark', benchmark['rows'])]:
        for index, r in enumerate(data):
            if r['elapsed_ns'] < 0:
                raise ValueError('negative measurement')
            rows.append(dict(source=source, index=index,
                             request_id=r.get('request_id'), n_in=r['n_in'],
                             n_prefix=r.get('n_out_billed', r.get('n_out')),
                             elapsed_ns=r['elapsed_ns'],
                             cost=(r['elapsed_ns']*rho+999999999)//1000000000))
    result = fit(rows)
    result.update(source_load_sha256=hashlib.sha256(load_raw).hexdigest(),
                  source_benchmark_sha256=hashlib.sha256(benchmark_raw).hexdigest(),
                  cost_basis='ceil(elapsed_ns*1000/1e9); equal row weights; '
                  'absolute residual percentiles use linear interpolation')
    Path(a.output).write_text(json.dumps(result, indent=2)+'\n')
    print(json.dumps({k: v for k, v in result.items() if k != 'rows'}, indent=2))


if __name__ == '__main__':
    main()
