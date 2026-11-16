// Package computefixtures contains synthetic stored headers, never live weights.
package computefixtures

import (
	core "ember/core-go"
	"fmt"
)

// Kimi preserves all four exact published truth totals and the exact routed
// total. 92 routed blocks give 2,722,740,830,208 logical parameters. Layer 0
// enumerates every expert's three true packed matrix shapes; other layers use
// equivalent expert-major packed tensors. Nonexpert classes are aggregates,
// not a claim to reproduce the unavailable 96 original shard headers.
func Kimi() core.DerivationInput {
	in := core.DerivationInput{ConfigJSON: []byte(`{"num_hidden_layers":92,"hidden_size":7168,"vocab_size":163840,"n_routed_experts":896,"num_experts_per_tok":16,"routed_expert_hidden_size":3584,"tie_word_embeddings":false}`), DeclaredTotalParams: 2780110363392, DeclaredActiveParams: 105542546176, DeclaredFlopsPerToken: 208736282112}
	add := func(n, d string, s ...uint64) {
		in.Tensors = append(in.Tensors, core.TensorShape{Name: n, Dtype: d, Shape: s})
	}
	add("model.embed_tokens.weight", "bf16", 163840, 7168)
	add("lm_head.weight", "bf16", 163840, 7168)
	add("model.norm.weight", "bf16", 7168)
	add("vision.aggregate", "bf16", 447358976)
	// Active nonlookup/nonrouted total is 55,747,769,088, including the head.
	// Backbone + shared experts/projections/router aggregates retain this exact sum.
	left := uint64(55747769088 - 1174405120 - 7168)
	per := left / 92
	rem := left % 92
	for l := 0; l < 92; l++ {
		p := fmt.Sprintf("model.layers.%d.", l)
		if l == 0 {
			for x := 0; x < 896; x++ {
				q := fmt.Sprintf("%smlp.experts.%d.", p, x)
				add(q+"w1", "fp4", 3072, 1792)
				add(q+"w3", "fp4", 3072, 1792)
				add(q+"w2", "fp4", 3584, 1536)
			}
		} else {
			add(p+"mlp.experts.w1", "fp4", 896, 3072, 1792)
			add(p+"mlp.experts.w3", "fp4", 896, 3072, 1792)
			add(p+"mlp.experts.w2", "fp4", 896, 3584, 1536)
		}
		add(p+"shared_input.weight", "bf16", 3584, 7168)
		add(p+"shared_output.weight", "bf16", 7168, 3584)
		add(p+"self_attn.mla.weight", "bf16", 100000000)
		add(p+"self_attn.kda.weight", "bf16", 100000000)
		add(p+"self_attn.dsa.weight", "bf16", 10000000)
		add(p+"mlp.gate.weight", "bf16", 896, 7168)
		n := per - 2*3584*7168 - 210000000 - 896*7168
		if uint64(l) < rem {
			n++
		}
		add(p+"mlp.shared_experts.aggregate", "bf16", n)
	}
	return in
}
