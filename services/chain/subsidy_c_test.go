package chain

import (
	"fmt"
	"testing"
)

func governors(s *State) {
	s.Governors = map[string]bool{}
	for i := 0; i < SubsidyGovernors; i++ {
		s.Governors[fmt.Sprint(i)] = true
	}
}
func TestDeveloperApprovalPublicationAndDistribution(t *testing.T) {
	s := subsidyFixture(t)
	governors(s)
	acknowledge(t, s, "alice")
	acknowledge(t, s, "bob")
	cap := ratio(s.GenesisSubsidy.Total, SubsidyCGrantBPS, 10000)
	propose := func(id string, n int64) error {
		return s.subsidyC("0", &SubsidyAction{Op: "propose", ID: id, To: "alice", Amount: n})
	}
	if propose("too-big", cap+1) == nil {
		t.Fatal("per-grant cap")
	}
	amount := ratio(s.GenesisSubsidy.Total, SubsidyCDailyBPS, 10000)
	if err := propose("grant", amount); err != nil {
		t.Fatal(err)
	}
	if s.subsidyC("outsider", &SubsidyAction{Op: "approve", ID: "grant"}) == nil {
		t.Fatal("outsider approved")
	}
	for i := 0; i < SubsidyCApprovals-1; i++ {
		if err := s.subsidyC(fmt.Sprint(i), &SubsidyAction{Op: "approve", ID: "grant"}); err != nil {
			t.Fatal(err)
		}
	}
	if s.subsidyC("0", &SubsidyAction{Op: "approve", ID: "grant"}) == nil {
		t.Fatal("duplicate approval")
	}
	if s.subsidyC("alice", &SubsidyAction{Op: "issue", ID: "grant"}) == nil {
		t.Fatal("3-of-5 issued")
	}
	if err := s.subsidyC(fmt.Sprint(SubsidyCApprovals-1), &SubsidyAction{Op: "approve", ID: "grant"}); err != nil {
		t.Fatal(err)
	}
	if s.subsidyC("alice", &SubsidyAction{Op: "issue", ID: "grant"}) == nil {
		t.Fatal("early publication issued")
	}
	published := s.BlockTime
	if err := s.advanceSubsidyTime(published + SubsidyActivationSeconds - 1); err != nil {
		t.Fatal(err)
	}
	if s.subsidyC("alice", &SubsidyAction{Op: "issue", ID: "grant"}) == nil {
		t.Fatal("publication boundary")
	}
	if err := s.advanceSubsidyTime(published + SubsidyActivationSeconds); err != nil {
		t.Fatal(err)
	}
	if err := s.subsidyC("alice", &SubsidyAction{Op: "issue", ID: "grant"}); err != nil {
		t.Fatal(err)
	}
	if s.Accounts["alice"].Balance != 0 || s.Accounts["alice"].ConsumptionCredit != 0 {
		t.Fatal("developer reserve became spendable")
	}
	if s.subsidyC("bob", &SubsidyAction{Op: "distribute", ID: "grant", To: "bob", Amount: 1}) == nil {
		t.Fatal("unauthorized distribution")
	}
	if err := s.subsidyC("alice", &SubsidyAction{Op: "distribute", ID: "grant", To: "bob", Amount: 100}); err != nil {
		t.Fatal(err)
	}
	if s.Accounts["bob"].ConsumptionCredit != 0 {
		t.Fatal("terminal distribution skipped activation")
	}
	g := s.GenesisSubsidy.Developers["grant"]
	if err := s.advanceSubsidyTime(g.Expires); err != nil {
		t.Fatal(err)
	}
	if !g.Closed || g.Remaining != 0 || s.GenesisSubsidy.Channels["C"].Recovered != amount {
		t.Fatal("quarterly plus pending recovery")
	}
	if s.subsidyC("alice", &SubsidyAction{Op: "distribute", ID: "grant", To: "bob", Amount: 1}) == nil {
		t.Fatal("expired distribution")
	}
}
func TestSubsidyDailyIndependentCaps(t *testing.T) {
	for ch, bps := range map[string]int64{"A": SubsidyADailyBPS, "B": SubsidyBDailyBPS, "C": SubsidyCDailyBPS} {
		s := subsidyFixture(t)
		acknowledge(t, s, "alice")
		limit := ratio(s.GenesisSubsidy.Total, bps, 10000)
		if err := s.issueSubsidy(ch, "alice", limit, "cap", true); err != nil {
			t.Fatal(ch, err)
		}
		if s.issueSubsidy(ch, "alice", 1, "excess", true) == nil {
			t.Fatal(ch, "daily cap exceeded")
		}
		if err := s.advanceSubsidyTime(s.BlockTime + SubsidyDaySeconds); err != nil {
			t.Fatal(err)
		}
		if err := s.issueSubsidy(ch, "alice", 1, "next-day", true); err != nil {
			t.Fatal(ch, err)
		}
	}
	s := subsidyFixture(t)
	acknowledge(t, s, "alice")
	for ch, bps := range map[string]int64{"A": SubsidyADailyBPS, "B": SubsidyBDailyBPS, "C": SubsidyCDailyBPS} {
		if err := s.issueSubsidy(ch, "alice", ratio(s.GenesisSubsidy.Total, bps, 10000), "full", true); err != nil {
			t.Fatal(err)
		}
	}
	total := int64(0)
	for _, a := range s.GenesisSubsidy.Daily[s.BlockTime/SubsidyDaySeconds] {
		total += a
	}
	if total > ratio(s.GenesisSubsidy.Total, SubsidyGlobalDailyBPS, 10000) || s.issueSubsidy("A", "alice", 1, "over", true) == nil {
		t.Fatal("global limit")
	}
}

func TestDeveloperTranchesAndPermanentDisqualification(t *testing.T) {
	s := subsidyFixture(t)
	governors(s)
	acknowledge(t, s, "alice")
	amount := ratio(s.GenesisSubsidy.Total, SubsidyCGrantBPS, 10000)
	if err := s.subsidyC("0", &SubsidyAction{Op: "propose", ID: "tranches", To: "alice", Amount: amount}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < SubsidyCApprovals; i++ {
		if err := s.subsidyC(fmt.Sprint(i), &SubsidyAction{Op: "approve", ID: "tranches"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.advanceSubsidyTime(s.BlockTime + SubsidyActivationSeconds); err != nil {
		t.Fatal(err)
	}
	if s.subsidyC("alice", &SubsidyAction{Op: "issue", ID: "tranches"}) == nil {
		t.Fatal("single grant bypassed C daily cap")
	}
	first := ratio(s.GenesisSubsidy.Total, SubsidyCDailyBPS, 10000)
	if err := s.subsidyC("alice", &SubsidyAction{Op: "issue", ID: "tranches", Amount: first}); err != nil {
		t.Fatal(err)
	}
	if err := s.advanceSubsidyTime(s.BlockTime + SubsidyDaySeconds); err != nil {
		t.Fatal(err)
	}
	if err := s.subsidyC("alice", &SubsidyAction{Op: "issue", ID: "tranches"}); err != nil {
		t.Fatal(err)
	}
	if s.GenesisSubsidy.Developers["tranches"].Remaining != amount {
		t.Fatal("tranche total")
	}
	for i := 0; i < SubsidyCApprovals; i++ {
		if err := s.subsidyC(fmt.Sprint(i), &SubsidyAction{Op: "revoke", ID: "tranches", Reason: "misuse"}); err != nil {
			t.Fatal(err)
		}
	}
	if !s.GenesisSubsidy.DeveloperBanned["alice"] || s.GenesisSubsidy.Channels["C"].Recovered != amount {
		t.Fatal("revocation did not recover")
	}
	if s.subsidyC("0", &SubsidyAction{Op: "propose", ID: "again", To: "alice", Amount: 1}) == nil {
		t.Fatal("permanent ban bypassed")
	}
}

func TestDeveloperRevocationRecoversAllOutstandingGrants(t *testing.T) {
	s := subsidyFixture(t)
	governors(s)
	acknowledge(t, s, "alice")
	for _, id := range []string{"one", "two"} {
		if err := s.subsidyC("0", &SubsidyAction{Op: "propose", ID: id, To: "alice", Amount: 100}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < SubsidyCApprovals; i++ {
			if err := s.subsidyC(fmt.Sprint(i), &SubsidyAction{Op: "approve", ID: id}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.advanceSubsidyTime(s.BlockTime + SubsidyActivationSeconds); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two"} {
		if err := s.subsidyC("alice", &SubsidyAction{Op: "issue", ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < SubsidyCApprovals; i++ {
		if err := s.subsidyC(fmt.Sprint(i), &SubsidyAction{Op: "revoke", ID: "one", Reason: "misuse"}); err != nil {
			t.Fatal(err)
		}
	}
	if s.GenesisSubsidy.Channels["C"].Recovered != 200 || !s.GenesisSubsidy.Developers["two"].Closed {
		t.Fatal("ban left another grant outstanding")
	}
}
