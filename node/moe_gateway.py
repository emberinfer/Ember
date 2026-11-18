"""One-request MoE executor; stdout is exclusively NDJSON protocol events.

Streaming (2026-11-17, morning incident line item 5): token events are
emitted as each token is produced (MoeAssembler on_token callback), using
the exact incremental-decode logic the old post-generation loop used, so
the emitted event sequence is unchanged; only its timing moves. Set
EMBER_MOE_STREAM=0 to restore the old generate-then-emit behaviour.
"""
import base64
import contextlib
import json
import os
from pathlib import Path
import sys
import threading
import time
import resource

ROOT = Path(os.environ.get('EMBER_MOE_TREE', Path(__file__).resolve().parent.parent))
sys.path[:0] = [str(ROOT / p) for p in ('proto', 'core-py', 'node')]


def lifecycle_marker(event, **fields):
    # An allowlisted schema: never log request IDs, exception text, or inputs.
    payload = {'channel': 'moe_executor_lifecycle', 'event': event,
               'pid': os.getpid(), 'monotonic_ns': time.monotonic_ns()}
    payload.update(fields)
    try:
        print(json.dumps(payload, sort_keys=True), file=sys.stderr, flush=True)
    except (OSError, ValueError):
        pass  # Diagnostics must not change protocol behavior.


def resource_sample():
    sample = {'rss_peak_bytes': resource.getrusage(resource.RUSAGE_SELF).ru_maxrss * 1024}
    for base, usage, events in (
            ('/sys/fs/cgroup', 'memory.current', 'memory.events'),
            ('/sys/fs/cgroup/memory', 'memory.usage_in_bytes', 'memory.oom_control')):
        try:
            memory = int((Path(base) / usage).read_text())
            counters = dict(line.split() for line in (Path(base) / events).read_text().splitlines())
            sample.update(cgroup_path=base, memory_current=memory,
                          oom_kill=int(counters['oom_kill']))
            break
        except (OSError, ValueError, KeyError):
            continue
    return sample


def run_executor(entry=None):
    stop = threading.Event()
    def periodic():
        while not stop.wait(1.0):
            lifecycle_marker('resource', **resource_sample())
    sampler = threading.Thread(target=periodic, name='moe-resource-sampler', daemon=True)
    lifecycle_marker('start', **resource_sample())
    sampler.start()
    try:
        (entry or main)()
    except BaseException as exc:
        lifecycle_marker('error', error_type=type(exc).__name__, **resource_sample())
        raise
    else:
        lifecycle_marker('done', **resource_sample())
    finally:
        stop.set()
        sampler.join(timeout=2)


def main():
    lifecycle_marker('phase', phase='read_input')
    q = json.load(sys.stdin)
    stream = os.environ.get('EMBER_MOE_STREAM', '1') != '0'
    real_stdout = sys.stdout
    state = {'previous': '', 'emitted': 0}
    with contextlib.redirect_stdout(sys.stderr):
        from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
        from transformers import AutoTokenizer
        from ember_pb2 import ModelManifest, ChainGenesis, ResourceProfile
        from moe_rpc import MoeAttentionClient, MoeExpertClient
        from moe_client import MoeAssembler
        base = Path(q.get('fleet_dir', '/root/work/act2e-nodes'))
        release = Path('/root/work/act2e-pubmoe/release')
        fleet = json.loads((base / 'fleet.json').read_text())
        key = Ed25519PrivateKey.from_private_bytes((base / 'assembler.key').read_bytes())
        profiles = {n: ResourceProfile.FromString((base / n / 'profile.pb').read_bytes()) for n in fleet}
        stages = [(MoeAttentionClient(fleet[n]['target'], key, profile=profiles[n]), profiles[n]) for n in ('stage-a', 'stage-b')]
        experts = {fleet[n]['node_id']: MoeExpertClient(fleet[n]['target'], key, profile=profiles[n]) for n in ('exp-even-1', 'exp-odd-1')}
        tokenizer = AutoTokenizer.from_pretrained(q['weights'], local_files_only=True)
        assembler = MoeAssembler(stages, tokenizer, ModelManifest.FromString((release / 'manifest.pb').read_bytes()), ChainGenesis.FromString((release / 'genesis.pb').read_bytes()), expert_node_clients=experts, receipt_v2=q.get('receipt_v2', False))

        def emit_tokens(tokens_so_far):
            # Buffer incomplete UTF-8 sequences until the next token.
            decoded = tokenizer.decode(tokens_so_far, skip_special_tokens=True).rstrip('\ufffd')
            if not decoded.startswith(state['previous']):
                raise ValueError('non-monotonic incremental decoding')
            print(json.dumps({'type': 'token', 'text': decoded[len(state['previous']):]}), file=real_stdout, flush=True)
            state['previous'] = decoded
            state['emitted'] += 1

        lifecycle_marker('phase', phase='generate')
        if stream:
            receipt, evidence = assembler.generate(q['request_id'], q['prompt'], q['max_tokens'], on_token=emit_tokens)
        else:
            receipt, evidence = assembler.generate(q['request_id'], q['prompt'], q['max_tokens'])
        lifecycle_marker('phase', phase='save_evidence')
        directory = Path(q['evidence_root']) / q['request_id']
        assembler.save_evidence(directory, receipt, evidence, q['prompt'], tokenizer(q['prompt']).input_ids)
    if stream:
        assert state['emitted'] == len(evidence['tokens']), 'streamed token count mismatch'
    else:
        previous = ''
        for i in range(len(evidence['tokens'])):
            # Buffer incomplete UTF-8 sequences until the next token.
            decoded = tokenizer.decode(evidence['tokens'][:i + 1], skip_special_tokens=True).rstrip('\ufffd')
            if not decoded.startswith(previous):
                raise ValueError('non-monotonic incremental decoding')
            print(json.dumps({'type': 'token', 'text': decoded[len(previous):]}), flush=True)
            previous = decoded
    print(json.dumps({'type': 'result', 'receipt_b64': base64.b64encode(receipt.SerializeToString(deterministic=True)).decode(), 'output_tokens': receipt.output_tokens}), flush=True)


if __name__ == '__main__':
    run_executor()
