package chain

import (
	"bytes"
	"crypto/sha256"
	core "ember/core-go"
	pb "ember/proto"
	"fmt"
	"google.golang.org/protobuf/proto"
	"reflect"
	"strings"
)

type VerifiedCompute struct {
	TotalParams     uint64   `json:"total_params"`
	ActiveParams    uint64   `json:"active_params"`
	FlopsPerToken   uint64   `json:"flops_per_token"`
	LayerFlops      []uint64 `json:"layer_flops"`
	HeadFlops       uint64   `json:"head_flops"`
	ShapesRoot      string   `json:"shapes_root"`
	CorrectedHeight int64    `json:"corrected_height"`
}
type ComputeCorrection struct {
	Height           int64  `json:"height"`
	Governor         string `json:"governor"`
	OldFlopsPerToken uint64 `json:"old_flops_per_token"`
	NewFlopsPerToken uint64 `json:"new_flops_per_token"`
	OldTotalParams   uint64 `json:"old_total_params"`
	NewTotalParams   uint64 `json:"new_total_params"`
	ShapesRoot       string `json:"shapes_root"`
	Reason           string `json:"reason,omitempty"`
}

// V11 allows a tied head's zero cost. The lookup field is a settlement floor,
// not matmul work: both 0 and 1 mean exactly one unit. Legacy validation is intact.
func validateComputeRangeRules(m *pb.ModelManifest, verified bool) error {
	return validateRangeRulesWithHead(m, verified)
}
func derivePublicationCompute(m *pb.ModelManifest, a Action, checkTable bool, appVersion uint64) (*VerifiedCompute, error) {
	bad := func(e any) (*VerifiedCompute, error) { return nil, fmt.Errorf("compute derivation mismatch: %v", e) }
	if a.DeclaredTotalParams == 0 || a.DeclaredActiveParams == 0 || a.DeclaredFlopsPerToken == 0 || len(a.TensorShapes) == 0 {
		return bad("shapes and all totals required")
	}
	hash := sha256.Sum256(m.ConfigBytes)
	if !bytes.Equal(hash[:], m.ConfigHash) {
		return bad("config bytes hash")
	}
	in := core.DerivationInput{AppVersion: appVersion, ConfigJSON: m.ConfigBytes, Tensors: a.TensorShapes}
	if checkTable {
		in.DeclaredLayerFlops = m.LayerFlops
	}
	r, err := core.DeriveCompute(in)
	if err != nil {
		return bad(err)
	}
	if r.TotalParams != a.DeclaredTotalParams || r.ActiveParams != a.DeclaredActiveParams || r.FlopsPerToken != a.DeclaredFlopsPerToken {
		return bad("declared totals")
	}
	if len(r.LayerFlops) == 0 || uint64(len(r.LayerFlops)) != uint64(m.TotalLayers) || r.FlopsPerToken == ^uint64(0) {
		return bad("missing/overflowing layer attribution")
	}
	for _, n := range r.LayerFlops {
		if n == 0 {
			return bad("zero layer compute")
		}
	}
	if checkTable {
		match := func(l []uint64, e, h uint64) bool {
			return reflect.DeepEqual(l, r.LayerFlops) && h == r.HeadFlops && e <= 1
		}
		if !match(m.LayerFlops, m.EmbeddingFlops, m.HeadFlops) {
			return bad("manifest table")
		}
		for _, s := range m.Schemes {
			if s.TotalLayers != m.TotalLayers || !match(s.LayerFlops, s.EmbeddingFlops, s.HeadFlops) {
				return bad("scheme table")
			}
		}
	}
	var size uint64
	for _, art := range m.Artifacts {
		if strings.HasSuffix(art.Name, ".safetensors") || strings.HasSuffix(art.Name, ".bin") {
			if art.Size <= 0 || uint64(art.Size) > ^uint64(0)-size {
				return bad("artifact sizes")
			}
			size += uint64(art.Size)
		}
	}
	if r.TensorBytes > size {
		return bad("tensor bytes exceed anchored weight sizes")
	}
	root, err := core.ShapesRoot(a.TensorShapes)
	if err != nil {
		return bad(err)
	}
	return &VerifiedCompute{TotalParams: r.TotalParams, ActiveParams: r.ActiveParams, FlopsPerToken: r.FlopsPerToken, LayerFlops: r.LayerFlops, HeadFlops: r.HeadFlops, ShapesRoot: root}, nil
}
func (s *State) verifiedComputeManifest(model Model, m *pb.ModelManifest) *pb.ModelManifest {
	if s.Version < 11 || model.VerifiedCompute == nil || len(model.VerifiedCompute.LayerFlops) == 0 {
		return m
	}
	n := proto.Clone(m).(*pb.ModelManifest)
	v := model.VerifiedCompute
	n.LayerFlops = append([]uint64(nil), v.LayerFlops...)
	n.TotalLayers = uint32(len(v.LayerFlops))
	n.HeadFlops = v.HeadFlops
	n.EmbeddingFlops = 0
	return n
}
func (s *State) correctModelCompute(signer string, a Action, height int64) error {
	if s.Version < 11 || !s.Governors[signer] {
		return fmt.Errorf("compute correction requires v11 governor")
	}
	m, ok := s.Models[a.ModelID]
	if !ok || m.Publication == nil {
		return fmt.Errorf("compute correction requires published model")
	}
	manifest := &pb.ModelManifest{}
	if err := proto.Unmarshal(m.Publication.Manifest, manifest); err != nil {
		return err
	}
	v, err := derivePublicationCompute(manifest, a, false, s.Version)
	if err != nil {
		return err
	}
	v.CorrectedHeight = height
	// Legacy manifests carry no T. Unknown old T is recorded as zero; old F is
	// the checked table sum, excluding the lookup settlement floor.
	oldT, oldF := uint64(0), uint64(0)
	if old := m.VerifiedCompute; old != nil {
		oldT, oldF = old.TotalParams, old.FlopsPerToken
	} else {
		oldF = manifest.HeadFlops
		for _, n := range manifest.LayerFlops {
			if n > ^uint64(0)-oldF {
				return fmt.Errorf("old compute overflow")
			}
			oldF += n
		}
	}
	m.ComputeCorrections = append(append([]ComputeCorrection(nil), m.ComputeCorrections...), ComputeCorrection{height, signer, oldF, v.FlopsPerToken, oldT, v.TotalParams, v.ShapesRoot, a.Reason})
	m.VerifiedCompute = v
	s.Models[m.ID] = m
	return nil
}
