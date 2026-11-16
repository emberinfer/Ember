package chain

import (
	core "ember/core-go"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestTrainPricingGovernanceAndGenesisSeed(t *testing.T) {
	s, a := testState(t)
	if !s.Governors[a["governor"].id] || s.Governors[a["payer"].id] {
		t.Fatal("genesis governor seed not loaded")
	}
	p := &RolePricing{2, 3, 80, 15, 5}
	action := Action{Type: "set_pricing", ModelID: "ember-test-model", Pricing: p}
	rejected(t, s, a["payer"], action, 1)
	bad := action
	bad.ModelID = "missing"
	rejected(t, s, a["governor"], bad, 1)
	bad = action
	bad.Pricing = &RolePricing{}
	rejected(t, s, a["governor"], bad, 1)
	apply(t, s, a["governor"], 1, action, 1)
	action.Pricing = &RolePricing{1, 1, 80, 15, 5}
	apply(t, s, a["governor"], 2, action, 2)
	clone, err := s.Clone()
	if err != nil || clone.Models[action.ModelID].Pricing.TargetExecutors != 1 {
		t.Fatal("pricing not persisted", err)
	}
}

func TestTrainPipelinePricingExactShares(t *testing.T) {
	s, a := pricingState(t)
	h := pricingHold(s, a)
	h.Pipeline = &PipelineHold{Credits: []core.StageShare{{NodeID: a["node"].id, Amount: 2500}, {NodeID: a["auditor2"].id, Amount: 7500}}}
	before := map[string]int64{}
	for id, account := range s.Accounts {
		before[id] = account.Balance
	}
	if err := s.settlePipeline(h, "accept", 10); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]int64{a["node"].id: 2000, a["auditor2"].id: 6000, a["auditor"].id: 1500, a["payer"].id: 2500} {
		if got := s.Accounts[id].Balance - before[id]; got != want {
			t.Fatalf("credit %s=%d want %d", id, got, want)
		}
	}
	if h.Shares.Executor != 8000 || h.Shares.Auditor != 1500 || h.Shares.Consumer != 500 {
		t.Fatalf("shares %+v", h.Shares)
	}
}

func TestTrainPipelinePricingUsesFrozenStageProfileAndWork(t *testing.T) {
	s, a, nodes, auth, r := pipelineState(t)
	s.Accounts[a["payer"].id].Balance = 10000
	m := s.Models["ember-unit-test"]
	m.Pricing = &RolePricing{1, 1, 80, 15, 5}
	s.Models[m.ID] = m
	apply(t, s, a["payer"], 1, Action{Type: "freeze_pipeline", HoldID: "units", Payer: a["payer"].id, Amount: 100, TotalLayers: 4, PipelineAuthorizations: auth}, 2)
	raw, _ := proto.Marshal(r)
	apply(t, s, a["coordinator"], 1, Action{Type: "submit_pipeline", HoldID: "units", Actual: 100, Receipt: raw}, 3)
	h := s.Holds["units"]
	h.Auditor = a["auditor"].id
	before := s.Accounts[h.Auditor].Balance
	if err := s.settleAt(h, "accept", 6); err != nil {
		t.Fatal(err)
	}
	if h.Shares.Executor != 80 || h.Shares.Auditor != 15 || h.Shares.Consumer != 5 || s.Accounts[h.Auditor].Balance-before != 15 {
		t.Fatal("pipeline pricing", h.Shares)
	}
	expected, err := s.pipelineExecutorCredits(h, 80)
	if err != nil {
		t.Fatal(err)
	}
	for _, credit := range expected {
		if s.Accounts[credit.NodeID].Balance != credit.Amount {
			t.Fatal("double rounding", nodes, expected, h.Pipeline.Credits)
		}
	}
}
