"""Single full-prefix step of the existing stateless CPU shard audit runtime."""
import math
import numpy as np

from .spot_binding import FormatFailure
from .numeric import relative_distance

CPU_REFERENCE = 'qwen2-shard-forward-sdpa-cpu-bf16-no-kv-v1'
NUMPY_TYPES = {'f32': '<f4', 'f64': '<f8', 'bf16': '<u2', 'f16': '<f2',
               'i64': '<i8', 'i32': '<i4'}


def mapped_tensor(path, fmt):
    """Map one canonical tensor; never materialize the whole audit package."""
    return np.memmap(path, dtype=NUMPY_TYPES[fmt['dtype']], mode='r',
                     offset=0,
                     shape=tuple(fmt['shape']))


def torch_tensor(path, fmt, torch):
    value = torch.from_numpy(np.array(mapped_tensor(path, fmt), copy=True))
    return value.view(torch.bfloat16) if fmt['dtype'] == 'bf16' else value


def replay_step(engine, opening, operation):
    leased = getattr(engine, 'replay_spot', None)
    if leased is not None:
        return leased(opening, operation)
    from .range_pipeline import _reference_shard
    shard = _reference_shard(engine)
    if str(shard.device) != 'cpu':
        raise ValueError('spot audit requires registered CPU reference engine')
    torch = shard.torch
    if shard.dtype != torch.bfloat16:
        raise ValueError('spot CPU reference requires BF16 shard semantics')
    value = torch_tensor(opening['tensors']['input']['path'], opening['tensors']['input'], torch)
    with torch.inference_mode():
        # Exactly the same full-sequence, no-KV forward used in audit_range.
        result = shard.forward(
            input_ids=(value.unsqueeze(0) if value.ndim == 1 else value) if operation['has_embedding'] else None,
            hidden_states=None if operation['has_embedding'] else value,
            start_layer=operation['start_layer'], end_layer=operation['end_layer'],
            has_embedding=operation['has_embedding'], has_lm_head=operation['has_lm_head'],
            use_cache=False, position_offset=0)
    tokens = result.argmax(dim=-1).reshape(-1).tolist() if operation['has_lm_head'] else None
    return result, tokens


def compare_output(reference, opening, epsilon):
    """Existing activation relative-distance and first > ε position rule."""
    if not math.isfinite(epsilon) or epsilon <= 0:
        raise ValueError('invalid chain equality epsilon')
    import torch
    actual = torch_tensor(opening['tensors']['output']['path'], opening['tensors']['output'], torch)
    if tuple(actual.shape) != tuple(reference.shape):
        raise FormatFailure('replay output shape differs from profile')
    a = actual.float().reshape(-1, actual.shape[-1] if actual.ndim else 1)
    b = reference.float().reshape(a.shape)
    first, first_distance, maximum = None, 0.0, 0.0
    for index, (left, right) in enumerate(zip(a, b)):
        distance = relative_distance(left.tolist(), right.tolist())
        if not math.isfinite(distance):
            raise FormatFailure('unparseable/non-finite tensor data')
        maximum = max(maximum, distance)
        if first is None and distance > epsilon:
            first, first_distance = index, distance
    return ('mismatch' if first is not None else 'match',
            first_distance if first is not None else maximum, first)
