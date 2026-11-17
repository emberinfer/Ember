package chain

import (
	"context"
	"crypto/sha256"
	core "ember/core-go"
	pb "ember/proto"
	"encoding/hex"
	"encoding/json"
	"fmt"
	abci "github.com/cometbft/cometbft/abci/types"
	"google.golang.org/protobuf/proto"
	"os"
	"testing"
)

// External execution evidence is explicit so ordinary Go tests need no Python
// runtime. This reproduces unchanged gray refunds; it adds no treatment policy.
func TestFairnessGrayRefundReproduction(t *testing.T) {
	path := os.Getenv("FAIRNESS_GRAY_FIXTURE")
	if path == "" {
		t.Skip("set FAIRNESS_GRAY_FIXTURE to ops/fairness_gray_probe.py output")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Executor string `json:"executor"`
		Rows     []struct {
			Request   string    `json:"request"`
			Receipt   string    `json:"receipt"`
			Scores    []float64 `json:"scores"`
			Output    string    `json:"output"`
			Delivered uint32    `json:"delivered_tokens"`
			Elapsed   int64     `json:"executor_elapsed_ns"`
		} `json:"rows"`
	}
	if err = json.Unmarshal(raw, &fixture); err != nil || len(fixture.Rows) < 5 {
		t.Fatal("fixture", err)
	}
	s, a := setupMisjudge(t)
	s.BlockHashes = nil
	if fixture.Executor != a["node"].id {
		t.Fatal("wrong executor")
	}
	app := &Application{committed: s}
	height := int64(0)
	block := func(who *actor, action Action) {
		t.Helper()
		height++
		var txs [][]byte
		if who != nil {
			current, e := app.State()
			if e != nil {
				t.Fatal(e)
			}
			tx, e := Sign(s.Parameters.ChainID, who.key, current.Accounts[who.id].Nonce+1, action)
			if e != nil {
				t.Fatal(e)
			}
			txs = [][]byte{tx}
		}
		hash := sha256.Sum256([]byte(fmt.Sprintf("fairness-test-block-%d", height)))
		result, e := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: height, Hash: hash[:], Txs: txs})
		if e != nil {
			t.Fatal(e)
		}
		for _, tx := range result.TxResults {
			if tx.Code != 0 {
				t.Fatal(tx.Log)
			}
		}
		if _, e = app.Commit(context.Background(), &abci.CommitRequest{}); e != nil {
			t.Fatal(e)
		}
	}
	totalRefund, totalUnpaid, totalTokens := int64(0), int64(0), uint32(0)
	for i, row := range fixture.Rows[:5] {
		if row.Output == "" || row.Delivered != 4 || len(row.Scores) != 2 {
			t.Fatal("no delivered execution")
		}
		receipt, e := hex.DecodeString(row.Receipt)
		if e != nil {
			t.Fatal(e)
		}
		payer := a["payer"]
		coordinator := a["coordinator"]
		block(&payer, Action{Type: "freeze", HoldID: row.Request, Payer: payer.id, Node: a["node"].id, Amount: 10})
		block(&coordinator, Action{Type: "submit", HoldID: row.Request, Actual: 4, Receipt: receipt})
		block(nil, Action{})
		block(nil, Action{})
		block(&coordinator, Action{Type: "assign_audit", HoldID: row.Request})
		for round := 0; round < 2; round++ {
			current, e := app.State()
			if e != nil {
				t.Fatal(e)
			}
			auditor, proof := assignedActor(t, current, a, row.Request)
			challenge, vr, _ := signedAudit(t, receipt, auditor, "reject", proof)
			v := &pb.AuditVerdict{}
			if e = proto.Unmarshal(vr, v); e != nil {
				t.Fatal(e)
			}
			v.Offense = numericGrade(row.Scores[round])
			v.Distances = []float64{row.Scores[round]}
			if e = core.Sign(v, auditor.key); e != nil {
				t.Fatal(e)
			}
			vr, e = proto.Marshal(v)
			if e != nil {
				t.Fatal(e)
			}
			block(&auditor, Action{Type: "audit", HoldID: row.Request, Challenge: challenge, Verdict: vr})
		}
		before, e := app.State()
		if e != nil {
			t.Fatal(e)
		}
		block(&coordinator, Action{Type: "settle", HoldID: row.Request, Outcome: "reject"})
		current, e := app.State()
		if e != nil {
			t.Fatal(e)
		}
		refund := current.Accounts[payer.id].Balance - before.Accounts[payer.id].Balance
		earned := current.Accounts[a["node"].id].Balance - before.Accounts[a["node"].id].Balance
		record := current.OffenseRecords[len(current.OffenseRecords)-1]
		if refund != 10 || earned != 0 || len(current.Holds[row.Request].Slashes) != 0 || current.Accounts[payer.id].Balance != 1000 {
			t.Fatal("reproduction failed")
		}
		totalRefund += refund
		totalUnpaid += 4
		totalTokens += row.Delivered
		t.Logf("row=%d request=%s settlement_height=%d score=%.9f status=%s delivered=%d refund=%d executor_paid=%d unpaid_fee=%d execution_ns=%d", i+1, row.Request, height, row.Scores[0], record.Status, row.Delivered, refund, earned, 4, row.Elapsed)
	}
	current, err := app.State()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("TOTAL requests=5 gray_decisions=%d refunded=%d delivered_tokens=%d executor_paid=0 unpaid_fee=%d consumer_net_spend=0 executor_stake=%d", current.GrayObservations[a["node"].id], totalRefund, totalTokens, totalUnpaid, current.Accounts[a["node"].id].Stake)
}
