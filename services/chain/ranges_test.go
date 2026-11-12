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

func TestDynamicCapabilitiesConstrainExecutionsAndExpire(t *testing.T) {
	s, a := testState(t)
	s.Parameters.RegisteredAuditors = true
	nodes := []actor{newActor(40), newActor(41)}
	m := &pb.ModelManifest{ChainId: "ember-range-test", TotalLayers: 28, DynamicRanges: true, MinStageLayers: 2, AllowedLayerCuts: []uint32{0, 8, 10, 20, 28}}
	core.Sign(m, a["governor"].key)
	root, _ := core.Digest(m)
	g := &pb.ChainGenesis{ChainId: m.ChainId, ModelManifestRoot: root, ExecutionProfileVer: "range-test"}
	core.Sign(g, a["governor"].key)
	gr, _ := proto.Marshal(g)
	mr, _ := proto.Marshal(m)
	gh, mh := sha256.Sum256(gr), sha256.Sum256(mr)
	s.Models[m.ChainId] = Model{ID: m.ChainId, GenesisHash: hex.EncodeToString(gh[:]), ProfileHash: hex.EncodeToString(mh[:]), Genesis: gr, Manifest: mr}
	profiles := []*pb.ResourceProfile{}
	for i, bounds := range [][2]uint32{{0, 20}, {8, 28}} {
		node := nodes[i]
		s.Accounts[node.id] = &Account{Stake: 5000}
		p := &pb.ResourceProfile{NodeId: node.id, ChainId: m.ChainId, ManifestRoot: root, ExecutionProfileVer: g.ExecutionProfileVer, ShardTotalLayers: 28, ShardStartLayer: bounds[0], ShardEndLayer: bounds[1], ShardHasEmbedding: bounds[0] == 0, ShardHasLmHead: bounds[1] == 28, ShardDynamicRange: true, ShardUnitKind: "range", ShardParametersHash: core.RangeParameters(root, bounds[0], bounds[1]), EngineCapabilities: &pb.EngineCapabilities{Roles: []string{"shard-stage"}}}
		core.Sign(p, node.key)
		raw, _ := proto.Marshal(p)
		apply(t, s, node, 1, Action{Type: "register_node", Profile: raw, ExpiresHeight: 90}, 1)
		profiles = append(profiles, p)
	}
	freeze := Action{Type: "freeze_pipeline", HoldID: "ranges", Payer: a["payer"].id, Amount: 28, TotalLayers: 28, PipelineAuthorizations: []PipelineAuthorization{{NodeID: nodes[0].id, StartLayer: 0, EndLayer: 21}, {NodeID: nodes[1].id, StartLayer: 21, EndLayer: 28}}}
	rejected(t, s, a["payer"], freeze, 2) // exceeds first node's capability
	freeze.PipelineAuthorizations[0].EndLayer = 9
	freeze.PipelineAuthorizations[1].StartLayer = 9
	rejected(t, s, a["payer"], freeze, 2) // violates signed cut/granularity rules
	freeze.PipelineAuthorizations[0].EndLayer = 10
	freeze.PipelineAuthorizations[1].StartLayer = 10
	apply(t, s, a["payer"], 1, freeze, 2)
	r := &pb.PipelineExecutionReceipt{RequestId: "ranges", InputTokens: 1, OutputTokens: 1, ElapsedMs: 1, OutputCommitment: bytes.Repeat([]byte{1}, 32)}
	for i, bounds := range [][2]uint32{{0, 10}, {10, 28}} {
		p := profiles[i]
		stage := &pb.ShardStepReceipt{NodeId: p.NodeId, ChainId: p.ChainId, ManifestRoot: root, ExecutionProfileVer: p.ExecutionProfileVer, RequestId: r.RequestId, StartLayer: bounds[0], EndLayer: bounds[1], UnitKind: "range", ParametersHash: p.ShardParametersHash, RequestHash: bytes.Repeat([]byte{2}, 32), ResponseHash: bytes.Repeat([]byte{3}, 32)}
		if i == 0 {
			stage.OutputActivationHash = bytes.Repeat([]byte{4}, 32)
		} else {
			stage.InputActivationHash = bytes.Repeat([]byte{4}, 32)
		}
		core.Sign(stage, nodes[i].key)
		r.StageReceipts = append(r.StageReceipts, stage)
	}
	raw, _ := proto.Marshal(r)
	apply(t, s, a["coordinator"], 1, Action{Type: "submit_pipeline", HoldID: "ranges", Actual: 28, Receipt: raw}, 3)
	credits := s.Holds["ranges"].Pipeline.Credits
	byNode := map[string]int64{}
	for _, c := range credits {
		byNode[c.NodeID] = c.Amount
	}
	if byNode[nodes[0].id] != 10 || byNode[nodes[1].id] != 18 {
		t.Fatal("credited advertised rather than executed layers", credits)
	}
	// An active capability cannot remove covered layers before its lease expires.
	p := profiles[0]
	p.ShardEndLayer = 10
	p.ShardParametersHash = core.RangeParameters(root, 0, 10)
	core.Sign(p, nodes[0].key)
	raw, _ = proto.Marshal(p)
	rejected(t, s, nodes[0], Action{Type: "register_node", Profile: raw, ExpiresHeight: 91}, 4)
	p.ShardEndLayer = 28
	p.ShardHasLmHead = true
	p.ShardParametersHash = core.RangeParameters(root, 0, 28)
	core.Sign(p, nodes[0].key)
	raw, _ = proto.Marshal(p)
	rejected(t, s, nodes[0], Action{Type: "register_node", Profile: raw, ExpiresHeight: 91}, 4)
	freeze.HoldID = "expired"
	rejected(t, s, a["payer"], freeze, 90)
}
