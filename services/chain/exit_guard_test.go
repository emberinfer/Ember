package chain

import (
	"context"
	abci "github.com/cometbft/cometbft/abci/types"
	"strings"
	"testing"
)

func TestExitWindowsTestChain(t *testing.T) {
	s, a := testState(t)
	s.Parameters.DisputeBlocks = 40
	apply(t, s, a["payer"], 1, Action{Type: "freeze", HoldID: "last-served", Payer: a["payer"].id, Node: a["node"].id, Amount: 5}, 1)
	apply(t, s, a["coordinator"], 1, Action{Type: "submit", HoldID: "last-served", Actual: 1, Receipt: signedReceipt(t, "last-served", a["node"])}, 2)
	// Existing timeout refund can finalize before the receipt challenge deadline.
	apply(t, s, a["payer"], 2, Action{Type: "expire_hold", HoldID: "last-served"}, 21)
	app := &Application{committed: s}
	tx, _ := Sign(s.Parameters.ChainID, a["node"].key, 1, Action{Type: "unbond", Amount: 5000})
	check, err := app.CheckTx(context.Background(), &abci.CheckTxRequest{Tx: tx})
	if err != nil || check.Code == 0 {
		t.Fatalf("CheckTx: %+v %v", check, err)
	}
	response, err := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: 22, Txs: [][]byte{tx}})
	if err != nil || response.TxResults[0].Code == 0 || !strings.Contains(response.TxResults[0].Log, "request challenge window open") {
		t.Fatalf("FinalizeBlock: %+v %v", response, err)
	}
	t.Logf("test-chain=%s request=last-served submit=2 refund=21 attempt=22 close=42 CheckTx=%d FinalizeBlock=%d error=%q", s.Parameters.ChainID, check.Code, response.TxResults[0].Code, response.TxResults[0].Log)
	if _, err = app.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
		t.Fatal(err)
	}
	s, err = app.State()
	if err != nil {
		t.Fatal(err)
	}
	if s.Accounts[a["node"].id].Stake != 5000 || s.Accounts[a["node"].id].Nonce != 0 {
		t.Fatal("rejection mutated account")
	}
	apply(t, s, a["node"], 1, Action{Type: "unbond", Amount: 5000}, 42)
	if s.Accounts[a["node"].id].Unbondings[0].ReleaseHeight != 45 {
		t.Fatal("wrong release height")
	}
	apply(t, s, a["node"], 2, Action{Type: "claim_unbonded"}, 45)
}

func TestExitWindowsClaimsPipelineAndBoundary(t *testing.T) {
	for _, pipeline := range []bool{false, true} {
		s, a := testState(t)
		node := a["node"].id
		h := &Hold{ID: "retained", Node: node, State: "cancelled", ExpireHeight: 20, SettleHeight: 30}
		if pipeline {
			h.Node = a["coordinator"].id
			h.Pipeline = &PipelineHold{Authorizations: []PipelineAuthorization{{NodeID: node}}}
		}
		s.Holds[h.ID] = h
		s.Accounts[node].Unbondings = []Unbond{{Amount: 2, ReleaseHeight: 1}}
		rejected(t, s, a["node"], Action{Type: "claim_unbonded"}, 29)
		rejected(t, s, a["node"], Action{Type: "unbond", Amount: 1}, 29)
		apply(t, s, a["node"], 1, Action{Type: "claim_unbonded"}, 30)
	}
}

func TestExitGuardRetiredPrivacyExecutor(t *testing.T) {
	s, a := testState(t)
	old := a["node"].id
	s.Holds["migrated"] = &Hold{ID: "migrated", Node: a["coordinator"].id, State: "dispute", ExpireHeight: 20, SettleHeight: 30}
	s.Privacy = &PrivacyState{Requests: map[string]*PrivacyRequest{"migrated": {Migrations: []PrivacyMigration{{From: old, To: a["coordinator"].id, Finalized: true}}}}}
	rejected(t, s, a["node"], Action{Type: "unbond", Amount: 5000}, 29)
	rejected(t, s, a["node"], Action{Type: "unbond", Amount: 5000}, 30)
	s.Holds["migrated"].State = "cancelled"
	apply(t, s, a["node"], 1, Action{Type: "unbond", Amount: 5000}, 30)
}
