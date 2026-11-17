package chain

import (
	"bytes"
	"crypto/sha256"
	core "ember/core-go"
	pb "ember/proto"
	"encoding/hex"
	"encoding/json"
	"errors"
	"google.golang.org/protobuf/proto"
	"io"
)

// Model identity never contains card ID, publisher, source, device or layout.
type ModelIdentity struct {
	ModelHash     string `json:"model_hash"`
	WeightRoot    string `json:"weight_root"`
	TokenizerHash string `json:"tokenizer_hash"`
	ConfigRoot    string `json:"config_root"`
}

func identityFor(g *pb.ChainGenesis, m *pb.ModelManifest) (*ModelIdentity, error) {
	w := weightRoot(m)
	if w == "" {
		return nil, nil
	}
	if len(m.CanonicalConfig) == 0 {
		return nil, nil
	} // legacy read: missing components are not invented
	config := m.ConfigBytes
	if len(config) == 0 {
		return nil, errors.New("canonical identity requires exact config bytes")
	}
	rawHash := sha256.Sum256(config)
	if !bytes.Equal(rawHash[:], m.ConfigHash) {
		return nil, errors.New("model config bytes differ from manifest hash")
	}
	canonical, err := CanonicalModelConfig(config)
	if err != nil {
		return nil, err
	}

	if !bytes.Equal(canonical, m.CanonicalConfig) || len(g.TokenizerHash) != 32 {
		return nil, errors.New("noncanonical model config or missing tokenizer hash")
	}
	c := sha256.Sum256(canonical)
	wb, _ := hex.DecodeString(w)
	raw := append([]byte("ember-model-v1\x00"), wb...)
	raw = append(raw, g.TokenizerHash...)
	raw = append(raw, c[:]...)
	h := sha256.Sum256(raw)
	return &ModelIdentity{hex.EncodeToString(h[:]), w, hex.EncodeToString(g.TokenizerHash), hex.EncodeToString(c[:])}, nil
}
func schemeHash(s *pb.LayoutScheme) []byte {
	raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(s)
	h := sha256.Sum256(append([]byte("ember-layout-v1\x00"), raw...))
	return h[:]
}
func schemeManifest(m *pb.ModelManifest, s *pb.LayoutScheme) *pb.ModelManifest {
	n := proto.Clone(m).(*pb.ModelManifest)
	n.Schemes = nil
	n.Shards = nil
	n.TotalLayers = s.TotalLayers
	n.MinStageLayers = s.MinStageLayers
	n.AllowedLayerCuts = s.AllowedLayerCuts
	n.DynamicRanges = true
	n.LayerFlops = s.LayerFlops
	n.EmbeddingFlops = s.EmbeddingFlops
	n.HeadFlops = s.HeadFlops
	n.MoeLayout = s.MoeLayout
	if s.MoeLayout != nil {
		n.Shards = s.Atoms
		n.DynamicRanges = false
	}
	return n
}
func selectScheme(m *pb.ModelManifest, hash []byte) (*pb.ModelManifest, error) {
	if len(m.Schemes) == 0 {
		if len(hash) > 0 {
			return nil, errors.New("unknown scheme hash")
		}
		return m, nil
	}
	for _, s := range m.Schemes {
		if bytes.Equal(schemeHash(s), hash) {
			return schemeManifest(m, s), nil
		}
	}
	return nil, errors.New("unknown or missing scheme hash")
}

// SelectScheme resolves the layout identified by hash using the signed schemes.
func SelectScheme(m *pb.ModelManifest, hash []byte) (*pb.ModelManifest, error) {
	return selectScheme(m, hash)
}

// ResolvePipelineManifest resolves an unambiguous signed pipeline layout.
func ResolvePipelineManifest(m *pb.ModelManifest) (*pb.ModelManifest, error) {
	if len(m.Schemes) == 0 {
		return m, nil
	}
	if len(m.Schemes) > 1 {
		return nil, errors.New("multiple layout schemes require explicit selection")
	}
	if err := validateSchemeAtoms(m.Schemes[0]); err != nil {
		return nil, err
	}
	resolved := schemeManifest(m, m.Schemes[0])
	if resolved.TotalLayers == 0 {
		return nil, errors.New("pipeline layout requires nonzero total layers")
	}
	if resolved.DynamicRanges && (resolved.MinStageLayers == 0 || resolved.MinStageLayers > resolved.TotalLayers) {
		return nil, errors.New("pipeline layout has invalid minimum stage layers")
	}
	return resolved, nil
}

func allowedSchemeRange(m *pb.ModelManifest, h []byte, start, end uint32) bool {
	s, e := selectScheme(m, h)
	return e == nil && core.AllowedRange(s, start, end)
}

func validateSchemeAtoms(s *pb.LayoutScheme) error {
	if s.MoeLayout != nil {
		return nil
	} // MoE table is validated by ValidateMoeLayout on dispatch.
	if uint64(len(s.Atoms)) != uint64(s.TotalLayers)+2 || len(s.LayerFlops) != int(s.TotalLayers) {
		return errors.New("scheme requires complete atom and compute tables")
	}
	for i, a := range s.Atoms {
		start, end := uint32(0), uint32(0)
		kind := "embedding"
		if i == len(s.Atoms)-1 {
			start = s.TotalLayers
			end = s.TotalLayers
			kind = "lm_head"
		} else if i > 0 {
			start = uint32(i - 1)
			end = uint32(i)
			kind = "layers"
		}
		if a == nil || a.UnitKind != kind || a.StartLayer != start || a.EndLayer != end || a.HasEmbedding != (i == 0) || a.HasLmHead != (i == len(s.Atoms)-1) || len(a.ParametersHash) != 32 {
			return errors.New("invalid canonical scheme atom table")
		}
	}
	return nil
}

// CanonicalModelConfig is shared by consensus and the offline publishing CLI.
func CanonicalModelConfig(config []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(config))
	var obj map[string]any
	if dec.Decode(&obj) != nil || obj == nil {
		return nil, errors.New("invalid model config")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return nil, errors.New("trailing model config")
	}
	return json.Marshal(obj)
}
