package chain

import (
	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestRegisteredAuditSelection(t *testing.T) {
	s, a := testState(t)
	s.Parameters.RegisteredAuditors = true
	producer := s.ActiveProfile(a["node"].id, 1)
	producer.ActualDevice = "cpu"
	producer.TorchVersion = "2.5.1+cpu"
	producer.EngineCapabilities = &pb.EngineCapabilities{EngineName: "transformers", EngineVersion: "4.45.2", Roles: []string{"model", "auditor"}, ForcedPrefixReplay: true, ScoreKind: "raw-logits", MaxTopK: 16, EvidenceLevels: []string{"full"}}
	register := func(name string, p *pb.ResourceProfile) {
		p.NodeId = a[name].id
		if err := core.Sign(p, a[name].key); err != nil {
			t.Fatal(err)
		}
		raw, _ := proto.Marshal(p)
		s.Nodes[p.NodeId] = &NodeRegistration{NodeID: p.NodeId, Profile: raw, ExpiresHeight: 100}
		s.Accounts[p.NodeId].Stake = 500
	}
	register("node", producer)
	hold := &Hold{RequestID: RequestID("payer", 1, []byte("commitment")), ID: "new-operator", Node: a["node"].id, ReceiptRef: "receipt"}
	if _, _, err := s.selectAuditor(hold, ""); err == nil {
		t.Fatal("accepted unregistered or self auditor")
	}
	ref := proto.Clone(producer).(*pb.ResourceProfile)
	register("auditor", ref)
	selected, _, err := s.selectAuditor(hold, "")
	if err != nil || selected != a["auditor"].id {
		t.Fatalf("fresh registration failed: %s %v", selected, err)
	}
	s.Accounts[selected].Stake = 0
	if _, _, err := s.selectAuditor(hold, ""); err == nil {
		t.Fatal("accepted unbacked reference")
	}
	s.Accounts[selected].Stake = 500
	s.Nodes[selected].ExpiresHeight = s.Height
	if _, _, err := s.selectAuditor(hold, ""); err == nil {
		t.Fatal("accepted expired reference")
	}
	s.Nodes[selected].ExpiresHeight = 100
	ref.TorchVersion = "unmeasured"
	register("auditor", ref)
	if _, _, err := s.selectAuditor(hold, ""); err == nil {
		t.Fatal("accepted incompatible runtime")
	}
}
