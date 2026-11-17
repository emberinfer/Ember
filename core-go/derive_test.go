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
			for _, version := range []uint64{11, 12} {
				in.AppVersion = version
				if _, e := core.DeriveCompute(in); e == nil {
					t.Fatalf("version=%d contradiction accepted", version)
				}
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

func TestDeriveKimiRealTruth(t *testing.T) {
	in := computefixtures.KimiReal()
	in.AppVersion = 12
	r, err := core.DeriveCompute(in)
	if err != nil {
		t.Fatal(err)
	}
	if r.TotalParams != 2780110363392 || r.ActiveParams != 105542546176 || r.FlopsPerToken != 208736282112 || r.EmbeddingParams != 1174405120 || r.TensorBytes != 1476109481472 {
		t.Fatalf("wrong real Kimi truth: %+v", r)
	}
	root, err := core.ShapesRoot(in.Tensors)
	if err != nil || root != "1528bba79445a348cc63ff0323600cb4520c3a87b3103637a066b44138e41784" || len(in.Tensors) != 931 || len(r.LayerFlops) != 93 {
		t.Fatalf("inventory mismatch: root=%s tensors=%d layers=%d err=%v", root, len(in.Tensors), len(r.LayerFlops), err)
	}
	t.Logf("real Kimi T=%d A=%d F=%d E=%d bytes=%d root=%s", r.TotalParams, r.ActiveParams, r.FlopsPerToken, r.EmbeddingParams, r.TensorBytes, root)
}

func TestParseConfigTopKSurfaces(t *testing.T) {
	base := `{"num_hidden_layers":1,"hidden_size":2,"vocab_size":4,"num_experts":32,`
	for _, tc := range []struct {
		name string
		raw  []byte
		want uint64
		bad  bool
	}{
		{"real dialect", computefixtures.KimiReal().ConfigJSON, 16, false},
		{"conflicting architecture", []byte(base + `"num_experts_per_token":16,"num_experts_per_tok":8}`), 0, true},
		{"sampling fallback", tiny().ConfigJSON, 1, false},
		{"sampling out of expert range ignored", []byte(base + `"num_experts_per_token":8,"top_k":50}`), 8, false},
		{"sampling type ignored", []byte(base + `"num_selected_experts":8,"top_k":"sampling"}`), 8, false},
		{"consistent architecture", []byte(base + `"num_experts_per_token":8,"num_experts_per_tok":8,"num_selected_experts":8,"top_k":0}`), 8, false},
		{"invalid architecture cannot fall back", []byte(base + `"num_experts_per_token":0,"top_k":8}`), 0, true},
	} {
		t.Run("v12/"+tc.name, func(t *testing.T) {
			c, err := core.ParseConfigScalarsForVersion(tc.raw, 12)
			if (err != nil) != tc.bad || (!tc.bad && c.TopK != tc.want) {
				t.Fatalf("TopK=%d err=%v; want %d error=%v", c.TopK, err, tc.want, tc.bad)
			}
		})
	}
}

func TestDeriveKimiRealVersionGate(t *testing.T) {
	for _, version := range []uint64{0, 10, 11, 12, 13} {
		in := computefixtures.KimiReal()
		in.AppVersion = version
		r, err := core.DeriveCompute(in)
		wantA := uint64(208860836608)
		if version >= 12 {
			wantA = 105542546176
		}
		if err != nil || r.TotalParams != 2780110363392 || r.ActiveParams != wantA {
			t.Fatalf("version=%d result=%+v err=%v", version, r, err)
		}
		t.Logf("AppVersion=%d T=%d A=%d F=%d", version, r.TotalParams, r.ActiveParams, r.FlopsPerToken)
	}
}

func TestParseConfigTopKLegacyAndScalarParity(t *testing.T) {
	base := `{"num_hidden_layers":1,"hidden_size":2,"vocab_size":4,"num_experts":64,"routed_expert_hidden_size":3,"tie_word_embeddings":true,`
	for _, tc := range []struct {
		keys string
		want uint64
		bad  bool
	}{
		{`"top_k":50,"num_experts_per_token":16`, 50, false},
		{`"top_k":8`, 8, false},
		{`"num_experts_per_tok":8,"top_k":16`, 0, true},
		{`"num_selected_experts":8,"top_k":16`, 0, true},
		{`"num_experts_per_tok":8,"top_k":8,"num_selected_experts":8`, 8, false},
		{`"num_experts_per_token":8`, 0, true},
		{`"num_experts_per_tok":0,"top_k":8`, 0, true},
	} {
		raw := []byte(base + tc.keys + `}`)
		legacy, err := core.ParseConfigScalarsForVersion(raw, 11)
		if (err != nil) != tc.bad || (!tc.bad && legacy.TopK != tc.want) {
			t.Fatalf("v11 keys=%s result=%+v err=%v", tc.keys, legacy, err)
		}
		unversioned, oldErr := core.ParseConfigScalars(raw)
		if !reflect.DeepEqual(legacy, unversioned) || (err != nil) != (oldErr != nil) {
			t.Fatal("unversioned helper changed legacy behavior")
		}
		modern, _ := core.ParseConfigScalarsForVersion(raw, 12)
		legacy.TopK, modern.TopK = 0, 0
		if legacy != modern {
			t.Fatalf("non-TopK scalars changed: %+v %+v", legacy, modern)
		}
	}
}

func TestDeriveFixtureVersionParity(t *testing.T) {
	for _, in := range []core.DerivationInput{computefixtures.Kimi(), computefixtures.Model("qwen25"), computefixtures.Model("qwen36"), computefixtures.Model("olmoe")} {
		in.AppVersion = 11
		legacy, err := core.DeriveCompute(in)
		if err != nil {
			t.Fatal(err)
		}
		in.AppVersion = 12
		modern, err := core.DeriveCompute(in)
		if err != nil || !reflect.DeepEqual(legacy, modern) {
			t.Fatalf("fixture regression: legacy=%+v modern=%+v err=%v", legacy, modern, err)
		}
	}
}
