import unittest
from compute import compute_table


class ComputeTests(unittest.TestCase):
    def test_qwen2_gqa_and_boundary_atoms(self):
        config = dict(model_type='qwen2', hidden_size=16, intermediate_size=32,
                      num_attention_heads=4, num_key_value_heads=2,
                      num_hidden_layers=3, vocab_size=100, tie_word_embeddings=True)
        layers, embedding, head = compute_table(config)
        # Q/O:512, K/V:256, MLP:1536, QKV biases:32, norms:32.
        self.assertEqual(layers, [2 * 2368] * 3)
        self.assertEqual(embedding, 0)
        self.assertEqual(head, 2 * 1616)
        config['model_type'] = 'qwen2_moe'
        with self.assertRaises(ValueError):
            compute_table(config)
