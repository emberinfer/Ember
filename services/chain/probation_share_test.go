package chain

import "testing"

// runShadowedLifecycle drives one freeze -> shadow-attributed submit ->
// audit accept -> settle flow and returns the settled state.
func runShadowedLifecycle(t *testing.T, redirect bool, priorSettled int) (*State, map[string]actor, actor) {
	t.Helper()
	state, actors := testState(t)
	state.Parameters.ProbationShareRedirect = redirect
	shadow := newActor(7)
	state.Nodes[shadow.id] = &NodeRegistration{NodeID: shadow.id, RegisteredAt: 0, ExpiresHeight: 100}
	for i := 0; i < priorSettled; i++ {
		id := "prior-" + string(rune('a'+i))
		state.Holds[id] = &Hold{State: "settled", Node: actors["node"].id, Payer: actors["payer"].id, Amount: 1, Actual: 1}
	}
	holdID := "request-shadow"
	apply(t, state, actors["payer"], 1, Action{Type: "freeze", HoldID: holdID, Payer: actors["payer"].id, Node: actors["node"].id, Amount: 100}, 1)
	receipt := signedReceipt(t, holdID, actors["node"])
	apply(t, state, actors["coordinator"], 1, Action{Type: "submit", HoldID: holdID, Actual: 100, Receipt: receipt, ShadowNode: shadow.id}, 2)
	apply(t, state, actors["coordinator"], 2, Action{Type: "assign_audit", HoldID: holdID}, 3)
	auditor, proof := assignedActor(t, state, actors, holdID)
	challenge, verdict, _ := signedAudit(t, receipt, auditor, "accept", proof)
	apply(t, state, auditor, 1, Action{Type: "audit", HoldID: holdID, Challenge: challenge, Verdict: verdict}, 4)
	apply(t, state, actors["coordinator"], 3, Action{Type: "settle", HoldID: holdID, Outcome: "accept"}, 4)
	return state, actors, shadow
}

// With the governance switch on, a probation executor's shadow-verified
// settlement pays 15% of the fee to the shadow node; the consumer's
// price and refund are untouched.
func TestProbationShareRedirectPaysShadow(t *testing.T) {
	state, actors, shadow := runShadowedLifecycle(t, true, 0)
	if got := state.Accounts[actors["node"].id].Balance; got != 85 {
		t.Fatalf("executor balance %d, want 85 (100 fee - 15 shadow share)", got)
	}
	if got := state.Accounts[shadow.id].Balance; got != 15 {
		t.Fatalf("shadow balance %d, want 15", got)
	}
	if got := state.Accounts[actors["payer"].id].Balance; got != 900 {
		t.Fatalf("payer balance %d, want 900 (paid exactly the standard 100)", got)
	}
	if got := state.Holds["request-shadow"].ShadowPaid; got != 15 {
		t.Fatalf("recorded shadow payment %d, want 15", got)
	}
}

func balanceOf(state *State, id string) int64 {
	if account := state.Accounts[id]; account != nil {
		return account.Balance
	}
	return 0
}

// Default off: settlement shares are byte-identical to the legacy rule.
func TestProbationShareRedirectDefaultOff(t *testing.T) {
	state, actors, shadow := runShadowedLifecycle(t, false, 0)
	if got := state.Accounts[actors["node"].id].Balance; got != 100 {
		t.Fatalf("executor balance %d, want 100 with the switch off", got)
	}
	if got := balanceOf(state, shadow.id); got != 0 {
		t.Fatalf("shadow balance %d, want 0 with the switch off", got)
	}
}

// A graduated executor keeps its full share even when a shadow is named:
// the redirect is the probation node's cost of buying reputation, and
// it ends at graduation.
func TestProbationShareRedirectSkipsGraduated(t *testing.T) {
	state, actors, shadow := runShadowedLifecycle(t, true, 25)
	if got := state.Accounts[actors["node"].id].Balance; got != 100 {
		t.Fatalf("graduated executor balance %d, want 100", got)
	}
	if got := balanceOf(state, shadow.id); got != 0 {
		t.Fatalf("shadow balance %d, want 0 for a graduated executor", got)
	}
}

// A submit naming the executor itself or an unregistered identity as
// its shadow is rejected at the transition.
func TestSubmitRejectsInvalidShadow(t *testing.T) {
	for _, bad := range []string{"self", "ghost"} {
		state, actors := testState(t)
		shadow := actors["node"].id
		if bad == "ghost" {
			shadow = newActor(9).id
		}
		holdID := "request-badshadow"
		apply(t, state, actors["payer"], 1, Action{Type: "freeze", HoldID: holdID, Payer: actors["payer"].id, Node: actors["node"].id, Amount: 100}, 1)
		receipt := signedReceipt(t, holdID, actors["node"])
		raw, err := Sign(state.Parameters.ChainID, actors["coordinator"].key, 1,
			Action{Type: "submit", HoldID: holdID, Actual: 100, Receipt: receipt, ShadowNode: shadow})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := state.Apply(raw, 2); err == nil {
			t.Fatalf("submit with %s shadow accepted", bad)
		}
	}
}
