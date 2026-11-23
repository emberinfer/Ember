"""Protocol vectors and independent audit-format compatibility checks."""
import hashlib
import json
from pathlib import Path
import struct
import sys

import pytest

ROOT = Path(__file__).resolve().parents[1]
sys.path[:0] = [str(ROOT / 'core-py'), str(ROOT / 'node')]
from ember_core import t2i
from auditor.spot_binding import tensor_hash, tensor_header

VECTORS = json.loads((ROOT / 'testdata/vectors/t2i-v1.json').read_text())['vectors']


@pytest.mark.parametrize('v', VECTORS, ids=lambda v: v['seed'] + ':' + 'x'.join(map(str, v['shape'])))
def test_latent_vectors(v, tmp_path):
    values = t2i.init_latent(int(v['seed']), v['shape'])
    wire = t2i.serialize_latent(values, v['shape'])
    assert hashlib.sha256(wire).hexdigest() == v['sha256']
    assert hashlib.sha256(wire[38:]).hexdigest() == v['raw_sha256']
    assert [f'{struct.unpack("<I", struct.pack("<f", x))[0]:08x}' for x in values[:8]] == v['first_bits']
    # Existing audit code independently reconstructs exactly the same hash input.
    assert wire[:38] == tensor_header('f32', v['shape'])
    path = tmp_path / 'latent.raw'
    path.write_bytes(wire[38:])
    assert tensor_hash(path, dict(dtype='f32', shape=v['shape'], encoding='raw-le-v1')) == v['sha256']


def test_philox_known_answer_and_high_counter():
    assert t2i.philox(0, 0) == (0x6627e8d5, 0xe169c58d, 0xbc57ac4c, 0x9b00dbd8)
    assert t2i.philox(0, 1) != t2i.philox(0, 2**32)


def test_prompt_boundaries_and_empty_negative():
    calls = []
    def tokenizer(text):
        calls.append(text)
        return [12] * len(text)
    positive, negative = t2i.encode_prompts('a'*75, '', tokenizer)
    assert calls == ['a'*75, '']
    assert positive == [49406] + [12]*75 + [49407]
    assert negative == [49406] + [49407]*76
    with pytest.raises(ValueError, match='exceeds'):
        t2i.encode_prompt('a'*76, tokenizer)
    with pytest.raises(ValueError):
        t2i.encode_prompt('x', lambda _: [49407])
    with pytest.raises(UnicodeEncodeError):
        t2i.encode_prompt('\ud800', tokenizer)


def test_serialization_small_and_rejection():
    shape = [1, 1, 1, 4]
    wire = t2i.serialize_latent([0.0, -0.0, 1.0, -2.0], shape)
    assert wire[:6] == b'ETV1\x01\x04'
    assert wire[6:38] == b''.join(d.to_bytes(8, 'big') for d in shape)
    assert wire[38:].hex() == '00000000000000800000803f000000c0'
    for seed in (-1, 2**64, True):
        with pytest.raises(ValueError):
            t2i.init_latent(seed, [1, 1, 1, 1])
    for shape in ([4, 64, 64], [2, 4, 64, 64], [1, 4, 0, 64]):
        with pytest.raises(ValueError):
            t2i.init_latent(0, shape)
    for values in ([0.0], [float('nan')]*4, [float('inf')]*4):
        with pytest.raises(ValueError):
            t2i.serialize_latent(values, [1, 1, 1, 4])
