"""MoE engine reference tests: segment engines vs the transformers OLMoE
implementation they must replicate, on a tiny random-weight model and on
real OLMoE layer-8 expert weights."""
import sys
import tempfile
import unittest
from pathlib import Path

sys.path[:0] = [str(Path(__file__).resolve().parents[1] / 'proto'),
                str(Path(__file__).resolve().parents[1] / 'core-py'),
                str(Path(__file__).resolve().parent)]

import torch
from transformers import OlmoeConfig, OlmoeForCausalLM

from ember_pb2 import MoeLayout
from engine.moe import AttentionStage, ExpertUnit, MoeWeights


def rel_l2(a, b):
    a, b = a.double(), b.double()
    return float(torch.linalg.vector_norm(a - b) / torch.linalg.vector_norm(b))


def tiny_model_dir():
    torch.manual_seed(7)
    cfg = OlmoeConfig(hidden_size=64, intermediate_size=32, num_hidden_layers=2,
                      num_attention_heads=4, num_key_value_heads=4,
                      num_experts=4, num_experts_per_tok=2, vocab_size=128,
                      max_position_embeddings=256)
    model = OlmoeForCausalLM(cfg)
    path = Path(tempfile.mkdtemp(prefix='olmoe-tiny-'))
    model.save_pretrained(path)
    return path, cfg


class MoeEngineTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.dir, cls.cfg = tiny_model_dir()
        cls.weights = MoeWeights(cls.dir)
        cls.layout = MoeLayout(num_layers=2, hidden_size=64, experts_per_layer=4,
                               top_k=2, expert_param_count=3 * 64 * 32,
                               attention_param_count=4 * 64 * 64 + 64 * 4,
                               vocab_size=128, max_frame_positions=4)

    def expert_call(self, layer, expert, rows):
        return ExpertUnit(self.weights, layer, expert).forward(rows)

    def test_expert_unit_matches_reference(self):
        model = OlmoeForCausalLM.from_pretrained(self.dir, torch_dtype=torch.bfloat16)
        rows = torch.randn(5, 64, dtype=torch.bfloat16)
        for layer in range(2):
            for expert in range(4):
                mine = ExpertUnit(self.weights, layer, expert).forward(rows)
                with torch.no_grad():
                    ref = model.model.layers[layer].mlp.experts[expert](rows)
                self.assertLess(rel_l2(mine, ref), 1e-6,
                                f'expert {layer}/{expert} drift')

    def test_attention_stage_matches_reference(self):
        model = OlmoeForCausalLM.from_pretrained(self.dir, torch_dtype=torch.bfloat16)
        model.eval()
        ids = [3, 17, 42, 9, 100, 5]
        stage = AttentionStage(self.weights, self.layout, 0, 2,
                               expert_call=self.expert_call)
        with torch.no_grad():
            out, records, a_att, logits, _in = stage.forward_frame(
                'req', 0, input_ids=ids[:4])
        # Reference: drive the decoder layers' own submodules directly.
        with torch.no_grad():
            x = model.model.embed_tokens(torch.tensor([ids[:4]]))
            pos = torch.arange(4).unsqueeze(0)
            cos, sin = model.model.rotary_emb(x, pos)
            ref_a_att = {}
            for li, layer in enumerate(model.model.layers):
                residual = x
                xn = layer.input_layernorm(x)
                attn_out, _, _ = layer.self_attn(xn, attention_mask=None,
                                                 position_ids=pos,
                                                 position_embeddings=(cos, sin))
                x = residual + attn_out
                ref_a_att[li] = layer.post_attention_layernorm(x)
                x = x + layer.mlp(ref_a_att[li])[0]
        for li in range(2):
            # bf16 cross-implementation band (MOE-0 calibration: expert
            # segments 3.6e-3..4.8e-3 cross-device); layer 1 cascades layer 0.
            self.assertLess(rel_l2(a_att[li], ref_a_att[li]), 6e-3,
                            f'A_att layer {li} drift')
        self.assertLess(rel_l2(out, x), 6e-3, 'stage output drift')
        # Routing records: same top-2 sets as the reference router.
        self.assertEqual(len(records), 2)
        for li, record in enumerate(records):
            gate = model.model.layers[li].mlp.gate
            with torch.no_grad():
                probs = torch.softmax(torch.nn.functional.linear(
                    ref_a_att[li].float(), gate.weight.float()), dim=-1)
            ref_top = probs.topk(2, dim=-1).indices.flatten().tolist()
            self.assertEqual(record['experts'], ref_top,
                             f'routing layer {li} differs')

    def test_two_frame_generation_matches_greedy(self):
        model = OlmoeForCausalLM.from_pretrained(self.dir, torch_dtype=torch.bfloat16)
        model.eval()
        ids = [3, 17, 42, 9]
        stage = AttentionStage(self.weights, self.layout, 0, 2,
                               expert_call=self.expert_call)
        with torch.no_grad():
            _, _, _, logits0, _i = stage.forward_frame('r2', 0, input_ids=ids)
            token0 = int(logits0.argmax())
            ref = model.generate(torch.tensor([ids]), max_new_tokens=3,
                                 do_sample=False)
            _, _, _, logits1, _i = stage.forward_frame('r2', 1, input_ids=[token0])
            token1 = int(logits1.argmax())
            _, _, _, logits2, _i = stage.forward_frame('r2', 2, input_ids=[token1])
            token2 = int(logits2.argmax())
        self.assertEqual([token0, token1, token2], ref[0, 4:].tolist(),
                         'frame-wise greedy tokens differ from whole-model')


class MoeRealWeightsTest(unittest.TestCase):
    """Real OLMoE layer-8 experts (present in the drill workspace)."""

    def test_layer8_expert(self):
        base = Path('/root/data/moe-shard-test/hf')
        if not (base / 'experts-l8.safetensors').exists():
            self.skipTest('real OLMoE shards not present')
        import json as _json
        # Assemble a minimal model dir view over the pre-downloaded shards:
        # MoeWeights reads a directory of safetensors; the hf/ dir holds the
        # full shard set from the earlier segment test plus experts-l8.
        weights = MoeWeights(base)
        if not weights.has('model.layers.8.mlp.experts.3.gate_proj.weight'):
            self.skipTest('layer 8 experts not in shard set')
        from transformers.models.olmoe.modeling_olmoe import OlmoeMLP
        cfg = OlmoeConfig.from_pretrained(base)
        torch.manual_seed(3)
        rows = torch.randn(6, 2048, dtype=torch.bfloat16)
        from safetensors.torch import load_file as _lf
        _sd_keys = _lf(str(base / 'experts-l8.safetensors')).keys()
        available = sorted({int(k.split('.')[5]) for k in _sd_keys})
        for expert in available:
            mine = ExpertUnit(weights, 8, expert).forward(rows)
            ref_module = OlmoeMLP(cfg)
            from safetensors.torch import load_file
            sd = load_file(str(base / 'experts-l8.safetensors'))
            prefix = f'model.layers.8.mlp.experts.{expert}.'
            ref_module.load_state_dict({
                'gate_proj.weight': sd[prefix + 'gate_proj.weight'],
                'up_proj.weight': sd[prefix + 'up_proj.weight'],
                'down_proj.weight': sd[prefix + 'down_proj.weight']}, strict=True)
            ref_module = ref_module.to(torch.bfloat16)
            with torch.no_grad():
                ref = ref_module(rows)
            self.assertLess(rel_l2(mine, ref), 1e-6, f'real expert 8/{expert} drift')


if __name__ == '__main__':
    unittest.main()
