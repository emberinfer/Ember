"""Evidence is JSON with canonical byte commitments; no raw KV leaves the node."""
import hashlib
import json

from ember_core import digest, sign
from ember_pb2 import ChunkRoot


def encode_step(step):
    result = {'token': step.token, 'topk': step.topk,
              'activations': {k: hashlib.sha256(v).hexdigest() for k, v in sorted(step.activations.items())}}
    if step.score_kind != 'raw-logits':
        result['score_kind'] = step.score_kind
    return result


def opening(step):
    result = {'token': step.token, 'topk': step.topk,
              'activations': {k: v.hex() for k, v in sorted(step.activations.items())}}
    if step.score_kind != 'raw-logits':
        result['score_kind'] = step.score_kind
    return result


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False).encode()


def chunks(request_id, steps, key):
    previous = b''
    result = []
    for start in range(0, len(steps), 16):
        group = [encode_step(s) for s in steps[start:start + 16]]
        root = ChunkRoot(request_id=request_id, seq=len(result), upto_step=start + len(group),
                         merkle_root=hashlib.sha256(canonical(group)).digest(), prev_root=previous)
        sign(root, key)
        result.append(root)
        previous = digest(root)
    return result
