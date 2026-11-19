"""v14 PARAMS §1 wire hashes. Tensor payloads are hashed in bounded blocks."""
import hashlib
import math
import struct
from pathlib import Path

DTYPES = {'f32': (1, 4), 'f64': (2, 8), 'bf16': (3, 2),
          'f16': (4, 2), 'i64': (5, 8), 'i32': (6, 4)}
BLOCK = 1 << 20


class FormatFailure(ValueError):
    pass


class BindingFailure(ValueError):
    pass


def hash32(value):
    raw = bytes.fromhex(value)
    if len(raw) != 32:
        raise ValueError('expected SHA-256')
    return raw


def tensor_header(dtype, shape):
    if dtype not in DTYPES or not isinstance(shape, list) or len(shape) > 255:
        raise FormatFailure('invalid tensor dtype or rank')
    if any(type(d) is not int or not 0 <= d < 2**64 for d in shape):
        raise FormatFailure('invalid tensor shape')
    return b'ETV1' + bytes((DTYPES[dtype][0], len(shape))) + b''.join(struct.pack('>Q', d) for d in shape)


def descriptor(dtype, shape):
    tensor_header(dtype, shape)
    return dtype + '|' + ','.join(map(str, shape)) + '|raw-le-v1'


def tensor_hash(path, fmt):
    header = tensor_header(fmt['dtype'], fmt['shape'])
    expected = math.prod(fmt['shape']) * DTYPES[fmt['dtype']][1]
    if fmt.get('encoding') != 'raw-le-v1':
        raise FormatFailure('unsupported tensor encoding')
    digest = hashlib.sha256(header)
    with Path(path).open('rb') as source:
        count = 0
        while True:
            block = source.read(BLOCK)
            if not block:
                break
            count += len(block)
            digest.update(block)
        if count != expected:
            raise FormatFailure('canonical tensor payload length')
    return digest.hexdigest()


def leaf_hash(leaf):
    step = leaf['step_index']
    if type(step) is not int or not 1 <= step < 2**64:
        raise BindingFailure('invalid leaf step')
    raw = (b'EMBER-SPOTLEAF-V1' + struct.pack('>Q', step)
           + b''.join(hash32(leaf[k]) for k in ('input_hash', 'output_hash',
                         'format_hash', 'profile_id_hash', 'request_id')))
    return hashlib.sha256(b'\x00' + raw).digest()


def verify_proof(leaf, proof, root, count):
    value = leaf_hash(leaf)
    index = leaf['step_index'] - 1
    if type(count) is not int or not 0 <= index < count:
        raise BindingFailure('leaf outside committed tree')
    width = count
    for item in proof:
        if width <= 1:
            raise BindingFailure('excess Merkle siblings')
        sibling = hash32(item['hash'])
        left = bool(index % 2)
        if type(item['left']) is not bool or item['left'] != left:
            raise BindingFailure('wrong Merkle direction')
        if index == width - 1 and width % 2 and sibling != value:
            raise BindingFailure('odd Merkle node must self-pair')
        value = hashlib.sha256(b'\x01' + (sibling + value if left else value + sibling)).digest()
        index //= 2
        width = (width + 1) // 2
    if width != 1 or value != hash32(root):
        raise BindingFailure('Merkle root mismatch')


def verify_binding(opening, context):
    """Hash actual tensors, check leaf, prove membership, then boundary anchors."""
    leaf = opening['leaf']
    step = opening['step_index']
    profile_hash = hashlib.sha256(context['profile_id'].encode()).hexdigest()
    if (leaf['step_index'], leaf['request_id'], leaf['profile_id_hash']) != (step, context['request_id'], profile_hash):
        raise BindingFailure('leaf identity mismatch')
    for direction in ('input', 'output'):
        fmt = opening['tensors'][direction]
        if fmt.get('encoding', 'raw-le-v1') != 'raw-le-v1':
            raise FormatFailure('unsupported tensor encoding')
        actual = tensor_hash(fmt['path'], dict(fmt, encoding='raw-le-v1'))
        if actual != leaf[direction + '_hash']:
            raise BindingFailure('opened tensor hash mismatch')
    fmt = opening['tensors']['output']
    if leaf['format_hash'] != hashlib.sha256(descriptor(fmt['dtype'], fmt['shape']).encode()).hexdigest():
        raise BindingFailure('leaf format binding mismatch')
    verify_proof(leaf, opening['proof'], context['root'], context['total_steps'])
    if context['total_steps'] > 1:
        adjacent = opening['neighbor_leaf']
        if (adjacent['step_index'], adjacent['request_id'], adjacent['profile_id_hash']) != (step + 1 if step < context['total_steps'] else step - 1, context['request_id'], profile_hash):
            raise BindingFailure('adjacent leaf identity')
        verify_proof(adjacent, opening['neighbor_proof'], context['root'], context['total_steps'])
        boundary_ok = (leaf['output_hash'] == adjacent['input_hash'] if step < context['total_steps']
                       else adjacent['output_hash'] == leaf['input_hash'])
        if not boundary_ok:
            raise BindingFailure('broken boundary chain')
    if step == 1 and leaf['input_hash'] != context['input_anchor']:
        raise BindingFailure('request token anchor mismatch')
    if step == context['total_steps'] and leaf['output_hash'] != context['output_anchor']:
        raise BindingFailure('output token anchor mismatch')
