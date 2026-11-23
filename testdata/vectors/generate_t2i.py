"""Explicit vector regeneration / byte comparison helper (stdlib only).

python testdata/vectors/generate_t2i.py > testdata/vectors/t2i-v1.json
python testdata/vectors/generate_t2i.py --binary > /tmp/t2i-python.bin
Do not regenerate frozen vectors to make an implementation change pass.
"""
import hashlib
import json
from pathlib import Path
import struct
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / 'core-py'))
from ember_core.t2i import init_latent, serialize_latent


def generate(binary=False):
    rows = []
    for seed in (0, 1, 42, 2**63, 2**64-1):
        for shape in ((1, 4, 64, 64), (1, 4, 64, 96), (1, 4, 96, 64)):
            values = init_latent(seed, shape)
            wire = serialize_latent(values, shape)
            if binary:
                sys.stdout.buffer.write(wire)
            else:
                rows.append(dict(seed=str(seed), shape=list(shape),
                                 raw_sha256=hashlib.sha256(wire[38:]).hexdigest(),
                                 sha256=hashlib.sha256(wire).hexdigest(),
                                 first_bits=[f'{struct.unpack("<I", struct.pack("<f", v))[0]:08x}' for v in values[:8]]))
    if not binary:
        print(json.dumps(dict(spec='philox-gauss-v1', dtype='f32', vectors=rows), indent=2))


if __name__ == '__main__':
    generate('--binary' in sys.argv[1:])
