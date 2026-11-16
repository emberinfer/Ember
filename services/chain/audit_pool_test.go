package chain

import "testing"

func TestAuditPoolProRataRemaindersAndRollover(t *testing.T) {
	s, a := samplingState(t)
	x, y := a["auditor"].id, a["auditor2"].id
	h := sampleHold(s, a, 1)
	h.AuditStatsEvents = []AuditStatsEvent{{Kind: "audit", Node: x, Height: 5, Actual: 300}, {Kind: "audit", Node: y, Height: 6, Actual: 100}}
	if err := s.fundAuditPool(h, 100, 10); err != nil {
		t.Fatal(err)
	}
	bx, by := s.Accounts[x].Balance, s.Accounts[y].Balance
	if err := s.distributeAuditPool(h.ModelID, 0, 999); err == nil {
		t.Fatal("early distribution")
	}
	if err := s.distributeAuditPool(h.ModelID, 0, 1000); err != nil {
		t.Fatal(err)
	}
	if s.Accounts[x].Balance-bx != 75 || s.Accounts[y].Balance-by != 25 || s.AuditPool[h.ModelID][0] != 0 {
		t.Fatal("3:1 allocation")
	}
	if err := s.distributeAuditPool(h.ModelID, 0, 1000); err != nil {
		t.Fatal(err)
	}
	if s.Accounts[x].Balance-bx != 75 {
		t.Fatal("double distribution")
	}
	if err := s.fundAuditPool(h, 101, 1001); err != nil {
		t.Fatal(err)
	}
	if err := s.distributeAuditPool(h.ModelID, 1, 2000); err != nil {
		t.Fatal(err)
	}
	if s.AuditPool[h.ModelID][2] != 101 {
		t.Fatal("zero-audit pool lost")
	}
	h.AuditStatsEvents = append(h.AuditStatsEvents, AuditStatsEvent{Kind: "audit", Node: x, Height: 2001, Actual: 300}, AuditStatsEvent{Kind: "audit", Node: y, Height: 2002, Actual: 100})
	if err := s.distributeAuditPool(h.ModelID, 2, 3000); err != nil {
		t.Fatal(err)
	}
	if s.Accounts[x].Balance-bx != 151 || s.Accounts[y].Balance-by != 50 {
		t.Fatal("largest remainder")
	}
	if err := s.distributeAuditPool("unpriced-empty", 0, 3000); err != nil {
		t.Fatal("empty unpriced pool must no-op", err)
	}
}

func TestAuditPoolProbationConservation(t *testing.T) {
	s, a := samplingState(t)
	s.Parameters.ProbationShareRedirect = true
	m := s.Models["ember-test-model"]
	m.Pricing = &RolePricing{1, 1, 80, 15, 5}
	s.Models[m.ID] = m
	h := sampleHold(s, a, 1)
	h.Unaudited = true
	h.ShadowNode = a["auditor2"].id
	before := int64(0)
	for _, v := range s.Accounts {
		before += v.Balance
	}
	if err := s.settleAt(h, "accept", 10); err != nil {
		t.Fatal(err)
	}
	after := int64(0)
	for _, v := range s.Accounts {
		after += v.Balance
	}
	if after-before+s.AuditPool[h.ModelID][0] != h.Amount || h.ShadowPaid != 15 || h.Shares.Auditor != 15 || s.AuditPool[h.ModelID][0] != 15 {
		t.Fatal("redirect double spent audit allocation", h.ShadowPaid, after-before)
	}
}
