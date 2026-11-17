package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type TensorShape struct {
	Name  string   `json:"name"`
	Shape []uint64 `json:"shape"`
	Dtype string   `json:"dtype"`
}
type DerivationInput struct {
	// AppVersion selects consensus TopK semantics; zero retains legacy behavior.
	AppVersion                                                       uint64
	ConfigJSON                                                       []byte
	Tensors                                                          []TensorShape
	DeclaredLayerFlops                                               []uint64
	DeclaredEmbeddingFlops, DeclaredHeadFlops                        uint64
	DeclaredTotalParams, DeclaredActiveParams, DeclaredFlopsPerToken uint64
}
type DerivationResult struct {
	TotalParams, ActiveParams, EmbeddingParams, FlopsPerToken uint64
	LayerFlops                                                []uint64
	EmbeddingFlops, HeadFlops, TensorBytes                    uint64
}
type ConfigScalars struct {
	Layers, HiddenSize, NumExperts, TopK, VocabSize, RoutedExpertHiddenSize uint64
	TieWordEmbeddings                                                       bool
}

// Conflicting architecture aliases fail closed. Config supplies structure, never parameter counts.
// At AppVersion >= 12, TopK uses architecture aliases before sampling top_k.
// Earlier versions retain the exact v11 alias order and conflict behavior.
// The unversioned helper retains legacy semantics.
// V12 uses num_experts_per_token, num_experts_per_tok, or num_selected_experts.
// Only when none is present does the HF sampling field top_k supply a legacy
// fallback; otherwise top_k is completely ignored, including its type and range.
func ParseConfigScalars(raw []byte) (ConfigScalars, error) {
	return ParseConfigScalarsForVersion(raw, 0)
}

// ParseConfigScalarsForVersion changes only TopK key selection at version 12.
func ParseConfigScalarsForVersion(raw []byte, appVersion uint64) (c ConfigScalars, err error) {
	var m map[string]json.RawMessage
	if err = json.Unmarshal(raw, &m); err != nil {
		return
	}
	// Multimodal dialects put the text structure under text_config.
	if v, ok := m["text_config"]; ok {
		var text map[string]json.RawMessage
		if e := json.Unmarshal(v, &text); e != nil {
			return c, e
		}
		if len(text) == 0 {
			return c, fmt.Errorf("empty text_config")
		}
		m = text
	}
	read := func(dst *uint64, keys ...string) {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				var n uint64
				if e := json.Unmarshal(v, &n); e != nil || n == 0 || (*dst != 0 && *dst != n) {
					err = fmt.Errorf("invalid/conflicting config scalar %s", k)
					return
				}
				*dst = n
			}
		}
	}
	read(&c.Layers, "num_hidden_layers", "n_layer")
	read(&c.HiddenSize, "hidden_size", "n_embd")
	read(&c.NumExperts, "num_experts", "n_routed_experts", "num_local_experts")
	if appVersion < 12 {
		read(&c.TopK, "num_experts_per_tok", "top_k", "num_selected_experts")
	} else {
		topKKeys := []string{"num_experts_per_token", "num_experts_per_tok", "num_selected_experts"}
		architectureTopK := false
		for _, key := range topKKeys {
			if _, ok := m[key]; ok {
				architectureTopK = true
			}
		}
		if architectureTopK {
			read(&c.TopK, topKKeys...)
		} else {
			read(&c.TopK, "top_k")
		}
	}
	read(&c.VocabSize, "vocab_size")
	read(&c.RoutedExpertHiddenSize, "routed_expert_hidden_size")
	if v, ok := m["tie_word_embeddings"]; ok {
		if e := json.Unmarshal(v, &c.TieWordEmbeddings); e != nil {
			err = e
		}
	}
	if c.Layers == 0 || c.HiddenSize == 0 || c.VocabSize == 0 || (c.NumExperts == 0) != (c.TopK == 0) || c.TopK > c.NumExperts {
		err = fmt.Errorf("missing/invalid structural scalars")
	}
	return
}

// ShapesRoot hashes exact names, stored dimensions and dtype spellings in name order.
func ShapesRoot(tensors []TensorShape) (string, error) {
	ts := append([]TensorShape(nil), tensors...)
	sort.Slice(ts, func(i, j int) bool { return ts[i].Name < ts[j].Name })
	for i, t := range ts {
		if t.Name == "" || (i > 0 && ts[i-1].Name == t.Name) {
			return "", fmt.Errorf("duplicate/empty tensor name")
		}
	}
	raw, err := json.Marshal(ts)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:]), nil
}

var layerName = regexp.MustCompile(`(?:^|\.)(?:layers|h|blocks)\.([0-9]+)(?:\.|$)`)
var expertName = regexp.MustCompile(`(?:^|\.)experts\.([0-9]+)(?:\.|$)`)

func deriveAdd(dst *uint64, n uint64) error {
	if n > ^uint64(0)-*dst {
		return fmt.Errorf("compute overflow")
	}
	*dst += n
	return nil
}
func deriveMul(a, b uint64) (uint64, error) {
	if b != 0 && a > ^uint64(0)/b {
		return 0, fmt.Errorf("compute overflow")
	}
	return a * b, nil
}
func ratUint(r *big.Rat) (uint64, error) {
	if !r.IsInt() || !r.Num().IsUint64() {
		return 0, fmt.Errorf("nonintegral or overflowing active compute")
	}
	return r.Num().Uint64(), nil
}

// Protocol classification priority: vision, lookup, head, routed experts, other.
// Indexed experts require a complete [0,N) index set per layer. Packed experts
// require leading dimension N. Shared experts never match the routed pattern.
// FP4/INT4 names describe byte-packed storage: last stored dimension contains two
// logical elements; numel(logical)=2*numel(stored), bytes=numel(stored). Other
// dtypes describe ordinary logical shapes. Unindexed non-boundary tensors go to
// the final decoder layer (norms included); without any layer indices no per-layer
// attribution is claimed. No rounding: A and every layer's FLOPs must be integral.
func DeriveCompute(in DerivationInput) (out DerivationResult, err error) {
	fail := func(s string) (DerivationResult, error) {
		return DerivationResult{}, fmt.Errorf("compute derivation: %s", s)
	}
	c, e := ParseConfigScalarsForVersion(in.ConfigJSON, in.AppVersion)
	if e != nil {
		return out, e
	}
	if len(in.Tensors) == 0 {
		return fail("missing tensors")
	}
	if _, e = ShapesRoot(in.Tensors); e != nil {
		return out, e
	}
	ts := append([]TensorShape(nil), in.Tensors...)
	sort.Slice(ts, func(i, j int) bool { return ts[i].Name < ts[j].Name })
	layers := map[uint64]bool{}
	indexed := map[uint64]map[uint64]bool{}
	active := new(big.Rat)
	head := uint64(0)
	byLayer := map[uint64]*big.Rat{}
	unassigned := new(big.Rat)
	for _, t := range ts {
		if len(t.Shape) == 0 {
			return fail("empty shape")
		}
		stored := uint64(1)
		for _, d := range t.Shape {
			if d == 0 {
				return fail("zero dimension")
			}
			stored, e = deriveMul(stored, d)
			if e != nil {
				return out, e
			}
		}
		n := stored
		width := uint64(0)
		dt := strings.ToLower(t.Dtype)
		switch dt {
		case "bf16", "bfloat16", "fp16", "f16", "float16":
			width = 2
		case "fp32", "f32", "float32":
			width = 4
		case "fp4", "int4", "fp4_packed", "int4_packed":
			if len(t.Shape) < 2 {
				return fail("ambiguous packed shape")
			}
			n, e = deriveMul(stored, 2)
			width = 1
		default:
			if strings.HasPrefix(dt, "fp8") || strings.HasPrefix(dt, "f8_") {
				width = 1
			} else {
				return fail("unsupported dtype")
			}
		}
		if e != nil {
			return out, e
		}
		nb, e := deriveMul(stored, width)
		if e != nil {
			return out, e
		}
		if e = deriveAdd(&out.TensorBytes, nb); e != nil {
			return out, e
		}
		if e = deriveAdd(&out.TotalParams, n); e != nil {
			return out, e
		}
		name := strings.ToLower(t.Name)
		if strings.Contains(name, "vision") || strings.Contains(name, "visual") {
			continue
		}
		li := layerName.FindStringSubmatch(name)
		layer := uint64(0)
		if li != nil {
			layer, e = strconv.ParseUint(li[1], 10, 64)
			if e != nil || layer >= c.Layers {
				return fail("layer index outside config")
			}
			layers[layer] = true
		}
		if strings.Contains(name, "embed_tokens") || strings.Contains(name, "embedding") {
			dims := append([]uint64(nil), t.Shape...)
			if dt == "fp4" || dt == "int4" || dt == "fp4_packed" || dt == "int4_packed" {
				dims[len(dims)-1] *= 2
			}
			if len(dims) != 2 || !((dims[0] == c.VocabSize && dims[1] == c.HiddenSize) || (dims[1] == c.VocabSize && dims[0] == c.HiddenSize)) {
				return fail("embedding/config shape contradiction")
			}
			if e = deriveAdd(&out.EmbeddingParams, n); e != nil {
				return out, e
			}
			active.Add(active, new(big.Rat).SetUint64(n))
			continue
		}
		// Input attention projections expose the text hidden dimension.
		if li != nil && len(t.Shape) == 2 && (strings.HasSuffix(name, "q_proj.weight") || strings.HasSuffix(name, "k_proj.weight") || strings.HasSuffix(name, "v_proj.weight") || strings.HasSuffix(name, "q_a_proj.weight") || strings.HasSuffix(name, "kv_a_proj_with_mqa.weight")) {
			last := t.Shape[1]
			if dt == "fp4" || dt == "int4" || dt == "fp4_packed" || dt == "int4_packed" {
				last *= 2
			}
			if last != c.HiddenSize {
				return fail("input projection/config hidden contradiction")
			}
		}
		share := new(big.Rat).SetUint64(n)
		if strings.Contains(name, "experts.") && !strings.Contains(name, "shared_experts.") {
			if c.NumExperts == 0 || li == nil {
				return fail("expert routing/config missing")
			}
			ex := expertName.FindStringSubmatch(name)
			if ex != nil {
				idx, e := strconv.ParseUint(ex[1], 10, 64)
				if e != nil || idx >= c.NumExperts {
					return fail("expert index outside config")
				}
				if indexed[layer] == nil {
					indexed[layer] = map[uint64]bool{}
				}
				indexed[layer][idx] = true
			} else if len(t.Shape) < 3 || t.Shape[0] != c.NumExperts || n%c.NumExperts != 0 {
				return fail("packed expert count contradiction")
			}
			share.Mul(share, new(big.Rat).SetFrac(new(big.Int).SetUint64(c.TopK), new(big.Int).SetUint64(c.NumExperts)))
		}
		active.Add(active, share)
		if strings.Contains(name, "lm_head") || strings.HasPrefix(name, "output.") {
			if e = deriveAdd(&head, n); e != nil {
				return out, e
			}
			continue
		}
		if li != nil {
			if byLayer[layer] == nil {
				byLayer[layer] = new(big.Rat)
			}
			byLayer[layer].Add(byLayer[layer], share)
		} else {
			unassigned.Add(unassigned, share)
		}
	}
	if len(layers) > 0 && uint64(len(layers)) != c.Layers {
		return fail("layer count contradiction")
	}
	for l := uint64(0); l < uint64(len(layers)); l++ {
		if ids := indexed[l]; ids != nil && uint64(len(ids)) != c.NumExperts {
			return fail("incomplete expert index range")
		}
	}
	out.ActiveParams, e = ratUint(active)
	if e != nil {
		return out, e
	}
	out.HeadFlops, e = deriveMul(head, 2)
	if e != nil {
		return out, e
	}
	out.FlopsPerToken, e = deriveMul(out.ActiveParams-out.EmbeddingParams, 2)
	if e != nil {
		return out, e
	}
	if len(layers) == 0 {
		if len(in.DeclaredLayerFlops) > 0 {
			return fail("per-layer attribution impossible")
		}
		return out, nil
	}
	if uint64(len(layers)) != c.Layers {
		return fail("layer count contradiction")
	}
	// Bound allocation by the already validated, observed number of layers.
	out.LayerFlops = make([]uint64, len(layers))
	last := c.Layers - 1
	if byLayer[last] == nil {
		byLayer[last] = new(big.Rat)
	}
	byLayer[last].Add(byLayer[last], unassigned)
	sum := out.HeadFlops
	for i := range out.LayerFlops {
		r := byLayer[uint64(i)]
		if r == nil {
			r = new(big.Rat)
		}
		r = new(big.Rat).Mul(r, big.NewRat(2, 1))
		out.LayerFlops[i], e = ratUint(r)
		if e != nil {
			return out, e
		}
		if e = deriveAdd(&sum, out.LayerFlops[i]); e != nil {
			return out, e
		}
	}
	if sum != out.FlopsPerToken {
		return fail("layer sum contradiction")
	}
	return out, nil
}
