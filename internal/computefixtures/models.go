package computefixtures

import (
	_ "embed"
	core "ember/core-go"
	"fmt"
)

// Config snapshots from Qwen/Qwen2.5-1.5B-Instruct, Qwen/Qwen3.6-35B-A3B-FP8
// (the latter reports model_type qwen3_5_moe), and allenai/OLMoE-1B-7B-0924.
// Quantization metadata is omitted from the Qwen3.6 snapshot, not text structure.
//
//go:embed qwen25.json
var qwen25 []byte

//go:embed qwen36.json
var qwen36 []byte

//go:embed olmoe.json
var olmoe []byte

// Model contains config-derived main-text weight shapes. Qwen3.6 deliberately
// excludes the optional vision/MTP towers and FP8 scale metadata; it is a
// regression of text matrix shapes, not a full checkpoint inventory.
func Model(name string) core.DerivationInput {
	var in core.DerivationInput
	add := func(n string, s ...uint64) {
		in.Tensors = append(in.Tensors, core.TensorShape{Name: n, Dtype: "bf16", Shape: s})
	}
	switch name {
	case "qwen25":
		in.ConfigJSON = qwen25
		add("model.embed_tokens.weight", 151936, 1536)
		add("model.norm.weight", 1536)
		for l := 0; l < 28; l++ {
			p := fmt.Sprintf("model.layers.%d.", l)
			for _, v := range []struct {
				n    string
				a, b uint64
			}{{"self_attn.q_proj", 1536, 1536}, {"self_attn.k_proj", 256, 1536}, {"self_attn.v_proj", 256, 1536}, {"self_attn.o_proj", 1536, 1536}, {"mlp.gate_proj", 8960, 1536}, {"mlp.up_proj", 8960, 1536}, {"mlp.down_proj", 1536, 8960}} {
				add(p+v.n+".weight", v.a, v.b)
			}
			add(p+"self_attn.q_proj.bias", 1536)
			add(p+"self_attn.k_proj.bias", 256)
			add(p+"self_attn.v_proj.bias", 256)
			add(p+"input_layernorm.weight", 1536)
			add(p+"post_attention_layernorm.weight", 1536)
		}
	case "olmoe":
		in.ConfigJSON = olmoe
		add("model.embed_tokens.weight", 50304, 2048)
		add("lm_head.weight", 50304, 2048)
		add("model.norm.weight", 2048)
		for l := 0; l < 16; l++ {
			p := fmt.Sprintf("model.layers.%d.", l)
			for _, n := range []string{"q_proj", "k_proj", "v_proj", "o_proj"} {
				add(p+"self_attn."+n+".weight", 2048, 2048)
			}
			for _, n := range []string{"q_norm", "k_norm"} {
				add(p+"self_attn."+n+".weight", 2048)
			}
			add(p+"input_layernorm.weight", 2048)
			add(p+"post_attention_layernorm.weight", 2048)
			add(p+"mlp.gate.weight", 64, 2048)
			for _, n := range []string{"gate_proj", "up_proj", "down_proj"} {
				add(p+"mlp.experts."+n+".weight", 64, 2048, 1024)
			}
		}
	case "qwen36":
		in.ConfigJSON = qwen36
		add("model.language_model.embed_tokens.weight", 248320, 2048)
		add("lm_head.weight", 248320, 2048)
		add("model.language_model.norm.weight", 2048)
		for l := 0; l < 40; l++ {
			p := fmt.Sprintf("model.language_model.layers.%d.", l)
			add(p+"input_layernorm.weight", 2048)
			add(p+"post_attention_layernorm.weight", 2048)
			add(p+"mlp.gate.weight", 256, 2048)
			add(p+"mlp.shared_expert_gate.weight", 1, 2048)
			for _, n := range []string{"gate_proj", "up_proj", "down_proj"} {
				add(p+"mlp.experts."+n+".weight", 256, 2048, 512)
				add(p+"mlp.shared_experts."+n+".weight", 2048, 512)
			}
			if l%4 == 3 {
				add(p+"self_attn.q_proj.weight", 8192, 2048)
				add(p+"self_attn.k_proj.weight", 512, 2048)
				add(p+"self_attn.v_proj.weight", 512, 2048)
				add(p+"self_attn.o_proj.weight", 2048, 4096)
				add(p+"self_attn.q_norm.weight", 256)
				add(p+"self_attn.k_norm.weight", 256)
			} else {
				add(p+"linear_attn.in_proj_qkv.weight", 8192, 2048)
				add(p+"linear_attn.in_proj_z.weight", 4096, 2048)
				add(p+"linear_attn.in_proj_b.weight", 32, 2048)
				add(p+"linear_attn.in_proj_a.weight", 32, 2048)
				add(p+"linear_attn.out_proj.weight", 2048, 4096)
				add(p+"linear_attn.conv1d.weight", 8192, 1, 4)
				add(p+"linear_attn.A_log", 32)
				add(p+"linear_attn.dt_bias", 32)
				add(p+"linear_attn.norm.weight", 128)
			}
		}
	default:
		panic("unknown fixture")
	}
	return in
}
