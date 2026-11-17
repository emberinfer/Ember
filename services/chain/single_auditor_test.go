package chain

import "testing"

func TestSingleAuditorCooldownEvidence(t *testing.T) {
	s, a := testState(t)
	s.Version = 8
	s.Auditors = map[string]bool{a["auditor"].id: true}
	s.Parameters.AuditSampling.defaults()
	s.Parameters.Assignment.CooldownRequests = 2
	s.PairStreaks = map[string]PairStreak{a["node"].id: {Auditor: a["auditor"].id, Consecutive: 2}}
	apply(t, s, a["payer"], 1, Action{Type: "freeze", HoldID: "single-cooldown", Payer: a["payer"].id, Node: a["node"].id, Amount: 10}, 1)
	winner, _, err := s.selectAuditor(s.Holds["single-cooldown"], "")
	if err != nil {
		t.Fatal(err)
	}
	r := s.Holds["single-cooldown"].Assignments[0]
	if winner != a["auditor"].id || !r.FallbackCooldown {
		t.Fatal("missing single-pool fallback evidence")
	}
	if got, err := RecomputeAssignment(r); err != nil || got != winner {
		t.Fatal("recompute", err)
	}
}
