package core_test

import (
	"bytes"
	core "ember/core-go"
	"ember/internal/computefixtures"
	"reflect"
	"strings"
	"testing"
)

func TestDeriveKimiTruth(t *testing.T) {
	in := computefixtures.Kimi()
	r, e := core.DeriveCompute(in)
	if e != nil {
		t.Fatal(e)
	}
	if r.TotalParams != in.DeclaredTotalParams || r.ActiveParams != in.DeclaredActiveParams || r.EmbeddingParams != 1174405120 || r.FlopsPerToken != in.DeclaredFlopsPerToken || r.EmbeddingFlops != 0 || r.HeadFlops != 2348810240 {
		t.Fatalf("wrong Kimi truth: %+v", r)
	}
	if r.FlopsPerToken == 250580697088 {
		t.Fatal("accepted old wrong total")
	}
	root, _ := core.ShapesRoot(in.Tensors)
	for i, j := 0, len(in.Tensors)-1; i < j; i, j = i+1, j-1 {
		in.Tensors[i], in.Tensors[j] = in.Tensors[j], in.Tensors[i]
	}
	other, _ := core.ShapesRoot(in.Tensors)
	again, e := core.DeriveCompute(in)
	if e != nil || root != other || !reflect.DeepEqual(r, again) {
		t.Fatal("order dependence", e)
	}
	t.Logf("Kimi T=%d A=%d E=%d F=%d bytes=%d", r.TotalParams, r.ActiveParams, r.EmbeddingParams, r.FlopsPerToken, r.TensorBytes)
}
func tiny() core.DerivationInput {
	return core.DerivationInput{ConfigJSON: []byte(`{"num_hidden_layers":1,"hidden_size":2,"vocab_size":4,"num_experts":3,"top_k":1}`), Tensors: []core.TensorShape{{Name: "embed_tokens.weight", Shape: []uint64{4, 2}, Dtype: "bf16"}, {Name: "layers.0.experts.w1", Shape: []uint64{3, 2, 2}, Dtype: "bf16"}}}
}
func TestDeriveRejectsContradictions(t *testing.T) {
	cases := []struct {
		name   string
		change func(*core.DerivationInput)
	}{
		{"hidden", func(in *core.DerivationInput) { in.Tensors[0].Shape = []uint64{4, 3} }},
		{"layers", func(in *core.DerivationInput) {
			in.ConfigJSON = bytes.ReplaceAll(in.ConfigJSON, []byte(`"num_hidden_layers":1`), []byte(`"num_hidden_layers":2`))
		}},
		{"experts", func(in *core.DerivationInput) { in.Tensors[1].Shape[0] = 2 }},
		{"fractional", func(in *core.DerivationInput) {
			in.Tensors = []core.TensorShape{in.Tensors[0], {Name: "layers.0.experts.0.w", Shape: []uint64{1}, Dtype: "bf16"}, {Name: "layers.0.experts.1.w", Shape: []uint64{1}, Dtype: "bf16"}, {Name: "layers.0.experts.2.w", Shape: []uint64{2}, Dtype: "bf16"}}
		}},
		{"duplicate", func(in *core.DerivationInput) { in.Tensors = append(in.Tensors, in.Tensors[0]) }},
		{"zero", func(in *core.DerivationInput) { in.Tensors[1].Shape[1] = 0 }},
		{"overflow", func(in *core.DerivationInput) { in.Tensors[1].Shape[1] = ^uint64(0) }},
		{"dtype", func(in *core.DerivationInput) { in.Tensors[1].Dtype = "unknown" }},
		{"fp4ambiguous", func(in *core.DerivationInput) { in.Tensors[1].Shape = []uint64{3}; in.Tensors[1].Dtype = "fp4" }},
		{"missingindices", func(in *core.DerivationInput) { in.Tensors[1].Name = "layers.0.experts.0.w" }},
		{"projection", func(in *core.DerivationInput) {
			in.Tensors = append(in.Tensors, core.TensorShape{Name: "layers.0.self_attn.q_proj.weight", Shape: []uint64{2, 3}, Dtype: "bf16"})
		}},
		{"aliases", func(in *core.DerivationInput) {
			in.ConfigJSON = []byte(`{"num_hidden_layers":1,"n_layer":2,"hidden_size":2,"vocab_size":4}`)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tiny()
			tc.change(&in)
			if _, e := core.DeriveCompute(in); e == nil {
				t.Fatal("contradiction accepted")
			}
		})
	}
}
func TestDeriveExactFractionsAndFallback(t *testing.T) {
	in := tiny()
	in.Tensors = in.Tensors[:1]
	for i, n := range []uint64{1, 1, 1} {
		in.Tensors = append(in.Tensors, core.TensorShape{Name: []string{"layers.0.experts.0.w", "layers.0.experts.1.w", "layers.0.experts.2.w"}[i], Shape: []uint64{n}, Dtype: "bf16"})
	}
	r, e := core.DeriveCompute(in)
	if e != nil || r.ActiveParams != 9 || r.FlopsPerToken != 2 {
		t.Fatal(r, e)
	}
	in = tiny()
	in.ConfigJSON = []byte(`{"num_hidden_layers":1,"hidden_size":2,"vocab_size":4,"tie_word_embeddings":true}`)
	in.Tensors[1] = core.TensorShape{Name: "dense.weight", Shape: []uint64{2, 2}, Dtype: "fp4"}
	r, e = core.DeriveCompute(in)
	if e != nil || r.LayerFlops != nil || r.HeadFlops != 0 || r.TensorBytes != 20 || r.FlopsPerToken != 16 {
		t.Fatal(r, e)
	}
	in.DeclaredLayerFlops = []uint64{16}
	if _, e = core.DeriveCompute(in); e == nil || !strings.Contains(e.Error(), "attribution") {
		t.Fatal(e)
	}
}

func TestDeriveExistingModels(t *testing.T) {
	for _, name := range []string{"qwen25", "qwen36", "olmoe"} {
		t.Run(name, func(t *testing.T) {
			r, e := core.DeriveCompute(computefixtures.Model(name))
			if e != nil {
				t.Fatal(e)
			}
			t.Logf("T=%d A=%d E=%d F=%d head=%d", r.TotalParams, r.ActiveParams, r.EmbeddingParams, r.FlopsPerToken, r.HeadFlops)
			want := map[string][4]uint64{"qwen25": {1543714304, 1543714304, 233373696, 2620681216}, "qwen36": {34660610688, 3454988928, 508559360, 5892859136}, "olmoe": {6919161856, 1282017280, 103022592, 2357989376}}[name]
			if [4]uint64{r.TotalParams, r.ActiveParams, r.EmbeddingParams, r.FlopsPerToken} != want {
				t.Fatal("regression", r)
			}
		})
	}
}
