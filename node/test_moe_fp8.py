"""FP8-native MoE expert replay tests (2026-11-15 decision).

For fp8-e4m3-block128 profiles the auditor replays expert segments in
the production FP8 caliber itself (engine/fp8.py) instead of through a
bf16 dequantization, whose distance to production measured
2.7e-2..4.2e-2 in MOE-0 and forced a ~0.06 tolerance band. These tests
pin, on a synthetic FP8 checkpoint:

  * the FP8 unit's forward matches an independent FP8 reference
    (same quantization scheme, different accumulation order);
  * the native path is NOT the bf16 path in disguise (the two calibers
    disagree far above the FP8 band);
  * honest production-caliber evidence lands inside the pinned FP8
    expert band from chain-genesis/build_moe.py, while evidence
    produced from swapped weight blocks lands far outside it;
  * the params hash covers the block scales, and the unit factory
    selects the caliber from the profile's segment dtype.
"""
import importlib.util
import json
import sys
import tempfile
import unittest
from pathlib import Path

sys.path[:0] = [str(Path(__file__).resolve().parents[1] / 'proto'),
                str(Path(__file__).resolve().parents[1] / 'core-py'),
                str(Path(__file__).resolve().parent)]

import torch
from safetensors.torch import save_file

from ember_pb2 import MoeLayout
from engine.moe import ExpertUnit, Fp8ExpertUnit, MoeWeights, make_expert_unit

HIDDEN = 256
INTER = 256
PREFIX = 'model.language_model.layers.0.mlp.experts.0.'


def _quantize_block(w):
    """fp32 [R, C] -> (e4m3 codes, scale_inv) with 128x128 blocks."""
    r, c = w.shape
    blocks = w.view(r // 128, 128, c // 128, 128)
    scale = (blocks.abs().amax(dim=(1, 3)) / 448.0).clamp(min=1e-12)
    codes = (blocks / scale.view(r // 128, 1, c // 128, 1)).to(torch.float8_e4m3fn)
    return codes.view(r, c), scale


def _fp8_checkpoint():
    torch.manual_seed(11)
    tensors = {}
    for proj, shape in (('gate_proj', (INTER, HIDDEN)),
                        ('up_proj', (INTER, HIDDEN)),
                        ('down_proj', (HIDDEN, INTER))):
        codes, scale = _quantize_block(torch.randn(shape) * 0.05)
        tensors[PREFIX + proj + '.weight'] = codes
        tensors[PREFIX + proj + '.weight_scale_inv'] = scale
    # A bf16 expert under the OLMoE-style prefix, for the factory's
    # bf16 branch (expert 1; the FP8 expert above is expert 0).
    for proj, shape in (('gate_proj', (INTER, HIDDEN)),
                        ('up_proj', (INTER, HIDDEN)),
                        ('down_proj', (HIDDEN, INTER))):
        tensors[f'model.layers.0.mlp.experts.1.{proj}.weight'] = \
            (torch.randn(shape) * 0.05).to(torch.bfloat16)
    path = Path(tempfile.mkdtemp(prefix='fp8-tiny-'))
    (path / 'config.json').write_text(json.dumps({'hidden_size': HIDDEN}))
    save_file(tensors, str(path / 'model.safetensors'),
              metadata={'format': 'pt'})
    return path, tensors


def _dequant(w, s):
    r, c = w.shape
    return (w.float().view(r // 128, 128, c // 128, 128) *
            s.float().view(r // 128, 1, c // 128, 1)).view(r, c)


def _ref_fp8_ffn(x, tensors, perturb_gate=False):
    """Independent FP8 reference: per-group activation quantization,
    one-shot fp32 accumulation (the shipped code accumulates per
    input group; only the accumulation order differs)."""
    def lin(x, proj):
        w = tensors[PREFIX + proj + '.weight']
        if perturb_gate and proj == 'gate_proj':
            w = w.clone()
            block = w[:, :128].clone()
            w[:, :128] = w[:, 128:256]
            w[:, 128:256] = block
        wd = _dequant(w, tensors[PREFIX + proj + '.weight_scale_inv'])
        xf = x.float()
        n, c = xf.shape
        grouped = xf.view(n, c // 128, 128)
        scale = (grouped.abs().amax(dim=2, keepdim=True) / 448.0).clamp(min=1e-12)
        xq = ((grouped / scale).to(torch.float8_e4m3fn).float() * scale).view(n, c)
        return xq @ wd.t()
    g = lin(x, 'gate_proj')
    u = lin(x, 'up_proj')
    mid = torch.nn.functional.silu(g) * u
    return lin(mid, 'down_proj')


def _bf16_ffn(x, tensors):
    def lin(x, proj):
        wd = _dequant(tensors[PREFIX + proj + '.weight'],
                      tensors[PREFIX + proj + '.weight_scale_inv']).to(torch.bfloat16)
        return torch.nn.functional.linear(x.to(torch.bfloat16), wd)
    g = lin(x, 'gate_proj')
    u = lin(x, 'up_proj')
    mid = torch.nn.functional.silu(g) * u
    return lin(mid, 'down_proj').float()


def _rel_l2(a, b):
    a, b = a.float(), b.float()
    return float(torch.linalg.vector_norm(a - b) /
                 max(torch.linalg.vector_norm(b), 1e-12))


def _fp8_band():
    path = Path(__file__).resolve().parents[1] / 'chain-genesis' / 'build_moe.py'
    spec = importlib.util.spec_from_file_location('build_moe', path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module.BANDS['fp8-e4m3-block128']['expert']


class Fp8ExpertTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.dir, cls.tensors = _fp8_checkpoint()
        cls.weights = MoeWeights(cls.dir)
        cls.unit = Fp8ExpertUnit(cls.weights, 0, 0)
        torch.manual_seed(5)
        cls.rows = torch.randn(3, HIDDEN, dtype=torch.bfloat16)

    def test_forward_matches_independent_fp8_reference(self):
        # The unit returns bf16 (the evidence dtype); compare against
        # the reference cast to bf16 so only the arithmetic is measured.
        mine = self.unit.forward(self.rows)
        ref = _ref_fp8_ffn(self.rows, self.tensors).to(torch.bfloat16)
        self.assertLess(_rel_l2(mine, ref), 1e-4)

    def test_native_path_is_not_bf16_in_disguise(self):
        mine = self.unit.forward(self.rows)
        bf16 = _bf16_ffn(self.rows, self.tensors)
        self.assertGreater(_rel_l2(mine, bf16), 1e-3)

    def test_honest_within_band_cheater_outside(self):
        band = _fp8_band()
        honest = _ref_fp8_ffn(self.rows, self.tensors)
        cheated = _ref_fp8_ffn(self.rows, self.tensors, perturb_gate=True)
        mine = self.unit.forward(self.rows)
        self.assertLess(_rel_l2(mine, honest), band,
                        'honest FP8 evidence outside the pinned band')
        self.assertGreater(_rel_l2(mine, cheated), band,
                           'weight-swapped evidence inside the pinned band')

    def test_params_hash_covers_block_scales(self):
        # Same weight codes, one block scale changed: the hash must
        # move, because the scales are weight material.
        variant = dict(self.tensors)
        key = PREFIX + 'gate_proj.weight_scale_inv'
        changed = self.tensors[key].clone()
        changed[0, 0] *= 1.5
        variant[key] = changed
        path = Path(tempfile.mkdtemp(prefix='fp8-variant-'))
        (path / 'config.json').write_text(json.dumps({'hidden_size': HIDDEN}))
        save_file(variant, str(path / 'model.safetensors'),
                  metadata={'format': 'pt'})
        other = Fp8ExpertUnit(MoeWeights(path), 0, 0)
        self.assertNotEqual(self.unit.params_hash, other.params_hash)
        again = Fp8ExpertUnit(self.weights, 0, 0)
        self.assertEqual(self.unit.params_hash, again.params_hash)

    def test_factory_selects_caliber_from_segment_dtype(self):
        fp8_layout = MoeLayout(segment_dtype='fp8-e4m3-block128')
        self.assertIsInstance(
            make_expert_unit(self.weights, fp8_layout, 0, 0), Fp8ExpertUnit)
        bf16_layout = MoeLayout(segment_dtype='bf16')
        self.assertIsInstance(
            make_expert_unit(self.weights, bf16_layout, 0, 1), ExpertUnit)


if __name__ == '__main__':
    unittest.main()
