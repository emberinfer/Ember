package chain

import (
	"fmt"
	"testing"
)

func retrospectiveFixture(t *testing.T, forecast int64) *State {
	t.Helper()
	s := &State{Accounts: map[string]*Account{"supply": {}, "consumer": {}, "audit": {}}}
	g := &GenesisSubsidy{OrganicYearEquivalents: forecast, Entities: map[string]string{"supply": "supplier", "consumer": "consumer", "alias": "supplier", "audit": "auditor"}}
	for i := 0; i < 200; i++ {
		g.TestnetRecords = append(g.TestnetRecords, TestnetContribution{RequestID: fmt.Sprint(i), RecordRef: fmt.Sprint("record-", i), Payer: "consumer", Executor: "supply", Auditor: "audit", Settled: true, AuditCompleted: true})
	}
	g.TestnetRecords = append(g.TestnetRecords, TestnetContribution{RequestID: "self", RecordRef: "self-ref", Payer: "alias", Executor: "supply", Auditor: "audit", Settled: true, AuditCompleted: true})
	if err := s.initGenesisSubsidy(g); err != nil {
		t.Fatal(err)
	}
	s.BlockTime = SubsidyDaySeconds
	return s
}
func TestRetrospectiveScoresCapAndExclusion(t *testing.T) {
	s := retrospectiveFixture(t, SubsidyTotalEquivalents*2)
	r := s.GenesisSubsidy.Retrospective
	if r.Scores["supply"] != 200*SubsidySupplyWeight || r.Scores["consumer"] != 200*SubsidyConsumeWeight || r.Scores["audit"] != 201*SubsidyAuditWeight || r.Scores["alias"] != 0 || len(r.Excluded) != 1 || r.Excluded[0] != "self" {
		t.Fatal("weights/self-exclusion", r.Scores, r.Excluded)
	}
	cap := SubsidyHouseholdEquivalents * SubsidyBCapMultiplier * SubsidyEquivalentSubunits
	if r.Allocations["supply"] != cap || r.Scaled || r.Unallocated <= 0 {
		t.Fatal("cap/remaining budget")
	}
	if s.claimRetrospective("supply") == nil {
		t.Fatal("B missing notice accepted")
	}
	acknowledge(t, s, "supply")
	if s.subsidyA("supply", Action{Type: "subsidy_claim_a"}) == nil {
		t.Fatal("lower A allowed")
	}
	if err := s.claimRetrospective("supply"); err != nil {
		t.Fatal(err)
	}
	if s.Accounts["supply"].ConsumptionCredit != cap || s.claimRetrospective("supply") == nil || s.subsidyA("supply", Action{Type: "subsidy_claim_a"}) == nil {
		t.Fatal("B restriction or stacking")
	}
	if s.GenesisSubsidy.Channels["A"].Issued != 0 {
		t.Fatal("B borrowed A budget")
	}
}
func TestRetrospectiveProportionalScale(t *testing.T) {
	s := retrospectiveFixture(t, 1000)
	r := s.GenesisSubsidy.Retrospective
	if !r.Scaled {
		t.Fatal("overbudget did not scale")
	}
	total := int64(0)
	for _, a := range r.Capped {
		total += a
	}
	budget := s.GenesisSubsidy.Channels["B"].Initial
	sum := int64(0)
	for id, a := range r.Allocations {
		if a != ratio(r.Capped[id], budget, total) {
			t.Fatal("not proportional")
		}
		sum += a
	}
	if sum > budget || r.Unallocated != budget-sum || s.GenesisSubsidy.Channels["B"].Balance != budget {
		t.Fatal("rounding or reserve")
	}
}
func TestRetrospectiveRejectsUnverifiableIdentity(t *testing.T) {
	s := &State{Accounts: map[string]*Account{}}
	if s.initGenesisSubsidy(&GenesisSubsidy{OrganicYearEquivalents: 1000, TestnetRecords: []TestnetContribution{{RequestID: "x", RecordRef: "x", Payer: "a", Executor: "b", Settled: true}}}) == nil {
		t.Fatal("unknown entity accepted")
	}
}

func TestRetrospectiveSelectionTakeHigher(t *testing.T) {
	build := func(records int) *State {
		s := &State{Accounts: map[string]*Account{"worker": {}, "peer": {}}}
		g := &GenesisSubsidy{OrganicYearEquivalents: SubsidyTotalEquivalents * 2, Entities: map[string]string{"worker": "worker-entity", "peer": "peer-entity"}}
		for i := 0; i < records; i++ {
			g.TestnetRecords = append(g.TestnetRecords, TestnetContribution{RequestID: fmt.Sprint(i), RecordRef: fmt.Sprint("record-", i), Payer: "worker", Executor: "peer", Settled: true})
		}
		if err := s.initGenesisSubsidy(g); err != nil {
			t.Fatal(err)
		}
		s.BlockTime = SubsidyDaySeconds
		return s
	}
	amount := SubsidyHouseholdEquivalents * SubsidyEquivalentSubunits
	// B below A: the retrospective claim is refused; the higher A remains open.
	s := build(50)
	if got := s.GenesisSubsidy.Retrospective.Allocations["worker"]; got != amount/2 {
		t.Fatal("fixture allocation", got)
	}
	acknowledge(t, s, "worker")
	if s.claimRetrospective("worker") == nil {
		t.Fatal("lower B claim allowed over higher A")
	}
	if err := s.subsidyA("worker", Action{Type: "subsidy_claim_a"}); err != nil {
		t.Fatal(err)
	}
	if s.claimRetrospective("worker") == nil {
		t.Fatal("B stacked on A below entitlement")
	}
	// B exactly equal to A: either channel may be taken, never both. Only the
	// grants-map exclusion can reject the second claim at equality.
	s = build(100)
	if got := s.GenesisSubsidy.Retrospective.Allocations["worker"]; got != amount {
		t.Fatal("fixture allocation", got)
	}
	acknowledge(t, s, "worker")
	if err := s.subsidyA("worker", Action{Type: "subsidy_claim_a"}); err != nil {
		t.Fatal(err)
	}
	if s.claimRetrospective("worker") == nil {
		t.Fatal("B stacked on A at equal entitlement")
	}
	s = build(100)
	acknowledge(t, s, "worker")
	if err := s.claimRetrospective("worker"); err != nil {
		t.Fatal(err)
	}
	if s.Accounts["worker"].ConsumptionCredit != amount {
		t.Fatal("equal B did not pay the full higher amount")
	}
	if s.subsidyA("worker", Action{Type: "subsidy_claim_a"}) == nil {
		t.Fatal("A stacked on B at equal entitlement")
	}
}
