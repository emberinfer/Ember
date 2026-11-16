"""Pre-load admission for dynamic Qwen2 ranges (no tensor materialization).

The caller supplies a local memory/VRAM allocation budget, not total machine
capacity. This is a conservative estimate at batch=1 and the reference sequence
length, not a measurement of peak RSS or a guarantee for unbounded prompts.
"""
import argparse
from dataclasses import asdict, dataclass
import json
import os
from pathlib import Path
import re
import struct


@dataclass(frozen=True)
class RangeFit:
    start: int
    end: int
    layer_bytes: tuple
    embedding_bytes: int
    head_bytes: int
    weight_bytes: int
    activation_bytes: int
    required_bytes: int
    available_bytes: int
    reference_sequence: int


class RangeFitError(ValueError):
    def __init__(self, estimate):
        self.estimate = estimate
        super().__init__(f'range [{estimate.start},{estimate.end}) does not fit: '
                         f'required {estimate.required_bytes} bytes, '
                         f'available {estimate.available_bytes} bytes '
                         f'(weights {estimate.weight_bytes}, activation/KV reserve '
                         f'{estimate.activation_bytes}; sequence {estimate.reference_sequence})')


def _local(root, name):
    if Path(name).name != name or name in ('', '.', '..'):
        raise ValueError('unsafe range-fit artifact name')
    return root / name


def tensor_sizes(root, artifacts):
    """Index maps tensor names to shards; byte lengths come from file headers.

    An index has no per-tensor sizes. Reading data_offsets avoids loading weights
    and counts the actual stored dtype. Unindexed single-file releases work too.
    Only files declared in the manifest may contribute to the estimate.
    """
    root = Path(root)
    names = {a.name for a in artifacts}
    if 'config.json' not in names:
        raise ValueError('range fit requires config.json in manifest artifacts')
    indexes = sorted(n for n in names if n.endswith('.safetensors.index.json'))
    if len(indexes) > 1:
        raise ValueError('ambiguous safetensors index')
    mapping = None
    if indexes:
        mapping = json.loads(_local(root, indexes[0]).read_text())['weight_map']
        files = set(mapping.values())
        if not files <= names:
            raise ValueError('safetensors index references undeclared artifacts')
    else:
        files = {n for n in names if n.endswith('.safetensors')}
    if not files:
        raise ValueError('no manifest safetensors weights for range fit')
    # Qwen2ShardEngine opens every local *.safetensors. Fail closed if its
    # inventory could differ from the signed/indexed files estimated here.
    if files != {p.name for p in root.glob('*.safetensors')}:
        raise ValueError('local safetensors inventory differs from manifest/index')
    sizes = {}
    for name in sorted(files):
        with _local(root, name).open('rb') as reader:
            length = struct.unpack('<Q', reader.read(8))[0]
            if not 0 < length <= 64 * 1024 * 1024:
                raise ValueError('invalid safetensors header length')
            header = json.loads(reader.read(length))
            data_size = os.fstat(reader.fileno()).st_size - 8 - length
        for key, value in header.items():
            if key == '__metadata__':
                continue
            if key in sizes or (mapping is not None and mapping.get(key) != name):
                raise ValueError('duplicate or inconsistent safetensors index entry')
            a, b = value['data_offsets']
            if not 0 <= a < b <= data_size:
                raise ValueError('invalid safetensors data offsets')
            sizes[key] = b - a
    if mapping is not None and set(mapping) != set(sizes):
        raise ValueError('safetensors index tensor missing')
    return sizes


def check_range_fit(artifacts, config, model_path, start, end, budget_bytes,
                    reference_sequence=2048):
    total = int(config['num_hidden_layers'])
    if config.get('model_type') != 'qwen2' or not 0 <= start < end <= total:
        raise ValueError('range fit requires a valid Qwen2 layer range')
    if budget_bytes is None or int(budget_bytes) <= 0 or reference_sequence <= 0:
        raise ValueError('range fit needs a positive memory budget and reference sequence')
    sizes = tensor_sizes(model_path, artifacts)
    layers = [0] * total
    for key, size in sizes.items():
        match = re.fullmatch(r'model\.layers\.(\d+)\..+', key)
        if match:
            index = int(match[1])
            if index >= total:
                raise ValueError('weight layer outside model config')
            layers[index] += size
    if any(size == 0 for size in layers):
        raise ValueError('missing layer weights in manifest safetensors')
    embedding = sizes['model.embed_tokens.weight'] if start == 0 else 0
    # The existing Qwen2ShardEngine materializes a separate output projection
    # from embed_tokens, even for tied weights and ranges owning both endpoints.
    head = (sizes['model.norm.weight'] + sizes['model.embed_tokens.weight']
            if end == total else 0)
    weight = sum(layers[start:end]) + embedding + head
    hidden = int(config['hidden_size'])
    intermediate = int(config['intermediate_size'])
    heads = int(config['num_attention_heads'])
    kv_heads = int(config.get('num_key_value_heads', heads))
    seq = reference_sequence
    # Batch=1, BF16=2 bytes; reserve 4x live hidden/MLP buffers plus a
    # dense attention matrix (conservative for SDPA), and KV for every owned
    # layer although Ember currently recomputes rather than retaining KV.
    # Head logits are FP32. Keep 512 MiB runtime/allocator allowance.
    live = 2 * (seq * (8 * hidden + 3 * intermediate) + heads * seq * seq)
    kv = 2 * 2 * (end - start) * seq * kv_heads * (hidden // heads)
    logits = 4 * seq * int(config['vocab_size']) if end == total else 0
    activation = 4 * (live + kv + logits) + 512 * 1024**2
    # 2x stored weights covers resident BF16 plus source/loading copies.
    estimate = RangeFit(start, end, tuple(layers), embedding, head, weight,
                        activation, 2 * weight + activation, int(budget_bytes), seq)
    if estimate.required_bytes > estimate.available_bytes:
        raise RangeFitError(estimate)
    return estimate


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--manifest', required=True, help='binary ModelManifest')
    parser.add_argument('--model-path', required=True)
    parser.add_argument('--range-start', type=int, required=True)
    parser.add_argument('--range-end', type=int, required=True)
    parser.add_argument('--fit-memory-bytes', type=int,
                        default=os.environ.get('EMBER_RANGE_MEMORY_BYTES'))
    parser.add_argument('--fit-reference-sequence', type=int, default=2048)
    args = parser.parse_args()
    from ember_pb2 import ModelManifest
    from fetcher import verify_local
    manifest = ModelManifest.FromString(Path(args.manifest).read_bytes())
    for artifact in manifest.artifacts:
        verify_local(artifact, args.model_path)
    config = json.loads(Path(args.model_path, 'config.json').read_text())
    if manifest.total_layers != config['num_hidden_layers']:
        parser.error('manifest/config layer count mismatch')
    try:
        result = check_range_fit(manifest.artifacts, config, args.model_path,
                                 args.range_start, args.range_end,
                                 args.fit_memory_bytes, args.fit_reference_sequence)
    except ValueError as exc:
        parser.exit(1, f'{exc}\n')
    print(json.dumps(asdict(result)))


if __name__ == '__main__':
    main()
