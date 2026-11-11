package chain

import (
	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
	"math"
	"testing"
)

func pricingState(t *testing.T) (*State, map[string]actor) {
	s, a := testState(t)
	s.Parameters.RegisteredAuditors = true
	model := s.Models["ember-test-model"]
	model.Pricing = &RolePricing{TargetExecutors: 2, TargetAuditors: 3, ExecutorWeight: 80, AuditorWeight: 15, ConsumerWeight: 5}
	s.Models[model.ID] = model
	base := s.ActiveProfile(a["node"].id, 0)
	base.ActualDevice = "cpu"
	base.TorchVersion = "2.5.1"
	base.EngineCapabilities = &pb.EngineCapabilities{EngineName: "transformers", EngineVersion: "4.45.2", Roles: []string{"model", "auditor"}, ForcedPrefixReplay: true, ScoreKind: "raw-logits", MaxTopK: 16, EvidenceLevels: []string{"full"}}
	for _, name := range []string{"node", "auditor", "auditor2", "governor"} {
		p := proto.Clone(base).(*pb.ResourceProfile)
		p.NodeId = a[name].id
		if err := core.Sign(p, a[name].key); err != nil {
			t.Fatal(err)
		}
		raw, _ := proto.Marshal(p)
		s.Nodes[p.NodeId] = &NodeRegistration{NodeID: p.NodeId, Profile: raw, ExpiresHeight: 100}
		s.Accounts[p.NodeId].Stake = 500
	}
	s.Height = 9
	return s, a
}
func pricingHold(s *State, a map[string]actor) *Hold {
	return &Hold{ID: "priced", Payer: a["payer"].id, Node: a["node"].id, Auditor: a["auditor"].id, ModelID: "ember-test-model", ProducerProfile: s.Nodes[a["node"].id].Profile, Amount: 12000, Actual: 10000, State: "dispute"}
}
func TestScarcityRedistributesWithoutIssuance(t *testing.T) {
	s, a := pricingState(t)
	base := pricingHold(s, a)
	policy := s.Models[base.ModelID].Pricing
	b, err := s.priceShares(base, policy)
	if err != nil {
		t.Fatal(err)
	}
	if b.Executor != 8000 || b.Auditor != 1500 || b.Consumer != 500 || b.Coverage.Auditors != 3 {
		t.Fatalf("base %+v", b)
	}
	s.Nodes[a["auditor2"].id].ExpiresHeight = 10
	s.Nodes[a["governor"].id].ExpiresHeight = 10
	s.Height = 10
	scarce, err := s.priceShares(base, policy)
	if err != nil {
		t.Fatal(err)
	}
	if scarce.Auditor <= b.Auditor || scarce.Executor >= b.Executor || scarce.Consumer >= b.Consumer || scarce.Coverage.Auditors != 1 || scarce.Coverage.AuditorMultiplier > 2*ScarcityScale {
		t.Fatalf("scarce %+v", scarce)
	}
	var before int64
	for _, v := range s.Accounts {
		before += v.Balance
	}
	issued := s.SubsidyIssued
	if err := s.settle(base, "accept"); err != nil {
		t.Fatal(err)
	}
	var after int64
	for _, v := range s.Accounts {
		after += v.Balance
	}
	if after-before != base.Amount || s.SubsidyIssued != issued || base.Shares.Executor+base.Shares.Auditor+base.Shares.Consumer != base.Actual {
		t.Fatal("fee conservation or issuance failure")
	}
	s.Nodes[a["auditor2"].id].ExpiresHeight = 100
	s.Nodes[a["governor"].id].ExpiresHeight = 100
	restored, err := s.priceShares(base, policy)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Executor != b.Executor || restored.Auditor != b.Auditor || restored.Consumer != b.Consumer {
		t.Fatal("restoring coverage did not restore base")
	}
}
func TestCoverageUsesPriorFinalizedSnapshot(t *testing.T) {
	s, a := pricingState(t)
	prior, err := s.Clone()
	if err != nil {
		t.Fatal(err)
	}
	s.finalized = prior
	s.Height = 10
	delete(s.Nodes, a["auditor2"].id)
	delete(s.Nodes, a["governor"].id)
	h := pricingHold(s, a)
	shares, err := s.priceShares(h, s.Models[h.ModelID].Pricing)
	if err != nil {
		t.Fatal(err)
	}
	if shares.Coverage.Height != 9 || shares.Coverage.Auditors != 3 || shares.Auditor != 1500 {
		t.Fatal("in-block changes influenced finalized signal")
	}
	cloned, err := s.Clone()
	if err != nil || cloned.finalized != prior {
		t.Fatal("snapshot lost while applying block")
	}
	raw, _ := s.Bytes()
	s.finalized = nil
	rawWithout, _ := s.Bytes()
	if string(raw) != string(rawWithout) {
		t.Fatal("transient snapshot was serialized")
	}
}
func TestCoverageRejectsUnbackedAndDuplicateIdentities(t *testing.T) {
	s, a := pricingState(t)
	h := pricingHold(s, a)
	policy := s.Models[h.ModelID].Pricing
	s.Accounts[a["auditor2"].id].Stake = 0
	copied := *s.Nodes[a["governor"].id]
	s.Nodes["fake-key"] = &copied
	s.Accounts["fake-key"] = &Account{Stake: 500}
	c := s.coverage(h.Node, s.ActiveProfile(h.Node, s.Height), policy)
	if c.Auditors != 2 || c.Executors != 3 {
		t.Fatalf("counted unbacked or duplicate signer: %+v", c)
	}
}
func TestActiveCoverageCannotWithdrawUntilTTL(t *testing.T) {
	s, a := pricingState(t)
	who := a["auditor"]
	profile := s.ActiveProfile(who.id, s.Height)
	profile.EngineCapabilities.Roles = []string{"model"}
	if err := core.Sign(profile, who.key); err != nil {
		t.Fatal(err)
	}
	raw, _ := proto.Marshal(profile)
	for _, action := range []Action{{Type: "unbond", Amount: 500}, {Type: "register_node", Profile: raw, ExpiresHeight: 100}, {Type: "register_node", Profile: s.Nodes[who.id].Profile, ExpiresHeight: 99}} {
		signed, _ := Sign(s.Parameters.ChainID, who.key, 1, action)
		if _, err := s.Apply(signed, 10); err == nil {
			t.Fatalf("active coverage withdrawal allowed: %s", action.Type)
		}
	}
	apply(t, s, who, 1, Action{Type: "unbond", Amount: 500}, 100)
	if s.Accounts[who.id].Stake != 0 || s.Accounts[who.id].Unbondings[0].ReleaseHeight != 103 {
		t.Fatal("TTL-delayed unbond failed")
	}
}
func TestFeeRoundingAndOverflowBoundaries(t *testing.T) {
	for _, fee := range []int64{0, 1, 2, 3, 101, math.MaxInt64} {
		for _, weights := range [][3]int64{{80e6, 15e6, 5e6}, {80e6, 30e6, 5e6}, {160e6, 30e6, 5e6}} {
			parts := allocateFee(fee, weights)
			if parts[0] < 0 || parts[1] < 0 || parts[2] < 0 || parts[0]+parts[1]+parts[2] != fee {
				t.Fatal("rounding lost fee", fee, parts)
			}
			if fee == 1 && parts[0] != 1 {
				t.Fatal("one-subunit executor must be able to earn")
			}
		}
	}
	if multiplier(3, 0) != 2*ScarcityScale || multiplier(3, 4) != ScarcityScale {
		t.Fatal("multiplier bounds")
	}
}
