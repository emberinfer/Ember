"""Independent two-epoch production-shaped and flood replay cost observations.

No fee tuning: the caller's signed profile supplies unchanged fee parameters.
The fixed ten prompt/prefix pairs are reused across two epochs; flood uses the
shortest pair. Measurements include engine replay and CUDA synchronization.
"""
import argparse
import hashlib
import json
from pathlib import Path
import time
from engine import TransformersEngine

p = argparse.ArgumentParser()
p.add_argument('--model', required=True)
p.add_argument('--output', required=True)
p.add_argument('--profile', default='chain-genesis/ember-qwen2.5-1.5b-gpu-v3/genesis.json')
a = p.parse_args()
profile = json.loads(Path(a.profile).read_text())
print(str(profile)[:100], flush=True)
e = TransformersEngine(a.model, 'cuda')
prompts = ['Reply briefly: test 1', 'Explain why the sky is blue.',
           'Write a short thank you note.', 'Summarize the benefits of exercise.',
           'What is a binary search?', 'Translate hello into French.',
           'List three common fruits.', 'Explain gravity to a child.',
           'Suggest a simple pasta recipe.', 'Give an example of a prime number.']
prefixes = [1, 8, 32, 8, 32, 1, 8, 32, 8, 32]
rows = []
for workload in ('production', 'flood'):
    for epoch in range(2):
        for i, prompt in enumerate(prompts):
            if workload == 'flood':
                prompt = 'Hi'
            prefix = prefixes[i] if workload == 'production' else 1
            tokens = [step.token for step in e.generate(prompt, 7, prefix)]
            prefix = len(tokens)
            e.torch.cuda.synchronize()
            start = time.perf_counter_ns()
            e.replay(prompt, 7, tokens)
            e.torch.cuda.synchronize()
            elapsed = time.perf_counter_ns()-start
            rows.append(dict(workload=workload, epoch=epoch, auditor=f'auditor-{i}',
                             request_id=f'{workload}-{epoch}-{i}', n_in=e.count_input_tokens(prompt),
                             n_out_billed=prefix, elapsed_ns=elapsed,
                             prefix_sha256=hashlib.sha256(json.dumps(tokens).encode()).hexdigest()))
            print(workload, epoch, i, elapsed, flush=True)
Path(a.output).write_text(json.dumps(dict(schema='audit-load-observations-v1',
    runtime=e.resource_metadata(), profile=a.profile,
    profile_sha256=hashlib.sha256(Path(a.profile).read_bytes()).hexdigest(),
    fee_profile=profile, rows=rows), indent=2)+'\n')
e.close()
