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

ROOT = Path(os.environ.get('EMBER_MOE_TREE', Path(__file__).resolve().parent.parent))
sys.path[:0] = [str(ROOT / p) for p in ('proto', 'core-py', 'node')]


def main():
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

        if stream:
            receipt, evidence = assembler.generate(q['request_id'], q['prompt'], q['max_tokens'], on_token=emit_tokens)
        else:
            receipt, evidence = assembler.generate(q['request_id'], q['prompt'], q['max_tokens'])
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
    main()
