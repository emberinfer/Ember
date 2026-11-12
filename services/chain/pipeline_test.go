package chain

import (
	"bytes"
	"crypto/sha256"
	core "ember/core-go"
	pb "ember/proto"
	"encoding/hex"
	"google.golang.org/protobuf/proto"
	"testing"
)

func pipelineState(t *testing.T) (*State, map[string]actor, []actor, []PipelineAuthorization, *pb.PipelineExecutionReceipt) {
	s, a := testState(t)
	total := uint32(4)
	nodes := []actor{newActor(20), newActor(21), newActor(22)}
	specs := []*pb.ShardSpec{{Index: 0, StartLayer: 0, EndLayer: 0, HasEmbedding: true, UnitKind: "embedding"}, {Index: 1, StartLayer: 0, EndLayer: 4, UnitKind: "layers"}, {Index: 2, StartLayer: 4, EndLayer: 4, HasLmHead: true, UnitKind: "lm_head"}}
	m := &pb.ModelManifest{ChainId: "ember-unit-test", TotalLayers: total, Shards: specs}
	for _, spec := range specs {
		spec.ParametersHash = bytes.Repeat([]byte{byte(spec.Index + 1)}, 32)
	}
	core.Sign(m, a["governor"].key)
	root, _ := core.Digest(m)
	g := &pb.ChainGenesis{ChainId: m.ChainId, ModelManifestRoot: root, ExecutionProfileVer: "units-test"}
	core.Sign(g, a["governor"].key)
	gr, _ := proto.Marshal(g)
	mr, _ := proto.Marshal(m)
	gh := sha256.Sum256(gr)
	mh := sha256.Sum256(mr)
	model := Model{ID: m.ChainId, GenesisHash: hex.EncodeToString(gh[:]), ProfileHash: hex.EncodeToString(mh[:]), Genesis: gr, Manifest: mr}
	if err := validateModelArtifacts(model); err != nil {
		t.Fatal(err)
	}
	s.Models[m.ChainId] = model
	auths := []PipelineAuthorization{}
	receipt := &pb.PipelineExecutionReceipt{RequestId: "units", InputTokens: 2, OutputTokens: 1, ElapsedMs: 10, OutputCommitment: bytes.Repeat([]byte{8}, 32)}
	for i, node := range nodes {
		s.Accounts[node.id] = &Account{Stake: 5000}
		spec := specs[i]
		p := &pb.ResourceProfile{NodeId: node.id, ChainId: m.ChainId, ManifestRoot: root, ExecutionProfileVer: g.ExecutionProfileVer, ShardIndex: uint32(i), ShardStartLayer: spec.StartLayer, ShardEndLayer: spec.EndLayer, ShardTotalLayers: total, ShardHasEmbedding: spec.HasEmbedding, ShardHasLmHead: spec.HasLmHead, ShardParametersHash: spec.ParametersHash, ShardUnitKind: spec.UnitKind, EngineCapabilities: &pb.EngineCapabilities{Roles: []string{"shard-stage"}}}
		core.Sign(p, node.key)
		raw, _ := proto.Marshal(p)
		apply(t, s, node, 1, Action{Type: "register_node", Profile: raw, ExpiresHeight: 90}, 1)
		auths = append(auths, PipelineAuthorization{NodeID: node.id, StartLayer: spec.StartLayer, EndLayer: spec.EndLayer})
		r := &pb.ShardStepReceipt{NodeId: node.id, ChainId: m.ChainId, ManifestRoot: root, ExecutionProfileVer: g.ExecutionProfileVer, RequestId: receipt.RequestId, ShardIndex: uint32(i), StartLayer: spec.StartLayer, EndLayer: spec.EndLayer, ParametersHash: spec.ParametersHash, UnitKind: spec.UnitKind, RequestHash: bytes.Repeat([]byte{1}, 32), ResponseHash: bytes.Repeat([]byte{2}, 32)}
		if i > 0 {
			r.InputActivationHash = bytes.Repeat([]byte{byte(i + 2)}, 32)
		}
		if i < 2 {
			r.OutputActivationHash = bytes.Repeat([]byte{byte(i + 3)}, 32)
		}
		core.Sign(r, node.key)
		receipt.StageReceipts = append(receipt.StageReceipts, r)
	}
	return s, a, nodes, auths, receipt
}

func TestPipelineConsensusSettlementByUnits(t *testing.T) {
	s, a, nodes, auth, r := pipelineState(t)
	apply(t, s, a["payer"], 1, Action{Type: "freeze_pipeline", HoldID: "units", Payer: a["payer"].id, Amount: 6, TotalLayers: 4, PipelineAuthorizations: auth}, 2)
	raw, _ := proto.Marshal(r)
	apply(t, s, a["coordinator"], 1, Action{Type: "submit_pipeline", HoldID: "units", Actual: 6, Receipt: raw}, 3)
	apply(t, s, a["coordinator"], 2, Action{Type: "assign_audit", HoldID: "units"}, 4)
	h := s.Holds["units"]
	auditor := a["auditor"]
	if auditor.id != h.Auditor {
		auditor = a["auditor2"]
	}
	ref, _ := hex.DecodeString(h.ReceiptRef)
	proof, _ := hex.DecodeString(h.AuditProof)
	c := &pb.AuditChallenge{ReceiptRef: ref, VrfProof: proof}
	core.Sign(c, auditor.key)
	cr, _ := core.Digest(c)
	v := &pb.AuditVerdict{ChallengeRef: cr, AuditorId: auditor.id, Verdict: "accept", EvidenceRef: bytes.Repeat([]byte{7}, 32)}
	core.Sign(v, auditor.key)
	cb, _ := proto.Marshal(c)
	vb, _ := proto.Marshal(v)
	apply(t, s, auditor, 1, Action{Type: "audit", HoldID: "units", Challenge: cb, Verdict: vb}, 5)
	apply(t, s, a["coordinator"], 3, Action{Type: "settle", HoldID: "units", Outcome: "accept"}, 6)
	if s.Holds["units"].State != "settled" || s.Accounts[a["payer"].id].Balance != 994 {
		t.Fatal("pipeline not settled")
	}
	for i, want := range []int64{1, 4, 1} {
		if s.Accounts[nodes[i].id].Balance != want || s.exposure(nodes[i].id) != 0 {
			t.Fatal("wrong unit credit or exposure")
		}
	}
}

func TestPipelineConsensusRejectsBoundaryForgeryAndCoverageGap(t *testing.T) {
	s, a, nodes, auth, r := pipelineState(t)
	apply(t, s, a["payer"], 1, Action{Type: "freeze_pipeline", HoldID: "units", Payer: a["payer"].id, Amount: 6, TotalLayers: 4, PipelineAuthorizations: auth}, 2)
	r.StageReceipts[1].InputActivationHash = bytes.Repeat([]byte{99}, 32)
	core.Sign(r.StageReceipts[1], nodes[1].key)
	raw, _ := proto.Marshal(r)
	rejected(t, s, a["coordinator"], Action{Type: "submit_pipeline", HoldID: "units", Actual: 6, Receipt: raw}, 3)
	r.StageReceipts = r.StageReceipts[:2]
	raw, _ = proto.Marshal(r)
	rejected(t, s, a["coordinator"], Action{Type: "submit_pipeline", HoldID: "units", Actual: 6, Receipt: raw}, 3)
}
