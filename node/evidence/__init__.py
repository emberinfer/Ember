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


DEFAULT_MAX_FRAME_TOKENS = 512
# Leave room for protobuf envelopes inside the standard 4 MiB gRPC message limit.
MAX_FRAME_BYTES = 3 * 1024 * 1024


def merkle_root(hashes):
    level = list(hashes)
    if not level:
        raise ValueError('empty evidence tree')
    while len(level) > 1:
        level = [hashlib.sha256(b'\x01' + level[i] +
                 level[min(i + 1, len(level) - 1)]).digest()
                 for i in range(0, len(level), 2)]
    return level[0]


def manifest_chunks(openings, limit=DEFAULT_MAX_FRAME_TOKENS):
    from ember_pb2 import EvidenceManifest
    if type(limit) is not int or limit < 1:
        raise ValueError('invalid evidence.max_frame_tokens')
    data, group, size = [], [], 2
    for item in openings:
        encoded = canonical(item)
        if len(encoded) + 2 > MAX_FRAME_BYTES:
            raise ValueError('single evidence token exceeds transport byte limit')
        if group and (len(group) == limit or size + len(encoded) + 1 > MAX_FRAME_BYTES):
            data.append(b'[' + b','.join(group) + b']')
            group, size = [], 2
        group.append(encoded)
        size += len(encoded) + 1
    if group:
        data.append(b'[' + b','.join(group) + b']')
    hashes = [hashlib.sha256(chunk).digest() for chunk in data]
    return EvidenceManifest(merkle_root=merkle_root(hashes), chunk_count=len(data),
                            total_tokens=len(openings), chunk_hashes=hashes), data


def verify_manifest(manifest, total, limit):
    if (limit < 1 or manifest.total_tokens != total or manifest.chunk_count < 2 or
            manifest.chunk_count != len(manifest.chunk_hashes) or
            manifest.chunk_count > total or total > manifest.chunk_count * limit or
            any(len(h) != 32 for h in manifest.chunk_hashes) or
            merkle_root(manifest.chunk_hashes) != manifest.merkle_root):
        raise ValueError('invalid evidence manifest')


def fetch_openings(receipt, fetch, limit=DEFAULT_MAX_FRAME_TOKENS):
    manifest = receipt.evidence_manifest
    verify_manifest(manifest, receipt.output_tokens, limit)
    result = []
    for index, expected in enumerate(manifest.chunk_hashes):
        data = fetch(manifest.merkle_root, index)
        if len(data) > MAX_FRAME_BYTES:
            raise ValueError('evidence frame exceeds byte limit')
        if hashlib.sha256(data).digest() != expected:
            raise ValueError('evidence chunk hash mismatch')
        group = json.loads(data)
        if not isinstance(group, list) or not 0 < len(group) <= limit:
            raise ValueError('invalid evidence frame token count')
        result.extend(group)
    if len(result) != manifest.total_tokens:
        raise ValueError('evidence total token count mismatch')
    return result
