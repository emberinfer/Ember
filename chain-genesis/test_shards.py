import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path[:0] = [str(ROOT / 'proto'), str(ROOT / 'core-py'), str(Path(__file__).parent)]
import torch
from safetensors import safe_open
from transformers import Qwen2Config, Qwen2ForCausalLM
from build_shards import build
from ember_core import digest, verify


class ShardBuilderTests(unittest.TestCase):
    def test_builds_independent_layer_artifacts(self):
        config = Qwen2Config(vocab_size=64, hidden_size=32, intermediate_size=64,
                            num_hidden_layers=4, num_attention_heads=4,
                            num_key_value_heads=2, max_position_embeddings=32,
                            tie_word_embeddings=True)
        model = Qwen2ForCausalLM(config).to(torch.bfloat16).eval()
        with tempfile.TemporaryDirectory() as root:
            source, output = Path(root) / 'model', Path(root) / 'shards'
            model.save_pretrained(source, safe_serialization=True)
            (source / 'tokenizer.json').write_text('{}')
            genesis, manifest = build(source, output, 2)
            verify(genesis); verify(manifest)
            self.assertEqual(genesis.model_manifest_root, digest(manifest))
            self.assertEqual([(s.start_layer, s.end_layer) for s in manifest.shards],
                             [(0, 2), (2, 4)])
            with safe_open(output / 'shard-000.safetensors', framework='pt') as first:
                first_keys = set(first.keys())
            with safe_open(output / 'shard-001.safetensors', framework='pt') as last:
                last_keys = set(last.keys())
            self.assertTrue(any(k.startswith('model.layers.0.') for k in first_keys))
            self.assertFalse(any(k.startswith('model.layers.2.') for k in first_keys))
            self.assertTrue(any(k.startswith('model.layers.3.') for k in last_keys))
            self.assertFalse(any(k.startswith('model.layers.1.') for k in last_keys))
            self.assertIn('model.embed_tokens.weight', first_keys)
            self.assertIn('model.embed_tokens.weight', last_keys)
            # The stage refuses locally modified weights before model loading.
            from shard_rpc import load_shard_anchor
            load_shard_anchor(output / 'genesis.pb', output / 'manifest.pb', 0, output)
            with open(output / 'shard-000.safetensors', 'ab') as stream:
                stream.write(b'tamper')
            with self.assertRaisesRegex(ValueError, 'hash mismatch'):
                load_shard_anchor(output / 'genesis.pb', output / 'manifest.pb', 0, output)

    def test_embedding_and_head_are_independent_executable_units(self):
        config = Qwen2Config(vocab_size=64, hidden_size=32, intermediate_size=64,
                            num_hidden_layers=4, num_attention_heads=4,
                            num_key_value_heads=2, max_position_embeddings=32,
                            tie_word_embeddings=True)
        config._attn_implementation = 'sdpa'
        model = Qwen2ForCausalLM(config).to(torch.bfloat16).eval()
        with tempfile.TemporaryDirectory() as root:
            source, output = Path(root) / 'model', Path(root) / 'units'
            model.save_pretrained(source, safe_serialization=True)
            (source / 'tokenizer.json').write_text('{}')
            _, manifest = build(source, output, 2, separate_boundaries=True)
            self.assertEqual([s.unit_kind for s in manifest.shards],
                             ['embedding', 'layers', 'layers', 'lm_head'])
            from engine.shard import Qwen2ShardEngine
            from shard_rpc import load_shard_anchor
            ids = torch.tensor([[1, 2]])
            value = None
            with torch.inference_mode():
                for spec in manifest.shards:
                    load_shard_anchor(output / 'genesis.pb', output / 'manifest.pb', spec.index, output)
                    engine = Qwen2ShardEngine(output, spec.start_layer, spec.end_layer,
                                             spec.has_embedding, spec.has_lm_head, 'cpu')
                    value = engine.forward(input_ids=ids) if spec.has_embedding else engine.forward(hidden_states=value)
                    if spec.unit_kind == 'embedding':
                        self.assertEqual(engine.loaded_keys, {'model.embed_tokens.weight'})
                    if spec.unit_kind == 'lm_head':
                        self.assertEqual(engine.loaded_keys, {'model.norm.weight', 'model.embed_tokens.weight'})
                self.assertTrue(torch.allclose(value, model(input_ids=ids).logits.float(), rtol=.01, atol=.01))

    def test_pipeline_union_policy_is_signed(self):
        config = Qwen2Config(vocab_size=64, hidden_size=32, intermediate_size=64,
                            num_hidden_layers=4, num_attention_heads=4,
                            num_key_value_heads=2, max_position_embeddings=32,
                            tie_word_embeddings=True)
        with tempfile.TemporaryDirectory() as root:
            source, output = Path(root) / 'model', Path(root) / 'shards'
            Qwen2ForCausalLM(config).to(torch.bfloat16).eval().save_pretrained(
                source, safe_serialization=True)
            (source / 'tokenizer.json').write_text('{}')
            genesis, _ = build(source, output, 2, 1, .02, b'x' * 32)
            verify(genesis)
            self.assertEqual(genesis.numeric_audit_policy.metric,
                             'union-interval-v1')
            self.assertEqual(genesis.numeric_audit_policy.bands[0].tolerance,
                             .02)


if __name__ == '__main__':
    unittest.main()
