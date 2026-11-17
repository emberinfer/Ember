package computefixtures

import (
	_ "embed"
	core "ember/core-go"
	"fmt"
)

//go:embed kimi_real.json
var kimiRealConfig []byte

// KimiReal pairs the real Kimi-K3.1-Pro config dialect with the governance
// 93-layer, 931-tensor inventory. Layer zero has no routed experts; nonexpert
// classes are aggregates, not original checkpoint headers.
func KimiReal() core.DerivationInput {
	var ts []core.TensorShape
	add := func(n, d string, s ...uint64) {
		ts = append(ts, core.TensorShape{Name: n, Dtype: d, Shape: s})
	}
	add("model.embed_tokens.weight", "bf16", 163840, 7168)
	add("lm_head.weight", "bf16", 163840, 7168)
	add("model.norm.weight", "bf16", 7168)
	add("vision.aggregate", "bf16", 447358976)
	const layers = 93
	const aggSum = uint64(29667700864)
	base := aggSum / layers
	rem := aggSum % layers
	for l := 0; l < layers; l++ {
		p := fmt.Sprintf("model.layers.%d.", l)
		if l >= 1 {
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
		n := base
		if uint64(l) < rem {
			n++
		}
		add(p+"mlp.shared_experts.aggregate", "bf16", n)
	}
	return core.DerivationInput{ConfigJSON: kimiRealConfig, Tensors: ts}
}
