package chain

import (
	"bytes"
	core "ember/core-go"
	pb "ember/proto"
	"encoding/binary"
	"errors"
	"fmt"
	"google.golang.org/protobuf/proto"
	"math"
)

// Range replay uses the same decoder runtime through either the full-model
// adapter or the shard adapter. Their adapter names intentionally differ.
func (s *State) EligibleRangeAuditor(id, producer string, p *pb.ResourceProfile, height int64) bool {
	r := s.ActiveProfile(id, height)
	if p == nil || r == nil || id == producer || !s.ReviewAllowed(id, height) || p.EngineCapabilities == nil || r.EngineCapabilities == nil {
		return false
	}
	pc, rc := p.EngineCapabilities, r.EngineCapabilities
	transformers := func(name string) bool { return name == "transformers" || name == "transformers-shard" }
	return bytes.Equal(r.SchemeHash, p.SchemeHash) && r.ChainId == p.ChainId && r.ExecutionProfileVer == p.ExecutionProfileVer && bytes.Equal(r.ManifestRoot, p.ManifestRoot) &&
		transformers(pc.EngineName) && transformers(rc.EngineName) && pc.EngineVersion == rc.EngineVersion &&
		p.TorchVersion != "" && p.TorchVersion == r.TorchVersion && p.TransformersVersion == r.TransformersVersion &&
		pc.NumericalFormat == rc.NumericalFormat && rc.MaxTopK >= 16 && rc.ScoreKind == "raw-logits" &&
		((rc.ForcedPrefixReplay && contains(rc.EvidenceLevels, "full")) || (rc.EngineName == "transformers-shard" && r.ShardDynamicRange && contains(rc.EvidenceLevels, "shard-layer")))
}

// Stage index is into the immutable freeze authorizations, not receipt ordering.
type DrawnStage struct {
	Index        uint32 `json:"index"`
	StartLayer   uint32 `json:"start_layer"`
	EndLayer     uint32 `json:"end_layer"`
	HasEmbedding bool   `json:"has_embedding"`
	HasLmHead    bool   `json:"has_lm_head"`
	NodeID       string `json:"node_id"`
}

func (s *State) dynamicPipeline(h *Hold) bool {
	if h.Pipeline == nil {
		return false
	}
	_, raw := modelArtifacts(s.Models[h.ModelID])
	m := &pb.ModelManifest{}
	return proto.Unmarshal(raw, m) == nil && (m.DynamicRanges || len(m.Schemes) > 0)
}
func (s *State) drawPipelineStage(h *Hold) (*DrawnStage, *pb.RangeBandParams, error) {
	if !s.dynamicPipeline(h) {
		return nil, nil, nil
	}
	seed, _, err := s.assignmentSeed(h)
	if err != nil {
		return nil, nil, err
	}
	auths := h.Pipeline.Authorizations
	if len(auths) == 0 {
		return nil, nil, errors.New("no frozen stages")
	}
	indices, err := executedRangeIndices(h)
	if err != nil {
		return nil, nil, err
	}
	index := indices[binary.BigEndian.Uint64(seed[:8])%uint64(len(indices))]
	a := auths[index]
	p := &pb.ResourceProfile{}
	if proto.Unmarshal(a.Profile, p) != nil || p.NodeId != a.NodeID {
		return nil, nil, errors.New("invalid frozen range producer")
	}
	raw, _ := modelArtifacts(s.Models[h.ModelID])
	g := &pb.ChainGenesis{}
	if proto.Unmarshal(raw, g) != nil || g.NumericAuditPolicy == nil {
		return nil, nil, errors.New("missing range policy")
	}
	params := g.NumericAuditPolicy.RangeBand
	if err := core.ValidateRangeBand(params); err != nil {
		return nil, nil, err
	}
	return &DrawnStage{uint32(index), a.StartLayer, a.EndLayer, a.StartLayer == 0 && p.ShardHasEmbedding, a.EndLayer == h.Pipeline.TotalLayers && p.ShardHasLmHead, a.NodeID}, params, nil
}

// Only holders with verified invocations in the submitted receipt enter the
// draw. Merely authorizing a standby does not make it an executed stage.
func executedRangeIndices(h *Hold) ([]uint32, error) {
	// Retain the draw semantics of assignments finalized before this field.
	if len(h.Assignments) > 0 && len(h.Assignments[0].ExecutedStageIndices) == 0 {
		indices := make([]uint32, len(h.Pipeline.Authorizations))
		for i := range indices {
			indices[i] = uint32(i)
		}
		return indices, nil
	}
	r := &pb.PipelineExecutionReceipt{}
	if proto.Unmarshal(h.Pipeline.Receipt, r) != nil || len(r.StageReceipts) == 0 {
		return nil, errors.New("missing executed range receipt")
	}
	seen := map[uint32]bool{}
	for _, receipt := range r.StageReceipts {
		found := false
		for i, a := range h.Pipeline.Authorizations {
			if receipt.NodeId == a.NodeID && receipt.StartLayer == a.StartLayer && receipt.EndLayer == a.EndLayer {
				seen[uint32(i)] = true
				found = true
				break
			}
		}
		if !found {
			return nil, errors.New("executed range lacks frozen authorization")
		}
	}
	indices := []uint32{}
	for i := range h.Pipeline.Authorizations {
		if seen[uint32(i)] {
			indices = append(indices, uint32(i))
		}
	}
	return indices, nil
}
func coversRange(p *pb.ResourceProfile, d *DrawnStage, total uint32) bool {
	if p == nil {
		return false
	}
	// Legacy full-model registrations carry no shard bounds. Explicit range/full
	// registrations must still prove both requested boundary atoms.
	if !p.ShardDynamicRange && p.ShardTotalLayers == 0 {
		return true
	}
	return p.ShardTotalLayers == total && p.ShardStartLayer <= d.StartLayer && d.EndLayer <= p.ShardEndLayer && (!d.HasEmbedding || p.ShardHasEmbedding) && (!d.HasLmHead || p.ShardHasLmHead)
}
func rangeLength(d *DrawnStage, head bool) string {
	route := "layer"
	if head {
		route = "head"
	}
	return fmt.Sprintf("%s:%d-%d", route, d.StartLayer, d.EndLayer)
}
func rangePairs(h *Hold, d *DrawnStage, p *pb.RangeBandParams, producer, reference string) ([]ThresholdPair, error) {
	result := []ThresholdPair{}
	for _, head := range []bool{false, true} {
		// A pure head has no layer route; embedding is a one-unit layer route.
		if head && !d.HasLmHead || !head && d.StartLayer == d.EndLayer && !d.HasEmbedding {
			continue
		}
		soft, hard, err := core.RangeThresholds(p, d.StartLayer, d.EndLayer, h.Pipeline.TotalLayers, producer, reference, head)
		if err != nil {
			return nil, err
		}
		result = append(result, ThresholdPair{Model: h.ModelID, Band: core.RangeBand, LengthClass: rangeLength(d, head), MinOutput: 0, MaxOutput: math.MaxUint32, Soft: soft, Hard: hard})
	}
	return result, nil
}
func (s *State) selectRangeAuditor(h *Hold, exclude string, at ...int64) (string, []byte, error) {
	d, params, err := s.drawPipelineStage(h)
	if err != nil {
		return "", nil, err
	}
	source := s.finalizedState()
	producer := &pb.ResourceProfile{}
	if proto.Unmarshal(h.Pipeline.Authorizations[d.Index].Profile, producer) != nil {
		return "", nil, errors.New("invalid frozen producer")
	}
	excluded := map[string]bool{exclude: true, h.Payer: true}
	for _, a := range h.Pipeline.Authorizations {
		excluded[a.NodeID] = true
	}
	ids := []string{}
	for id := range source.Nodes {
		p := source.ActiveProfile(id, source.Height)
		if excluded[id] || !coversRange(p, d, h.Pipeline.TotalLayers) || !source.EligibleRangeAuditor(id, d.NodeID, producer, source.Height) {
			continue
		}
		if _, err := rangePairs(h, d, params, producer.ActualDevice, p.ActualDevice); err == nil {
			ids = append(ids, id)
		}
	}
	auditor, seed, err := s.drawAuditor(h, ids, at...)
	if err != nil {
		return "", nil, err
	}
	pairs, err := rangePairs(h, d, params, producer.ActualDevice, source.ActiveProfile(auditor, source.Height).ActualDevice)
	if err != nil {
		return "", nil, err
	}
	// Every assignment retains its own immutable device thresholds; the hold
	// exposes the current assignment for thresholdFor and worker consumption.
	kept := h.MisjudgeThresholds[:0]
	for _, p := range h.MisjudgeThresholds {
		if p.Band != core.RangeBand {
			kept = append(kept, p)
		}
	}
	h.MisjudgeThresholds = append(kept, pairs...)
	r := &h.Assignments[len(h.Assignments)-1]
	r.RangeThresholds = append([]ThresholdPair(nil), pairs...)
	r.RangeProducerDevice = producer.ActualDevice
	r.RangeReferenceDevice = source.ActiveProfile(auditor, source.Height).ActualDevice
	return auditor, seed, nil
}
func (s *State) validateRangeGrade(h *Hold, v *pb.AuditVerdict) error {
	if len(h.Assignments) == 0 {
		return errors.New("missing range assignment")
	}
	d := h.Assignments[len(h.Assignments)-1].DrawnStage
	r := h.Assignments[len(h.Assignments)-1]
	drawn, params, err := s.drawPipelineStage(h)
	if err != nil {
		return err
	}
	if d == nil || drawn == nil || *d != *drawn {
		return errors.New("range assignment differs from frozen draw")
	}
	pairs, err := rangePairs(h, d, params, r.RangeProducerDevice, r.RangeReferenceDevice)
	if err != nil {
		return err
	}
	frozen, err := thresholdFor(h, v.Offense)
	if err != nil {
		return err
	}
	matched := false
	for _, p := range pairs {
		if p.LengthClass == v.Offense.LengthClass && p.Soft == frozen.Soft && p.Hard == frozen.Hard {
			matched = true
		}
	}
	if !matched {
		return errors.New("range threshold differs from signed formula")
	}
	if d == nil || v.Offense.LengthClass != rangeLength(d, false) && v.Offense.LengthClass != rangeLength(d, true) {
		return errors.New("range grade differs from drawn stage")
	}
	if len(v.FaultNodeIds) != 1 || v.FaultNodeIds[0] != d.NodeID {
		return errors.New("range fault differs from drawn producer")
	}
	return nil
}
