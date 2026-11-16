#!/usr/bin/env python3
"""Teacher-forcing audit drill (branch feat/teacher-forcing-audit).

Phases:
  gen   -- build committed-output fixtures: honest engine generation plus
           cheat executors (smaller model, fake-quantized weights, skipped
           layers). Fixtures are JSON: prompt + committed token ids.
  audit -- run both audit execution paths (regeneration replay vs
           teacher-forced scoring) over the fixtures at several committed
           lengths; record verdicts, margins, wall times, and a per-position
           numeric comparison of the two distribution sources.

The auditor side always uses the pinned honest model. Nothing here talks
to a chain or a production process; it drives the branch's own code.
"""
import argparse
import json
import time
from types import SimpleNamespace as NS

PROMPTS = [
    "Explain how photosynthesis works, in detail, covering the light reactions and the Calvin cycle step by step.",
    "Write a Python function that implements merge sort, then explain its time complexity with a worked example.",
    "Summarize the causes of the French Revolution and discuss each cause at length.",
    "A train travels 240 km in 3 hours, then 180 km in 2 hours. Compute the average speed for the whole journey, showing every step of the reasoning in detail.",
    "Translate the following into French, then explain each translation choice: 'The early bird catches the worm, but the second mouse gets the cheese.'",
    "Describe the water cycle in detail, stage by stage, including the role of energy from the sun at each stage.",
    ("Read the passage and then continue it in the same style for several paragraphs. Passage: "
     "The old lighthouse had stood on the headland for a hundred and forty years, and in that time it had "
     "watched the harbour town below it change from a fishing village of forty cottages into a port with "
     "warehouses, a railway siding, and a pier that reached a quarter of a mile into the grey water. The "
     "keepers came and went with the decades: first the naval men with their brass buttons and logbooks, "
     "then the civilians who brought families and planted gardens in the thin soil behind the tower, and "
     "finally the engineers who arrived with the automation crews and stayed only long enough to fit the "
     "new lamp and seal the door. What none of them ever changed was the rhythm of the place. At dusk the "
     "light began its slow turning, four seconds of brightness and four of dark, and the fishing boats "
     "coming home across the bar timed their approach by it, the way their grandfathers had. In winter "
     "the storms threw spray clean over the lantern room, and the whole tower hummed like a struck bell; "
     "in summer the swallows nested in the lee of the gallery and the keeper's cat slept on the warm "
     "stones of the path. The town below kept its own rhythm too: market on Thursdays, the ferry twice a "
     "day to the island, the church bell at eight on Sunday mornings whether anyone was listening or not. "
     "People who left for the cities wrote letters home asking whether the light still turned, as if the "
     "answer could tell them whether the place they remembered still existed. It did, and it does, and "
     "the light keeps turning over the water long after the last keeper's name has faded from the logbook "
     "page. Now continue the passage: the following winter brought the worst storm in living memory, and"),
]

EPSILON = 1.0


def sync(torch, device):
    if device == 'cuda':
        torch.cuda.synchronize()


def timed(fn, torch, device):
    sync(torch, device)
    start = time.perf_counter()
    out = fn()
    sync(torch, device)
    return out, time.perf_counter() - start


# ---------------------------------------------------------------- gen phase

def hf_greedy(model, tok, prompt, max_new, device):
    import torch
    ids = tok(prompt, return_tensors='pt').input_ids.to(device)
    with torch.inference_mode():
        out = model.generate(ids, do_sample=False, max_new_tokens=max_new,
                             eos_token_id=tok.eos_token_id, pad_token_id=tok.eos_token_id)
    return out[0, ids.shape[1]:].tolist()


def load_hf(path, device):
    import torch
    from transformers import AutoModelForCausalLM, AutoTokenizer
    tok = AutoTokenizer.from_pretrained(path, local_files_only=True, trust_remote_code=False)
    model = AutoModelForCausalLM.from_pretrained(path, local_files_only=True,
                                                 trust_remote_code=False,
                                                 torch_dtype=torch.bfloat16).to(device).eval()
    return model, tok


def quantize_(model, kind):
    """Fake-quantize Linear weights in place, row-block by row-block so the
    FP32 working set stays small (the lm_head matrix alone is 892 MiB in
    FP32 and this drill runs in a few GiB of free GPU memory)."""
    import torch
    with torch.no_grad():
        for module in model.modules():
            if isinstance(module, torch.nn.Linear):
                w = module.weight.data
                if kind == 'int8':
                    for start in range(0, w.shape[0], 256):
                        block = w[start:start + 256]
                        amax = block.float().abs().amax(dim=1, keepdim=True).clamp_min(1e-8)
                        scale = amax / 127.0
                        block.copy_((torch.round(block.float() / scale) * scale).to(w.dtype))
                elif kind == 'fp8':
                    module.weight.data = w.to(torch.float8_e4m3fn).to(w.dtype)
                else:
                    raise ValueError(kind)


def skip_hook(module, args, kwargs, output):
    hidden = args[0] if args else kwargs['hidden_states']
    if isinstance(output, tuple):
        return (hidden,) + output[1:]
    return hidden


def gen_phase(args):
    import gc
    import torch
    from engine.adapters.transformers import TransformersEngine

    fixtures = []

    def save():
        with open(args.out, 'w') as f:
            json.dump({'prompts': PROMPTS, 'fixtures': fixtures}, f)

    engine = TransformersEngine(args.weights, device=args.device)
    for index, prompt in enumerate(PROMPTS):
        (steps, seconds) = timed(lambda: list(engine.generate(prompt, 0, args.max_new)),
                                 torch, args.device)
        tokens = [s.token for s in steps]
        fixtures.append({'name': 'honest', 'prompt_index': index, 'tokens': tokens,
                         'gen_seconds': round(seconds, 3)})
        save()
        print(f'honest[{index}] {len(tokens)} tokens in {seconds:.1f}s', flush=True)
    engine.close()
    del engine
    gc.collect()
    if args.device == 'cuda':
        torch.cuda.empty_cache()

    model, tok = load_hf(args.weights, args.device)
    for kind in ('int8', 'fp8'):
        quantize_(model, kind)
        for index, prompt in enumerate(PROMPTS):
            tokens, seconds = timed(lambda: hf_greedy(model, tok, prompt, args.max_new, args.device),
                                    torch, args.device)
            fixtures.append({'name': f'quant-{kind}', 'prompt_index': index, 'tokens': tokens,
                             'gen_seconds': round(seconds, 3)})
            save()
            print(f'quant-{kind}[{index}] {len(tokens)} tokens in {seconds:.1f}s', flush=True)
        del model
        gc.collect()
        if args.device == 'cuda':
            torch.cuda.empty_cache()
        model, tok = load_hf(args.weights, args.device)

    handles = [model.model.layers[i].register_forward_hook(skip_hook, with_kwargs=True)
               for i in range(14, 21)]
    for index, prompt in enumerate(PROMPTS):
        tokens, seconds = timed(lambda: hf_greedy(model, tok, prompt, args.max_new, args.device),
                                torch, args.device)
        fixtures.append({'name': 'layerskip-14-20', 'prompt_index': index, 'tokens': tokens,
                         'gen_seconds': round(seconds, 3)})
        save()
        print(f'layerskip[{index}] {len(tokens)} tokens in {seconds:.1f}s', flush=True)
    for handle in handles:
        handle.remove()
    del model
    gc.collect()
    if args.device == 'cuda':
        torch.cuda.empty_cache()

    if args.small_weights:
        small, stok = load_hf(args.small_weights, args.device)
        for index, prompt in enumerate(PROMPTS):
            tokens, seconds = timed(lambda: hf_greedy(small, stok, prompt, args.max_new, args.device),
                                    torch, args.device)
            fixtures.append({'name': 'small-0.5b', 'prompt_index': index, 'tokens': tokens,
                             'gen_seconds': round(seconds, 3)})
            save()
            print(f'small[{index}] {len(tokens)} tokens in {seconds:.1f}s', flush=True)

    save()
    print('wrote', args.out, len(fixtures), 'fixtures')


# -------------------------------------------------------------- audit phase

def audit_phase(args):
    import torch
    from auditor.equality import compare, compare_teacher_forced
    from engine.adapters.transformers import TransformersEngine

    with open(args.fixtures) as f:
        data = json.load(f)
    prompts, fixtures = data['prompts'], data['fixtures']
    lengths = [int(x) for x in args.lengths.split(',')]
    engine = TransformersEngine(args.weights, device=args.device)
    rows, probes = [], []

    for fixture in fixtures:
        prompt = prompts[fixture['prompt_index']]
        tokens = fixture['tokens']
        for length in lengths:
            if length > len(tokens):
                continue
            committed = tokens[:length]
            receipt = NS(input_tokens=engine.count_input_tokens(prompt), output_tokens=length)
            openings = [{'token': t} for t in committed]
            reps = args.repeats if fixture['name'] == 'honest' else 1
            regen_times, tf_times, regen, forced = [], [], None, None
            for _ in range(reps):
                regen, dt = timed(lambda: compare(engine, prompt, 0, receipt, openings, EPSILON),
                                  torch, args.device)
                regen_times.append(dt)
                forced, dt = timed(lambda: compare_teacher_forced(engine, prompt, 0, receipt, openings, EPSILON),
                                   torch, args.device)
                tf_times.append(dt)
            row = {'fixture': fixture['name'], 'prompt_index': fixture['prompt_index'],
                   'length': length,
                   'regen': {'verdict': regen[0], 'margin': regen[1], 'count': regen[3],
                             'seconds': round(sorted(regen_times)[len(regen_times) // 2], 4)},
                   'tf': {'verdict': forced[0], 'margin': forced[1], 'count': forced[3],
                          'seconds': round(sorted(tf_times)[len(tf_times) // 2], 4)},
                   'agree': regen[0] == forced[0] and regen[3] == forced[3]}
            rows.append(row)
            print(row['fixture'], row['prompt_index'], length, row['regen']['verdict'],
                  row['tf']['verdict'], 'agree' if row['agree'] else 'DISAGREE',
                  f"regen={row['regen']['seconds']}s tf={row['tf']['seconds']}s", flush=True)

    # Truncation claim: openings shorter than the receipt claims.
    base = next(f for f in fixtures if f['name'] == 'honest')
    prompt = prompts[base['prompt_index']]
    committed = base['tokens'][:64]
    receipt = NS(input_tokens=engine.count_input_tokens(prompt), output_tokens=len(committed) + 17)
    openings = [{'token': t} for t in committed]
    regen = compare(engine, prompt, 0, receipt, openings, EPSILON)
    forced = compare_teacher_forced(engine, prompt, 0, receipt, openings, EPSILON)
    rows.append({'fixture': 'trunc-claim', 'prompt_index': base['prompt_index'], 'length': len(committed),
                 'regen': {'verdict': regen[0], 'margin': regen[1], 'count': regen[3], 'seconds': 0},
                 'tf': {'verdict': forced[0], 'margin': forced[1], 'count': forced[3], 'seconds': 0},
                 'agree': regen[0] == forced[0]})
    print('trunc-claim', regen[0], forced[0], flush=True)

    # Numeric-path probe: step-by-step forced replay vs one-pass scoring.
    for fixture in [f for f in fixtures if f['name'] == 'honest'][:3]:
        prompt = prompts[fixture['prompt_index']]
        for length in lengths:
            if length > len(fixture['tokens']):
                continue
            committed = fixture['tokens'][:length]
            replay_steps = engine.replay(prompt, 0, committed)
            tf_steps = engine.score_forced(prompt, 0, committed)
            flips, deltas, committed_deltas = 0, [], []
            for a, b in zip(replay_steps, tf_steps):
                if a.token != b.token:
                    flips += 1
                da = dict(a.topk)
                db = dict(b.topk)
                if a.token in db:
                    deltas.append(abs(da[a.token] - db[a.token]))
            for i, (a, b) in enumerate(zip(replay_steps, tf_steps)):
                da, db = dict(a.topk), dict(b.topk)
                if committed[i] in da and committed[i] in db:
                    committed_deltas.append(abs(da[committed[i]] - db[committed[i]]))
            probes.append({'prompt_index': fixture['prompt_index'], 'length': length,
                           'positions': min(len(replay_steps), len(tf_steps)),
                           'argmax_flips': flips,
                           'max_argmax_score_delta': max(deltas) if deltas else None,
                           'max_committed_score_delta': max(committed_deltas) if committed_deltas else None})
            print('probe', probes[-1], flush=True)

    engine.close()
    env = {'device': args.device, 'torch': torch.__version__,
           'device_name': torch.cuda.get_device_name(0) if args.device == 'cuda' else 'cpu'}
    with open(args.out, 'w') as f:
        json.dump({'env': env, 'rows': rows, 'probes': probes}, f, indent=1)
    print('wrote', args.out)


def main():
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers(dest='phase', required=True)
    g = sub.add_parser('gen')
    g.add_argument('--weights', required=True)
    g.add_argument('--small-weights')
    g.add_argument('--device', default='cuda')
    g.add_argument('--max-new', type=int, default=512)
    g.add_argument('--out', required=True)
    a = sub.add_parser('audit')
    a.add_argument('--weights', required=True)
    a.add_argument('--device', default='cuda')
    a.add_argument('--fixtures', required=True)
    a.add_argument('--lengths', default='32,128,256,512')
    a.add_argument('--repeats', type=int, default=3)
    a.add_argument('--out', required=True)
    args = parser.parse_args()
    (gen_phase if args.phase == 'gen' else audit_phase)(args)


if __name__ == '__main__':
    main()
