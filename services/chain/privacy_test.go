package chain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPrivacyLifecycle(t *testing.T) {
	s, actors := privacyFixture(t)
	s.Privacy = nil
	send := func(who string, a PrivacyAction) {
		t.Helper()
		actor := actors[who]
		apply(t, s, actor, s.Accounts[actor.id].Nonce+1, Action{Type: "privacy", Privacy: &a}, s.Height+1)
	}
	fail := func(who string, a PrivacyAction) {
		t.Helper()
		actor := actors[who]
		raw, _ := Sign(s.Parameters.ChainID, actor.key, s.Accounts[actor.id].Nonce+1, Action{Type: "privacy", Privacy: &a})
		before, _ := s.Hash()
		if _, e := s.Apply(raw, s.Height+1); e == nil {
			t.Fatal("accepted invalid action")
		}
		after, _ := s.Hash()
		if string(before) != string(after) {
			t.Fatal("invalid tx changed state")
		}
	}
	policy := json.RawMessage(`{"retention_seconds":60,"deletion_practice":"best-effort","evidence_handling":"encrypted"}`)
	for _, who := range []string{"node", "auditor", "auditor2", "payer", "coordinator", "governor"} {
		send(who, PrivacyAction{Op: "operator", WrapKey: []byte(strings.Repeat("k", 32)), Policy: policy})
	}
	if !hash32(s.Privacy.Operators[actors["node"].id].PolicyHash) {
		t.Fatal("missing policy digest")
	}
	members := []string{actors["payer"].id, actors["coordinator"].id, actors["governor"].id}
	send("governor", PrivacyAction{Op: "epoch", Epoch: "e1", Threshold: 2, Members: members})
	request := PrivacyAction{Op: "request", Amount: 13, RequestID: "units", Executor: actors["node"].id, Tier: "P1", ModelID: "ember-unit-test", PricingCommitment: tokenCommitment(t, actors["node"], 2, 3), Epoch: "e1", EnvelopeHash: strings.Repeat("a", 64)}
	fail("payer", request)
	request.Tier = "P0"
	send("payer", request)
	send("payer", PrivacyAction{Op: "event", Kind: "share_refusal", Epoch: "e1", RequestID: "units", Reason: "no assignment"})
	fail("payer", PrivacyAction{Op: "event", Kind: "share_release", Epoch: "e1", RequestID: "units"})
	send("node", PrivacyAction{Op: "commit", Actual: 7, InputTokens: 2, OutputTokens: 1, RequestID: "units", Root: strings.Repeat("b", 64), BundleRef: strings.Repeat("c", 64)})
	fail("payer", PrivacyAction{Op: "assign", RequestID: "units", Auditor: actors["auditor"].id})
	send("coordinator", PrivacyAction{Op: "assign", RequestID: "units"})
	if !hash32(AssignmentProof(s.Parameters.ChainID, s.Privacy.Requests["units"])) {
		t.Fatal("proof")
	}
	send("payer", PrivacyAction{Op: "event", Kind: "share_release", Epoch: "e1", RequestID: "units"})
	fail("node", PrivacyAction{Op: "verdict", RequestID: "units", Verdict: "accept", StatsHash: strings.Repeat("d", 64)})
	who := "auditor"
	if s.Privacy.Requests["units"].Auditor == actors["auditor2"].id {
		who = "auditor2"
	}
	send(who, PrivacyAction{Op: "verdict", RequestID: "units", Verdict: "accept", StatsHash: strings.Repeat("d", 64)})
	send("payer", PrivacyAction{Op: "finalize", RequestID: "units"})
	send("node", PrivacyAction{Op: "event", RequestID: "units", Epoch: "e1", Kind: "bundle_deleted"})
	send("governor", PrivacyAction{Op: "end_epoch", Epoch: "e1"})
	send("payer", PrivacyAction{Op: "event", Epoch: "e1", Kind: "shares_destroyed"})
	fail("governor", PrivacyAction{Op: "epoch", Epoch: "e2", Threshold: 2, Members: members})
	for _, who := range []string{"payer", "coordinator", "governor"} {
		send(who, PrivacyAction{Op: "operator", WrapKey: []byte(strings.Repeat("z", 32)), Policy: policy})
	}
	send("governor", PrivacyAction{Op: "epoch", Epoch: "e2", Threshold: 2, Members: members})
	fail("payer", PrivacyAction{Op: "operator", WrapKey: []byte(strings.Repeat("x", 32)), Policy: policy})
}

func TestLegacyPrivacyTier(t *testing.T) {
	s, actors := testState(t)
	a := Action{Type: "freeze", HoldID: "tier", Payer: actors["payer"].id, Node: actors["node"].id, Amount: 1, Tier: "P1"}
	raw, _ := Sign(s.Parameters.ChainID, actors["payer"].key, 1, a)
	if _, err := s.Apply(raw, 1); err == nil || err.Error() != "TIER_UNAVAILABLE" {
		t.Fatal(err)
	}
	a.Tier = "P0"
	apply(t, s, actors["payer"], 1, a, 1)
	if s.Holds["tier"].PrivacyTier != "P0" {
		t.Fatal("tier not recorded")
	}
}
