"""Frozen T0 reference semantics; see docs/T2I-SPEC-V1.md. No ML dependencies."""
import math
import struct


def f32(x):
    return struct.unpack('<f', struct.pack('<f', x))[0]


def _bits(x):
    return struct.unpack('<I', struct.pack('<f', x))[0]


def _float(x):
    return struct.unpack('<f', struct.pack('<I', x))[0]


PI, TWO_PI, HALF_PI = map(_float, (0x40490fdb, 0x40c90fdb, 0x3fc90fdb))
LN2, INIT_SCALE = map(_float, (0x3f317218, 0x3e3a8588))
MASK = 0xffffffff


def philox(seed, index):
    """Philox4x32-10, counter=(index.lo,index.hi,0,0), word order c0..c3."""
    if type(seed) is not int or not 0 <= seed < 2**64:
        raise ValueError('seed must be uint64')
    if type(index) is not int or not 0 <= index < 2**64:
        raise ValueError('index must be uint64')
    c0, c1, c2, c3 = index & MASK, index >> 32, 0, 0
    k0, k1 = seed & MASK, seed >> 32
    for _ in range(10):
        p0, p1 = 0xd2511f53 * c0, 0xcd9e8d57 * c2
        c0, c1, c2, c3 = ((p1 >> 32) ^ c1 ^ k0), p1 & MASK, ((p0 >> 32) ^ c3 ^ k1), p0 & MASK
        k0, k1 = (k0 + 0x9e3779b9) & MASK, (k1 + 0xbb67ae85) & MASK
    return c0, c1, c2, c3


def log32(x):
    """Protocol finite series, NOT a call to platform libm."""
    bits = _bits(x)
    e = (bits >> 23) - 127
    m = _float((bits & 0x7fffff) | 0x3f800000)
    z = f32(f32(m - 1) / f32(m + 1))
    z2 = f32(z * z)
    p = s = z
    for k in range(1, 13):
        p = f32(p * z2)
        s = f32(s + f32(p / (2*k + 1)))
    return f32(f32(e * LN2) + f32(2 * s))


def sqrt32(x):
    if x == 0:
        return 0.0
    y = _float((_bits(x) >> 1) + 0x1fc00000)
    for _ in range(8):
        y = f32(0.5 * f32(y + f32(x / y)))
    return y


def cos32(theta):
    x = theta
    if x > PI:
        x = f32(x - TWO_PI)
    sign = 1.0
    if x > HALF_PI:
        x, sign = f32(PI - x), -1.0
    elif x < -HALF_PI:
        x, sign = f32(-PI - x), -1.0
    x2 = f32(x * x)
    term = total = 1.0
    for k in range(1, 13):
        term = f32(f32(-term * x2) / ((2*k - 1) * (2*k)))
        total = f32(total + term)
    return f32(sign * total)


def init_latent(seed, shape):
    """Return flat fp32 init; shape is the FULL (1,C,h,w), not image H,W."""
    if type(seed) is not int or not 0 <= seed < 2**64:
        raise ValueError('seed must be uint64')
    _shape(shape)
    result = []
    for i in range(math.prod(shape)):
        x0, x1, _, _ = philox(seed, i)
        u0, u1 = (x0 >> 8) * 2**-24, (x1 >> 8) * 2**-24
        radius = sqrt32(f32(-2 * log32(f32(1 - u0))))
        theta = f32(TWO_PI * u1)
        result.append(f32(f32(radius * cos32(theta)) * INIT_SCALE))
    return result


def _shape(shape):
    if (not isinstance(shape, (list, tuple)) or len(shape) != 4 or shape[0] != 1
            or any(type(d) is not int or not 0 < d < 2**64 for d in shape)):
        raise ValueError('expected positive rank-4 shape with batch=1')


def serialize_latent(values, shape, dtype='f32'):
    """Existing ETV1 audit bytes: big-endian dimensions, little-endian payload."""
    _shape(shape)
    if dtype != 'f32' or len(values) != math.prod(shape):
        raise ValueError('invalid dtype or payload length')
    if any(not math.isfinite(v) for v in values):
        raise ValueError('nonfinite latent')
    header = b'ETV1' + bytes((1, 4))
    header += struct.pack('>4Q', *shape)
    return header + b''.join(struct.pack('<f', f32(v)) for v in values)


def encode_prompt(text, tokenize, sequence_length=77):
    """tokenize(text) supplies profile-pinned content IDs WITHOUT specials/pad."""
    if not isinstance(text, str) or type(sequence_length) is not int or sequence_length < 2:
        raise ValueError('invalid prompt or sequence length')
    text.encode('utf-8', errors='strict')
    tokens = list(tokenize(text))
    if len(tokens) + 2 > sequence_length:
        raise ValueError('prompt exceeds sequence length')
    if any(type(t) is not int or not 0 <= t < 49406 for t in tokens):
        raise ValueError('invalid SD1.5 content token')
    return [49406] + tokens + [49407] * (sequence_length - len(tokens) - 1)


def encode_prompts(positive, negative, tokenize):
    return encode_prompt(positive, tokenize), encode_prompt(negative, tokenize)
