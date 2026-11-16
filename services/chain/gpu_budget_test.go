package chain

import (
	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
	"strings"
	"testing"
)

func TestGPUBudgetRegistration(t *testing.T) {
	s, a := testState(t)
	act := weightedPublication(t, a["payer"], "gpu-budget")
	apply(t, s, a["payer"], 1, act, 1)
	g := &pb.ChainGenesis{}
	proto.Unmarshal(act.Genesis, g)
	p := &pb.ResourceProfile{NodeId: a["node"].id, ChainId: act.ModelID, ManifestRoot: g.ModelManifestRoot, ExecutionProfileVer: g.ExecutionProfileVer, GpuBudget: &pb.GPUBudget{MemoryPermille: 100, ComputePermille: 1000, AcceleratorMemoryBytes: 500}}
	if err := s.validateGPUBudget(p); err == nil || !strings.Contains(err.Error(), "cannot fit") {
		t.Fatal(err)
	}
	core.Sign(p, a["node"].key)
	raw, _ := proto.Marshal(p)
	rejected(t, s, a["node"], Action{Type: "register_node", Profile: raw, ExpiresHeight: 90}, 2)
	p.GpuBudget.MemoryPermille = 1000
	core.Sign(p, a["node"].key)
	raw, _ = proto.Marshal(p)
	apply(t, s, a["node"], 1, Action{Type: "register_node", Profile: raw, ExpiresHeight: 90}, 2)
	p.GpuBudget = nil
	if err := s.validateGPUBudget(p); err != nil {
		t.Fatal("legacy", err)
	}
}
