package chain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
)

func TestRequestFirstDataFinalityAndIdempotency(t *testing.T) {
	s, a := testState(t)
	f := Action{Type: "freeze", HoldID: "first-data", Payer: a["payer"].id, Node: a["node"].id, Amount: 10}
	raw := apply(t, s, a["payer"], 1, f, 1)
	h := s.Holds[f.HoldID]
	if h.RequestID != RequestID(f.Payer, 1, freezePayload(f)) {
		t.Fatal("wrong frozen identity")
	}
	before, _ := s.Hash()
	if _, err := s.Apply(raw, 2); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Hash()
	if !bytes.Equal(before, after) {
		t.Fatal("identical resend mutated consensus state")
	}
	// A newly signed envelope still refers to the original request and freeze.
	apply(t, s, a["payer"], 2, f, 2)
	if s.Holds[f.HoldID].FreezeHeight != 1 || s.Accounts[f.Payer].Balance != 990 {
		t.Fatal("double freeze")
	}
	altered := f
	altered.Amount++
	rejected(t, s, a["payer"], altered, 3)
	altered = f
	altered.Node = a["auditor"].id
	rejected(t, s, a["payer"], altered, 3)
	s.Holds[f.HoldID].Actual = 1
	if err := s.settleAt(s.Holds[f.HoldID], "accept", 4); err != nil {
		t.Fatal(err)
	}
	before, _ = s.Hash()
	if _, err := s.Apply(raw, 5); err != nil {
		t.Fatal(err)
	}
	after, _ = s.Hash()
	if !bytes.Equal(before, after) || s.Accounts[f.Payer].Balance != 999 || s.Accounts[f.Node].Balance != 1 {
		t.Fatal("retry double settled or froze")
	}
	if _, err := s.Apply(raw[:len(raw)-1], 5); err == nil {
		t.Fatal("unauthenticated retry")
	}
}

func TestFutureEntropyArrivalAndOfflineDraw(t *testing.T) {
	s, a := testState(t)
	s.BlockHashes = nil
	f := Action{Type: "freeze", HoldID: "future", Payer: a["payer"].id, Node: a["node"].id, Amount: 10}
	apply(t, s, a["payer"], 1, f, 1)
	app := &Application{committed: s}
	ctx := context.Background()
	for height := int64(2); height <= 4; height++ {
		if _, _, err := app.committed.selectAuditor(app.committed.Holds[f.HoldID], ""); err == nil {
			t.Fatal("used unavailable future entropy")
		}
		hash := sha256.Sum256([]byte(fmt.Sprint(height)))
		if _, err := app.FinalizeBlock(ctx, &abci.FinalizeBlockRequest{Height: height, Hash: hash[:]}); err != nil {
			t.Fatal(err)
		}
		if _, err := app.Commit(ctx, &abci.CommitRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	s = app.committed
	h := s.Holds[f.HoldID]
	selected, proof, err := s.selectAuditor(h, "")
	if err != nil {
		t.Fatal(err)
	}
	record := h.Assignments[0]
	if len(record.SeedBlocks) != 3 {
		t.Fatal("default M")
	}
	offline, err := RecomputeAssignment(record)
	if err != nil || selected != offline || !bytes.Equal(proof, record.Seed) {
		t.Fatal("offline recomputation differs", err)
	}
	// Neither execution data nor the assignment transaction's timing enters seed.
	h.ReceiptRef = "freely changed output"
	s.Entropy = bytes.Repeat([]byte{99}, 32)
	s.Height += 10
	seed, _, err := s.assignmentSeed(h)
	if err != nil || !bytes.Equal(seed, proof) {
		t.Fatal("executor/timing influenced seed", err)
	}
	record.SeedBlocks[0][0] ^= 1
	if _, err := RecomputeAssignment(record); err == nil {
		t.Fatal("tampered seed record accepted")
	}
}

func TestPairCooldownRotationAndReset(t *testing.T) {
	s, a := testState(t)
	s.Parameters.Assignment.CooldownRequests = 8
	executor := a["node"].id
	first, second := a["auditor"].id, a["auditor2"].id
	// A one-member eligible pool makes the streak boundary deterministic.
	for i := 0; i < 8; i++ {
		h := &Hold{RequestID: RequestID("payer", uint64(i), []byte("commitment")), FreezeHeight: 1, Node: executor}
		if id, _, err := s.drawAuditor(h, []string{first}); err != nil || id != first {
			t.Fatal(i, id, err)
		}
	}
	h := &Hold{RequestID: RequestID("payer", 10, []byte("commitment")), FreezeHeight: 1, Node: executor}
	if _, _, err := s.drawAuditor(h, []string{first}); err == nil {
		t.Fatal("silently bypassed cooldown with no replacement")
	}
	if id, _, err := s.drawAuditor(h, []string{first, second}); err != nil || id != second {
		t.Fatal("rotation did not skip old pair", id, err)
	}
	if id, _, err := s.drawAuditor(h, []string{first}); err != nil || id != first {
		t.Fatal("different pair did not reset streak", id, err)
	}
	if s.PairStreaks[executor].Consecutive != 1 {
		t.Fatal("wrong reset count")
	}
}

func TestAssignmentGrinding100000(t *testing.T) {
	const requests = 100000
	s := &State{Parameters: Parameters{Assignment: AssignmentParameters{MBlocks: 3, CooldownRequests: 8}}, BlockHashes: map[int64][]byte{}}
	pool := make([]string, 100)
	for i := range pool {
		pool[i] = fmt.Sprintf("auditor-%02d", i)
	}
	hits := 0
	for i := 0; i < requests; i++ {
		// Choose among 256 commitment variants using only information available
		// before freeze. Future block hashes are disclosed after the choice.
		prior := sha256.Sum256([]byte(fmt.Sprintf("known-before-freeze/%d", i)))
		variant := int(prior[0])
		request := RequestID("attacker-payer", uint64(i), []byte(fmt.Sprintf("commitment/%d", variant)))
		h := &Hold{RequestID: request, FreezeHeight: 1, Node: "executor"}
		for j := int64(2); j <= 4; j++ {
			b := sha256.Sum256([]byte(fmt.Sprintf("future/%d/%d", i, j)))
			s.BlockHashes[j] = b[:]
		}
		id, seed, err := s.drawAuditor(h, pool)
		if err != nil {
			t.Fatal(err)
		}
		if id < "auditor-30" {
			hits++
		}
		// Post-freeze freely selected data variants cannot change a draw. The
		// invariant is exact, so all 256 variants share a single seed evaluation.
		h.ReceiptRef = hex.EncodeToString(prior[:])
		check, _, err := s.assignmentSeed(h)
		if err != nil || !bytes.Equal(check, seed) {
			t.Fatal("grinding changed seed")
		}
	}
	rate := 100 * float64(hits) / requests
	t.Logf("requests=%d pool=100 attacker=30 baseline=30.000%% observed=%.3f%% hits=%d delta_pp=%.3f commitment_variants=256", requests, rate, hits, rate-30)
	if rate > 30.5 {
		t.Fatal("self-assignment exceeds baseline + 0.5 percentage points")
	}
}
