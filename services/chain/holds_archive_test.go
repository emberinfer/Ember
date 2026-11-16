package chain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	abci "github.com/cometbft/cometbft/abci/types"
	"os"
	"path/filepath"
	"testing"
)

func TestHoldsArchiveReplayAndCrash(t *testing.T) {
	s, a := testState(t)
	s.Version = 11
	freeze := Action{Type: "freeze", HoldID: "archived", Payer: a["payer"].id, Node: a["node"].id, Amount: 100, IdempotencyKey: IdempotencyKey([]byte(a["payer"].id), []byte("archive"))}
	original := apply(t, s, a["payer"], 1, freeze, 1)
	apply(t, s, a["payer"], 2, Action{Type: "cancel", HoldID: "archived"}, 2)
	whole, _ := json.Marshal(s.Holds["archived"])
	s.Holds["open"] = &Hold{ID: "open", State: "frozen", ExpireHeight: 100}
	s.Holds["appealable"] = &Hold{ID: "appealable", State: "settled", SettleHeight: 1}
	s.Holds["commit-pending"] = &Hold{ID: "commit-pending", State: "slashed", Adjudication: &AdjudicationCase{ThirdAuditor: a["auditor"].id}}
	s.Holds["deposit-pending"] = &Hold{ID: "deposit-pending", State: "cancelled", Adjudication: &AdjudicationCase{Deposit: AdjudicationDeposit{From: a["payer"].id, Locked: 1}}}
	path := filepath.Join(t.TempDir(), "state.json")
	if e := SaveState(path, s); e != nil {
		t.Fatal(e)
	}
	app := &Application{committed: s, path: path}
	if _, e := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: 3}); e != nil {
		t.Fatal(e)
	}
	if app.staged.Holds["archived"] != nil || len(app.staged.Holds) != 4 {
		t.Fatal("nonterminal lost or terminal retained")
	}
	if e := persistArchive(path, app.staged, app.stagedBytes); e != nil {
		t.Fatal(e)
	}
	cold, e := LoadState(path)
	if e != nil || cold.Holds["archived"] == nil {
		t.Fatal("pre-rename crash lost hold", e)
	}
	// Force state rename failure after archive durability. Staged data remains retryable.
	oldPath := app.path
	app.path = filepath.Join(t.TempDir(), "directory")
	if e = os.Mkdir(app.path, 0700); e != nil {
		t.Fatal(e)
	}
	if _, e = app.Commit(context.Background(), &abci.CommitRequest{}); e == nil || app.staged == nil {
		t.Fatal("failed commit published")
	}
	app.path = oldPath
	if _, e = app.Commit(context.Background(), &abci.CommitRequest{}); e != nil {
		t.Fatal(e)
	}
	cold, e = LoadState(path)
	if e != nil {
		t.Fatal(e)
	}
	got, _ := json.Marshal(cold.LookupHold("archived"))
	if !bytes.Equal(got, whole) {
		t.Fatal("archive clipped record")
	}
	if cold.Holds["archived"] != nil {
		t.Fatal("terminal remains in consensus")
	}
	// Historical queries and idempotent rerouting survive cold restart.
	q, e := app.Query(context.Background(), &abci.QueryRequest{Path: "/hold/archived"})
	if e != nil || !bytes.Equal(q.Value, whole) {
		t.Fatal("historical query", e)
	}
	sum := sha256.Sum256(original)
	freeze.HoldID = "rerouted"
	for _, nonce := range []uint64{1, 3} {
		raw, _ := Sign(cold.Parameters.ChainID, a["payer"].key, nonce, freeze)
		before, _ := cold.Bytes()
		got, e := cold.Apply(raw, 4)
		after, _ := cold.Bytes()
		if e != nil || got != hex.EncodeToString(sum[:]) || !bytes.Equal(before, after) {
			t.Fatal("archived idempotency", e)
		}
	}
	freeze.Amount++
	raw, _ := Sign(cold.Parameters.ChainID, a["payer"].key, 3, freeze)
	before, _ := cold.Bytes()
	if _, e = cold.Apply(raw, 4); e == nil {
		t.Fatal("changed payload accepted")
	}
	after, _ := cold.Bytes()
	if !bytes.Equal(before, after) {
		t.Fatal("failed archived replay mutated")
	}
	dest := filepath.Join(t.TempDir(), "copy.json")
	if e = SaveState(dest, cold); e != nil {
		t.Fatal(e)
	}
	copied, e := LoadState(dest)
	if e != nil || copied.LookupHold("archived") == nil {
		t.Fatal("snapshot archive copy", e)
	}
}

func TestArchiveHistoricalConsensusReaders(t *testing.T) {
	s, a := testState(t)
	s.Version = 11
	s.Height = 10
	node := a["node"].id
	h := &Hold{ID: "closed", State: "settled", Node: node, ExpireHeight: 20, ModelID: "ember-test-model", AuditStatsEvents: []AuditStatsEvent{{Kind: "audit", Node: a["auditor"].id, Height: 1, Actual: 9}}}
	s.Holds[h.ID] = h
	wantCount := s.settledWorkCount(node)
	wantExit := s.checkExitWindows(node, 10)
	s.pruneBlockHashes()
	s.archiveTerminalHolds()
	if s.settledWorkCount(node) != wantCount || s.checkExitWindows(node, 10) == nil || wantExit == nil {
		t.Fatal("historical reputation/exit lost")
	}
	blocks := s.Parameters.AuditWork.EpochBlocks
	s.Height = blocks + 1
	s.AuditPool = map[string]map[int64]int64{h.ModelID: {0: 9}}
	if e := s.distributeAuditPool(h.ModelID, 0, s.Height); e != nil {
		t.Fatal(e)
	}
	d := s.AuditPoolDistributions[len(s.AuditPoolDistributions)-1]
	if d.Credits[a["auditor"].id] != 9 {
		t.Fatal("historical audit pool credits lost")
	}
}

func TestArchiveAcrossCommitsAndCheckTxIsolation(t *testing.T) {
	s, a := testState(t)
	s.Version = 11
	s.Height = 4999
	s.PriceEpochStart = 4999
	app := &Application{committed: s, path: filepath.Join(t.TempDir(), "state.json")}
	for i := int64(0); i < 3; i++ {
		if _, e := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: 5000 + i, Hash: bytes.Repeat([]byte{byte(i + 1)}, 32)}); e != nil {
			t.Fatal(e)
		}
		if _, e := app.Commit(context.Background(), &abci.CommitRequest{}); e != nil {
			t.Fatal(e)
		}
		cold, e := OpenApplication(app.path)
		if e != nil {
			t.Fatal(e)
		}
		app = cold
		if len(app.committed.blockHash(1)) != 32 {
			t.Fatal("historical entropy lost across manifests")
		}
	}
	app.committed.Accounts[a["node"].id].Balance = 1<<63 - 1
	for _, amount := range []int64{1, 99999} {
		raw, _ := Sign(s.Parameters.ChainID, a["payer"].key, 1, Action{Type: "transfer", To: a["node"].id, Amount: amount})
		before, _ := app.committed.Bytes()
		r, e := app.CheckTx(context.Background(), &abci.CheckTxRequest{Tx: raw})
		after, _ := app.committed.Bytes()
		if e != nil || r.Code == 0 || !bytes.Equal(before, after) {
			t.Fatal("CheckTx changed committed state", e)
		}
	}
}
