"""Measure forced-prefix replay; output a provenance-bearing governance fixture.

Run with the CUDA-enabled interpreter and PYTHONPATH=node:core-py:proto.
Subunits per second is an explicit accounting assumption, not a market price.
"""
import argparse
import hashlib
import json
import math
from pathlib import Path
import time
from engine import TransformersEngine

p = argparse.ArgumentParser()
p.add_argument('--model', required=True)
p.add_argument('--output', required=True)
p.add_argument('--subunits-per-second', type=int, default=1000)
a = p.parse_args()
e = TransformersEngine(a.model, 'cuda')
# Fixed grid selected before measurement; no workload or fit tuning.
rows = []
for repeat in range(3):
    for words in (8, 32, 128):
        prompt = 'Hello ' * words
        n_in = e.count_input_tokens(prompt)
        for n_out in (0, 1, 8, 32):
            tokens = [42] * n_out
            e.torch.cuda.synchronize()
            start = time.perf_counter_ns()
            result = e.replay(prompt, 7, tokens)
            e.torch.cuda.synchronize()
            elapsed = time.perf_counter_ns() - start
            assert len(result) == n_out
            rows.append(dict(repeat=repeat, n_in=n_in, n_out=n_out, elapsed_ns=elapsed))
# Nonnegative least-squares approximation, followed by a fixed residual envelope.
# Algorithm fixed before any P&L load; the fit is a policy estimate, not a guarantee.
import numpy as np
rate = a.subunits_per_second
x = np.array([[1, r['n_in']/1000, r['n_out']/1000] for r in rows])
y = np.array([r['elapsed_ns']*rate/1e9 for r in rows])
beta = np.maximum(0, np.linalg.lstsq(x, y, rcond=None)[0])
r_in, r_out = math.ceil(beta[1]), math.ceil(beta[2])
fixed = max(1, math.ceil(max(y - x[:,1]*r_in - x[:,2]*r_out)))
f = dict(schema='audit-replay-benchmark-v1', profile='qwen2.5-1.5b-gb10', runtime=e.resource_metadata(), model=a.model,
         model_config_sha256=hashlib.sha256((Path(a.model)/'config.json').read_bytes()).hexdigest(),
         subunits_per_second=rate, conversion_notice='Accounting assumption; no monetary exchange-rate claim.', rows=rows,
         derivation='OLS on [1,n_in/1000,n_out/1000], clamp negative coefficients to zero, ceil token rates; o_fixed=max(1,ceil(max observed cost minus token terms)). Conservative residual envelope; not independent validation',
         audit_comp=dict(formula_ver=2,o_fixed=fixed,r_in_per_ktok=r_in,r_out_per_ktok=r_out,margin_bps=12000,audit_share_bps=1500))
Path(a.output).write_text(json.dumps(f,indent=2)+'\n')
e.close()
print(json.dumps(f['audit_comp']))
