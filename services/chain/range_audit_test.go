package chain

import (
	core "ember/core-go"
	pb "ember/proto"
	"encoding/binary"
	"google.golang.org/protobuf/proto"
	"math"
	"testing"
)

func rangeAuditFixture(t *testing.T) (*State, map[string]actor, *Hold) {
	s, a := liveFixture(t)
	s.Parameters.AuditEquality = AuditEqualityParameters{Enabled: true, Epsilon: 1, MaxRounds: 3}
	s.Version = 7
	producer := s.ActiveProfile(a["node"].id, s.Height)
	producer.ShardTotalLayers = 20
	producer.ShardEndLayer = 20
	producer.ShardHasEmbedding = true
	producer.ShardHasLmHead = true
	core.Sign(producer, a["node"].key)
	pr, _ := proto.Marshal(producer)
	m := &pb.ModelManifest{DynamicRanges: true, TotalLayers: 20}
	mr, _ := proto.Marshal(m)
	params := &pb.RangeBandParams{Metric: "relative-l2-v1", DepthCoef: .5, HardMultiplier: 3, Base: []*pb.RangeDeviceBase{{ProducerDevice: "cpu", ReferenceDevice: "cpu", Base: .02}}, HeadBase: []*pb.RangeDeviceBase{{ProducerDevice: "cpu", ReferenceDevice: "cpu", Base: .03}}}
	g := &pb.ChainGenesis{NumericAuditPolicy: &pb.NumericAuditPolicy{RangeBand: params}}
	gr, _ := proto.Marshal(g)
	s.Models["range"] = Model{Manifest: mr, Genesis: gr}
	h := &Hold{ID: "range", ModelID: "range", RequestID: RequestID("payer", 1, []byte("range")), FreezeHeight: 1, Node: a["node"].id, AuditOutputTokens: 1, Pipeline: &PipelineHold{TotalLayers: 20, Authorizations: []PipelineAuthorization{{NodeID: producer.NodeId, StartLayer: 0, EndLayer: 4, Profile: pr}, {NodeID: producer.NodeId, StartLayer: 4, EndLayer: 20, Profile: pr}}}}
	r := &pb.PipelineExecutionReceipt{StageReceipts: []*pb.ShardStepReceipt{{NodeId: producer.NodeId, StartLayer: 0, EndLayer: 4}, {NodeId: producer.NodeId, StartLayer: 4, EndLayer: 20}}}
	h.Pipeline.Receipt, _ = proto.Marshal(r)
	// Isolate one eligible registered auditor.
	for id := range s.Nodes {
		if id != a["auditor"].id && id != producer.NodeId {
			delete(s.Nodes, id)
		}
	}
	// Choose deterministic request entropy drawing the head-containing stage.
	for n := uint64(1); ; n++ {
		h.RequestID = RequestID("payer", n, []byte("range"))
		seed, _, err := s.assignmentSeed(h)
		if err != nil {
			t.Fatal(err)
		}
		if binary.BigEndian.Uint64(seed[:8])%2 == 1 {
			break
		}
	}
	return s, a, h
}
func TestRangeAssignmentFreezesBothRoutes(t *testing.T) {
	s, a, h := rangeAuditFixture(t)
	id, _, err := s.selectPipelineAuditor(h, "")
	if err != nil || id != a["auditor"].id {
		t.Fatal(id, err)
	}
	r := h.Assignments[0]
	d := r.DrawnStage
	if d == nil || d.Index != 1 || d.StartLayer != 4 || d.EndLayer != 20 || !d.HasLmHead {
		t.Fatal(d)
	}
	if winner, err := RecomputeAssignment(r); err != nil || winner != id {
		t.Fatal(winner, err)
	}
	if len(h.MisjudgeThresholds) != 2 || len(r.RangeThresholds) != 2 {
		t.Fatal(h.MisjudgeThresholds)
	}
	for i, want := range []float64{.12, .03} {
		p := h.MisjudgeThresholds[i]
		if math.Abs(p.Soft-want) > 1e-15 || math.Abs(p.Hard-3*want) > 1e-15 {
			t.Fatal(p)
		}
	}
	for _, route := range []string{"layer:4-20", "head:4-20"} {
		v := &pb.AuditVerdict{FaultNodeIds: []string{d.NodeID}, Offense: &pb.OffenseGrade{Class: "numerical", Band: core.RangeBand, LengthClass: route, Score: .5}}
		raw, _ := proto.Marshal(v)
		if _, err := s.validateGrade(h, raw); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []*pb.AuditVerdict{
		{FaultNodeIds: []string{d.NodeID}, Offense: &pb.OffenseGrade{Class: "numerical", Band: "pipeline-replay-v1", Score: .5}},
		{FaultNodeIds: []string{d.NodeID}, Offense: &pb.OffenseGrade{Class: "numerical", Band: core.RangeBand, LengthClass: "head:0-20", Score: .5}},
		{FaultNodeIds: []string{"other"}, Offense: &pb.OffenseGrade{Class: "numerical", Band: core.RangeBand, LengthClass: "head:4-20", Score: .5}},
	} {
		raw, _ := proto.Marshal(v)
		if _, err := s.validateGrade(h, raw); err == nil {
			t.Fatal("forged grade accepted")
		}
	}
}
func TestRangePoolFiltering(t *testing.T) {
	for _, kind := range []string{"short", "embedding", "head", "device", "role", "class", "full", "larger", "payer"} {
		t.Run(kind, func(t *testing.T) {
			s, a, h := rangeAuditFixture(t)
			p := s.ActiveProfile(a["auditor"].id, s.Height)
			p.ShardDynamicRange = true
			p.ShardTotalLayers = 20
			p.ShardStartLayer = 0
			p.ShardEndLayer = 20
			p.ShardHasEmbedding = true
			p.ShardHasLmHead = true
			switch kind {
			case "short":
				p.ShardEndLayer = 10
			case "embedding":
				// Draw the first stage so embedding ownership matters.
				for n := uint64(1); ; n++ {
					h.RequestID = RequestID("payer", n, []byte("emb"))
					seed, _, _ := s.assignmentSeed(h)
					if binary.BigEndian.Uint64(seed[:8])%2 == 0 {
						break
					}
				}
				p.ShardHasEmbedding = false
			case "head":
				p.ShardHasLmHead = false
			case "device":
				p.ActualDevice = "cuda"
			case "role":
				p.EngineCapabilities.Roles = []string{"model"}
			case "class":
				p.EngineCapabilities.EvidenceLevels = []string{"shard-layer"}
			case "full":
				p.ShardDynamicRange = false
				p.ShardTotalLayers = 0
			case "payer":
				h.Payer = p.NodeId
			}
			core.Sign(p, a["auditor"].key)
			raw, _ := proto.Marshal(p)
			s.Nodes[p.NodeId].Profile = raw
			_, _, err := s.selectPipelineAuditor(h, "")
			want := kind == "full" || kind == "larger"
			if (err == nil) != want {
				t.Fatalf("%s eligibility: %v", kind, err)
			}
			if !want && (len(h.Assignments) != 0 || len(h.MisjudgeThresholds) != 0) {
				t.Fatal("empty pool mutated hold")
			}
		})
	}
}
func TestRangeAtomicThresholdRoutes(t *testing.T) {
	s, _, h := rangeAuditFixture(t)
	_, p, err := s.drawPipelineStage(h)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []*DrawnStage{{StartLayer: 0, EndLayer: 0, HasEmbedding: true}, {StartLayer: 20, EndLayer: 20, HasLmHead: true}} {
		pairs, err := rangePairs(h, d, p, "cpu", "cpu")
		if err != nil || len(pairs) != 1 {
			t.Fatal(pairs, err)
		}
		want := .02
		if d.HasLmHead {
			want = .03
		}
		if pairs[0].Soft != want {
			t.Fatal(pairs)
		}
	}
}

func TestRangeAssignAuditTransitionAndFrozenFormula(t *testing.T) {
	s, a, h := rangeAuditFixture(t)
	h.State = "dispute"
	s.Holds[h.ID] = h
	apply(t, s, a["coordinator"], s.Accounts[a["coordinator"].id].Nonce+1, Action{Type: "assign_audit", HoldID: h.ID}, 4)
	h = s.Holds[h.ID]
	if h.Auditor != a["auditor"].id || len(h.Assignments) != 1 || len(h.MisjudgeThresholds) != 2 {
		t.Fatal("assignment transition did not freeze both routes", h)
	}
	d := h.Assignments[0].DrawnStage
	v := &pb.AuditVerdict{FaultNodeIds: []string{d.NodeID}, Offense: &pb.OffenseGrade{Class: "numerical", Band: core.RangeBand, LengthClass: "head:4-20", Score: .5}}
	raw, _ := proto.Marshal(v)
	h.MisjudgeThresholds[1].Hard *= 2
	if _, err := s.validateGrade(h, raw); err == nil {
		t.Fatal("wrong frozen threshold accepted")
	}
	record := h.Assignments[0]
	copy := *record.DrawnStage
	copy.Index = 0
	record.DrawnStage = &copy
	if _, err := RecomputeAssignment(record); err == nil {
		t.Fatal("forged drawn stage accepted")
	}
}

// Exercise the actual shard/full adapter identities used by CPU deployments.
func TestRangeAssignmentShardProducerFullReference(t *testing.T) {
	for _, bad := range []string{"", "torch", "transformers", "format", "engine"} {
		t.Run(bad, func(t *testing.T) {
			s, a, h := rangeAuditFixture(t)
			s.Parameters.AuditEquality.Enabled = false
			p := &pb.ResourceProfile{}
			proto.Unmarshal(h.Pipeline.Authorizations[0].Profile, p)
			p.EngineCapabilities.EngineName = "transformers-shard"
			p.EngineCapabilities.EvidenceLevels = []string{"shard-layer"}
			p.EngineCapabilities.ForcedPrefixReplay = false
			core.Sign(p, a["node"].key)
			raw, _ := proto.Marshal(p)
			for i := range h.Pipeline.Authorizations {
				h.Pipeline.Authorizations[i].Profile = raw
			}
			r := s.ActiveProfile(a["auditor"].id, s.Height)
			switch bad {
			case "torch":
				r.TorchVersion = "other"
			case "transformers":
				r.EngineCapabilities.EngineVersion = "other"
			case "format":
				r.EngineCapabilities.NumericalFormat = "other"
			case "engine":
				r.EngineCapabilities.EngineName = "other"
			}
			core.Sign(r, a["auditor"].key)
			raw, _ = proto.Marshal(r)
			s.Nodes[r.NodeId].Profile = raw
			id, _, err := s.selectPipelineAuditor(h, "")
			if bad == "" {
				if err != nil || id != a["auditor"].id {
					t.Fatal(id, err)
				}
			} else if err == nil {
				t.Fatal("uncalibrated runtime admitted")
			}
		})
	}
}

func TestRangeDrawExcludesUnusedStandbyAndIncludesFailover(t *testing.T) {
	for _, failover := range []bool{false, true} {
		s, a, h := rangeAuditFixture(t)
		p := &pb.ResourceProfile{}
		proto.Unmarshal(h.Pipeline.Authorizations[0].Profile, p)
		p.NodeId = a["ghost"].id
		core.Sign(p, a["ghost"].key)
		raw, _ := proto.Marshal(p)
		h.Pipeline.Authorizations = append(h.Pipeline.Authorizations, PipelineAuthorization{NodeID: p.NodeId, StartLayer: 0, EndLayer: 4, Profile: raw})
		if failover {
			r := &pb.PipelineExecutionReceipt{}
			proto.Unmarshal(h.Pipeline.Receipt, r)
			r.StageReceipts[0].NodeId = p.NodeId
			h.Pipeline.Receipt, _ = proto.Marshal(r)
		}
		seen := map[uint32]bool{}
		for n := uint64(1); n <= 32; n++ {
			h.RequestID = RequestID("payer", n, []byte("standby"))
			d, _, err := s.drawPipelineStage(h)
			if err != nil {
				t.Fatal(err)
			}
			if (!failover && d.Index == 2) || (failover && d.Index == 0) {
				t.Fatal("unexecuted holder drawn", d)
			}
			seen[d.Index] = true
		}
		if len(seen) != 2 {
			t.Fatal("did not cover both executed ranges", seen)
		}
		_, _, err := s.selectPipelineAuditor(h, "")
		if err != nil {
			t.Fatal(err)
		}
		r := h.Assignments[0]
		if r.DrawnStageCount != 2 || len(r.ExecutedStageIndices) != 2 {
			t.Fatal(r)
		}
		if _, err := RecomputeAssignment(r); err != nil {
			t.Fatal(err)
		}
		r.ExecutedStageIndices = []uint32{2, 2}
		if _, err := RecomputeAssignment(r); err == nil {
			t.Fatal("duplicate draw indices accepted")
		}
	}
}

// Missing device-pair calibration is an admission failure, never a CPU fallback.
func TestTrainRangeUncalibratedGPUPairsFailClosed(t *testing.T) {
	for _, device := range []string{"cuda", "cuda:0", "gpu", "mps"} {
		s, a, h := rangeAuditFixture(t)
		p := s.ActiveProfile(a["auditor"].id, s.Height)
		p.ActualDevice = device
		core.Sign(p, a["auditor"].key)
		raw, _ := proto.Marshal(p)
		s.Nodes[p.NodeId].Profile = raw
		if _, _, err := s.selectPipelineAuditor(h, ""); err == nil {
			t.Fatalf("uncalibrated %s admitted", device)
		}
		if len(h.Assignments) != 0 {
			t.Fatal("failed draw mutated assignment history")
		}
	}
}
