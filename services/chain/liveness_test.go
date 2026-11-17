package chain

import (
	"bytes"
	"context"
	core "ember/core-go"
	pb "ember/proto"
	"encoding/json"
	"fmt"
	abci "github.com/cometbft/cometbft/abci/types"
	"google.golang.org/protobuf/proto"
	"testing"
)

func liveFixture(t *testing.T) (*State, map[string]actor) {
	s, a := testState(t)
	s.Parameters.RegisteredAuditors = true
	s.Parameters.RegistrationTTL = 20000
	s.Parameters.HoldTimeout = 20000
	s.Parameters.Assignment.CooldownRequests = 100000
	p := s.ActiveProfile(a["node"].id, 0)
	delete(s.Nodes, a["node"].id) // Replace the generic legacy fixture with fresh signed registrations.
	p.ActualDevice = "cpu"
	p.TorchVersion = "2.5.1+cpu"
	p.EngineCapabilities = &pb.EngineCapabilities{EngineName: "transformers", EngineVersion: "4.45.2", Roles: []string{"model", "auditor"}, ForcedPrefixReplay: true, ScoreKind: "raw-logits", MaxTopK: 16, EvidenceLevels: []string{"full"}}
	for _, name := range []string{"node", "auditor", "auditor2", "ghost", "extra", "extra2"} {
		if _, ok := a[name]; !ok {
			a[name] = newActor(byte(20 + len(a)))
			s.Accounts[a[name].id] = &Account{Balance: 1000, Stake: 5000}
		}
		s.Accounts[a[name].id].Stake = 0
		s.Accounts[a[name].id].Balance = 6000
		if name == "node" {
			s.Accounts[a[name].id].Balance = 5000
		}
		apply(t, s, a[name], s.Accounts[a[name].id].Nonce+1, Action{Type: "stake", Amount: 5000}, 2)
		q := proto.Clone(p).(*pb.ResourceProfile)
		q.NodeId = a[name].id
		if err := core.Sign(q, a[name].key); err != nil {
			t.Fatal(err)
		}
		raw, _ := proto.Marshal(q)
		// Use real self-signed register_node transactions (not fabricated registrations).
		apply(t, s, a[name], s.Accounts[a[name].id].Nonce+1, Action{Type: "register_node", Profile: raw, ExpiresHeight: 19000}, 2)
	}
	return s, a
}
func liveDraw(t *testing.T, s *State, a map[string]actor, n int) AssignmentRecord {
	t.Helper()
	h := &Hold{ID: fmt.Sprint(n), RequestID: RequestID("payer", uint64(n), []byte("live")), Node: a["node"].id, FreezeHeight: 1}
	if _, _, err := s.selectAuditor(h, "", s.Height+1); err != nil {
		t.Fatal(err)
	}
	return h.Assignments[0]
}
func checkLiveRecords(t *testing.T, records []AssignmentRecord) {
	t.Helper()
	for _, r := range records {
		raw, _ := json.Marshal(r)
		winner, err := RecomputeAssignment(r)
		after, _ := json.Marshal(r)
		if err != nil || len(r.AuditorIDs) != 1 || winner != r.AuditorIDs[0] || !bytes.Equal(raw, after) {
			t.Fatalf("recompute: %s %v", winner, err)
		}
	}
	t.Logf("recomputed=%d consistent=%d (100%%)", len(records), len(records))
}
func TestLivenessGhostAndFrozenMask(t *testing.T) {
	s, a := liveFixture(t)
	// Only ghost stops proving; every control is refreshed with signed transactions.
	for _, name := range []string{"auditor", "auditor2", "extra", "extra2"} {
		apply(t, s, a[name], s.Accounts[a[name].id].Nonce+1, Action{Type: "prove_liveness", Node: a[name].id}, 962)
	}
	s.Height = 963
	records := []AssignmentRecord{}
	hits := map[string]int{}
	controlGhost := 0
	for i := 0; i < 500; i++ {
		r := liveDraw(t, s, a, i)
		records = append(records, r)
		hits[r.AuditorIDs[0]]++
		// Exact same pool/seed, disable only online mask for the control experiment.
		control := r
		control.EligibleMask = make([]bool, len(r.Pool))
		for j := range control.EligibleMask {
			control.EligibleMask[j] = true
		}
		w, err := RecomputeAssignment(control)
		if err != nil {
			t.Fatal(err)
		}
		if w == a["ghost"].id {
			controlGhost++
		}
	}
	if hits[a["ghost"].id] != 0 || controlGhost == 0 || s.Nodes[a["ghost"].id].ExpiresHeight <= s.Height {
		t.Fatal("ghost not excluded by liveness alone")
	}
	for _, name := range []string{"auditor", "auditor2", "extra", "extra2"} {
		if hits[a[name].id] == 0 {
			t.Fatal("control never selected")
		}
		t.Logf("%s hits=%d", name, hits[a[name].id])
	}
	before, _ := json.Marshal(records)
	s.Height = 5000
	apply(t, s, a["ghost"], s.Accounts[a["ghost"].id].Nonce+1, Action{Type: "prove_liveness", Node: a["ghost"].id}, 5000)
	checkLiveRecords(t, records)
	after, _ := json.Marshal(records)
	if !bytes.Equal(before, after) {
		t.Fatal("historical eligibility changed")
	}
	t.Logf("ghost draws=0/500 disabled-liveness-control=%d/500 historical-recompute-height=%d", controlGhost, s.Height)
}
func TestLivenessJitterAndProofDiscipline(t *testing.T) {
	s, a := liveFixture(t)
	id := a["auditor"].id
	records := []AssignmentRecord{}
	for h := int64(2); h <= 482; h++ {
		s.Height = h
		if !s.livenessEligible(id) {
			t.Fatal("W/2 dropout")
		}
	}
	apply(t, s, a["auditor"], s.Accounts[a["auditor"].id].Nonce+1, Action{Type: "prove_liveness", Node: id}, 482)
	before, _ := s.Bytes()
	raw, _ := Sign(s.Parameters.ChainID, a["auditor"].key, s.Accounts[a["auditor"].id].Nonce+1, Action{Type: "prove_liveness", Node: id})
	if _, err := s.Apply(raw, 541); err == nil {
		t.Fatal("frequency accepted")
	}
	after, _ := s.Bytes()
	if !bytes.Equal(before, after) {
		t.Fatal("rejection mutated state")
	}
	raw, _ = Sign(s.Parameters.ChainID, a["auditor2"].key, s.Accounts[a["auditor2"].id].Nonce+1, Action{Type: "prove_liveness", Node: id})
	if _, err := s.Apply(raw, 542); err == nil {
		t.Fatal("third-party proof accepted")
	}
	s.Height = 1442
	if !s.livenessEligible(id) {
		t.Fatal("inclusive W rejected")
	}
	s.Height = 1443
	if s.livenessEligible(id) {
		t.Fatal("expired proof eligible")
	}
	// Freeze old finalized snapshot, then prove in the current block.
	snapshot, _ := s.Clone()
	s.finalized = snapshot
	apply(t, s, a["auditor"], s.Accounts[a["auditor"].id].Nonce+1, Action{Type: "prove_liveness", Node: id}, 1444)
	if s.livenessEligible(id) {
		t.Fatal("unfinalized proof leaked")
	}
	s.finalized = nil
	if !s.livenessEligible(id) {
		t.Fatal("next finalized snapshot not restored")
	}
	for _, name := range []string{"auditor2", "extra", "extra2"} {
		apply(t, s, a[name], s.Accounts[a[name].id].Nonce+1, Action{Type: "prove_liveness", Node: a[name].id}, 1444)
	}
	records = append(records, liveDraw(t, s, a, 800))
	checkLiveRecords(t, records)
	t.Log("W/2 outage=480 blocks no dropout; expiry=961 blocks; recovery latency=1 finalized snapshot/block")
}
func TestLivenessSilentAudit(t *testing.T) {
	for _, normal := range []bool{false, true} {
		t.Run(fmt.Sprint(normal), func(t *testing.T) {
			s, a := liveFixture(t)
			id := "silent"
			apply(t, s, a["payer"], 1, Action{Type: "freeze", HoldID: id, Payer: a["payer"].id, Node: a["node"].id, Amount: 100}, 3)
			receipt := signedReceipt(t, id, a["node"])
			apply(t, s, a["coordinator"], 1, Action{Type: "submit", HoldID: id, Actual: 4, Receipt: receipt}, 4)
			apply(t, s, a["coordinator"], 2, Action{Type: "assign_audit", HoldID: id}, 5)
			h := s.Holds[id]
			start := h.AuditAssignedHeight
			for round := 0; round < 4; round++ {
				old := h.Auditor
				at := h.AuditAssignedHeight + s.Parameters.Liveness.AuditTimeout
				raw, _ := Sign(s.Parameters.ChainID, a["payer"].key, s.Accounts[a["payer"].id].Nonce+1, Action{Type: "reassign_audit", HoldID: id})
				if _, err := s.Apply(raw, at); err == nil {
					t.Fatal("timeout boundary allowed")
				}
				// Honest controls keep proving while selected auditor remains silent.
				for name, actor := range a {
					if s.Nodes[actor.id] != nil && actor.id != old && at+1-s.Nodes[actor.id].LastProofHeight >= 60 {
						apply(t, s, actor, s.Accounts[actor.id].Nonce+1, Action{Type: "prove_liveness", Node: actor.id}, at+1)
						_ = name
					}
				}
				if round == 0 {
					left, _ := s.Clone()
					right, _ := s.Clone()
					apply(t, left, a["payer"], left.Accounts[a["payer"].id].Nonce+1, Action{Type: "reassign_audit", HoldID: id}, at+1)
					apply(t, right, a["governor"], right.Accounts[a["governor"].id].Nonce+1, Action{Type: "reassign_audit", HoldID: id}, at+1)
					checkLiveRecords(t, left.Holds[id].Assignments)
					checkLiveRecords(t, right.Holds[id].Assignments)
					l, _ := json.Marshal(left.Holds[id])
					r, _ := json.Marshal(right.Holds[id])
					if !bytes.Equal(l, r) {
						t.Fatal("caller changed reassignment")
					}
					if !bytes.Equal(left.Holds[id].Assignments[0].Seed, left.Holds[id].Assignments[1].Seed) {
						t.Fatal("redraw changed seed")
					}
				}
				apply(t, s, a["payer"], s.Accounts[a["payer"].id].Nonce+1, Action{Type: "reassign_audit", HoldID: id}, at+1)
				h = s.Holds[id]
				if h.AuditVerdict == "service_failure" {
					break
				}
				// Lateness must fail the current-auditor guard, before signature payload validation.
				for _, actor := range a {
					if actor.id == old {
						rejected(t, s, actor, Action{Type: "audit", HoldID: id}, at+2)
					}
				}
				if normal {
					auditor, proof := assignedActor(t, s, a, id)
					c, v, _ := signedAudit(t, receipt, auditor, "accept", proof)
					apply(t, s, auditor, s.Accounts[auditor.id].Nonce+1, Action{Type: "audit", HoldID: id, Challenge: c, Verdict: v}, at+2)
					h = s.Holds[id]
					break
				}
			}
			outcome := "service_failure"
			wantPayer, wantNode := int64(1000), int64(0)
			if normal {
				outcome = "accept"
				wantPayer, wantNode = 996, 4
			}
			if !normal && h.AuditReassignments != 3 {
				t.Fatal("did not reach reassignment cap")
			}
			if h.AuditVerdict != outcome {
				t.Fatalf("outcome=%s", h.AuditVerdict)
			}
			apply(t, s, a["payer"], s.Accounts[a["payer"].id].Nonce+1, Action{Type: "settle", HoldID: id, Outcome: outcome}, s.Height+1)
			if s.Accounts[a["payer"].id].Balance != wantPayer || s.Accounts[a["node"].id].Balance != wantNode {
				t.Fatal("wrong ledger")
			}
			rejected(t, s, a["payer"], Action{Type: "settle", HoldID: id, Outcome: outcome}, s.Height+1)
			if s.Accounts[a["payer"].id].Balance != wantPayer {
				t.Fatal("double refund")
			}
			h = s.Holds[id]
			checkLiveRecords(t, h.Assignments)
			t.Logf("normal=%v reassignments=%d assignments=%d total-blocks=%d payer=%d executor=%d", normal, h.AuditReassignments, len(h.Assignments), s.Height-start, wantPayer, wantNode)
		})
	}
}
func TestLivenessMigrationActivation(t *testing.T) {
	s, a := testState(t)
	s.Version = 5
	s.Parameters.Liveness = LivenessParameters{}
	s.Height = 10
	raw, _ := s.Bytes()
	if bytes.Contains(raw, []byte("audit_equality")) || bytes.Contains(raw, []byte("liveness")) || bytes.Contains(raw, []byte("last_proof_height")) {
		t.Fatal("legacy bytes changed")
	}
	app := &Application{committed: s}
	if err := app.ConfigureMembershipUpgrade(MembershipUpgrade{ChainID: s.Parameters.ChainID, ActivationHeight: 12}); err != nil {
		t.Fatal(err)
	}
	for _, height := range []int64{11, 12} {
		r, err := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: height})
		if err != nil {
			t.Fatal(err)
		}
		if height == 11 && (app.staged.Version != 5 || app.staged.Parameters.AuditEquality.Enabled) {
			t.Fatal("early activation")
		}
		if height == 12 && (app.staged.Version != 6 || app.staged.Parameters.AuditEquality != (AuditEqualityParameters{true, 1, 3}) || r.ConsensusParamUpdates.Version.App != 6) {
			t.Fatal("missing activation")
		}
		if _, err = app.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	if app.committed.Nodes[a["node"].id].LastProofHeight != 12 || app.committed.Parameters.Liveness.ProofInterval != 240 {
		t.Fatal("migration defaults")
	}
	if app.committed.Parameters.RegistrationTTL != 100 {
		t.Fatal("migration changed registration TTL")
	}
	path := t.TempDir() + "/state.json"
	if err := SaveState(path, app.committed); err != nil {
		t.Fatal(err)
	}
	restarted, err := LoadState(path)
	if err != nil || restarted.Parameters.AuditEquality != (AuditEqualityParameters{true, 1, 3}) || restarted.Nodes[a["node"].id].LastProofHeight != 12 {
		t.Fatal("migration not persisted", err)
	}
	checkLiveRecords(t, []AssignmentRecord{liveDraw(t, app.committed, a, 900)})
}

// A deterministic, no-wall-clock load harness: 100 identities staggered over
// 240 heights, two complete cycles. Transactions still use real envelope checks.
func TestLivenessProofLoad(t *testing.T) {
	s, _ := testState(t)
	actors := make([]actor, 100)
	for i := range actors {
		actors[i] = newActor(byte(80 + i))
		id := actors[i].id
		s.Accounts[id] = &Account{}
		s.Nodes[id] = &NodeRegistration{NodeID: id, RegisteredAt: 1, LastProofHeight: 1, ExpiresHeight: 10000}
	}
	before, _ := s.Bytes()
	proofs, maxPerBlock := 0, 0
	for height := int64(241); height <= 720; height++ {
		count := 0
		for i, a := range actors {
			if (height-241)%240 == int64(i) {
				apply(t, s, a, s.Accounts[a.id].Nonce+1, Action{Type: "prove_liveness", Node: a.id}, height)
				proofs++
				count++
			}
		}
		if count > maxPerBlock {
			maxPerBlock = count
		}
	}
	after, _ := s.Bytes()
	if proofs != 200 || maxPerBlock != 1 {
		t.Fatal("unexpected throughput")
	}
	for _, r := range s.Nodes {
		if r.LastProofHeight > 0 {
			r.LastProofHeight = 0
		}
	}
	without, _ := s.Bytes()
	t.Logf("nodes=100 blocks=480 accepted=%d proofs/block=%.6f peak/block=%d state-before=%d state-after=%d growth=%d serialized-last-height-overhead=%d", proofs, float64(proofs)/480, maxPerBlock, len(before), len(after), len(after)-len(before), len(after)-len(without))
}

func TestLivenessEmptyPoolAndEntropyErrors(t *testing.T) {
	s, a := testState(t)
	h := &Hold{ID: "empty", Node: a["node"].id, RequestID: RequestID("payer", 1, []byte("empty")), FreezeHeight: 1, State: "dispute", Auditor: a["auditor"].id, AuditAssignedHeight: 5}
	// Missing entropy is a failed transaction, never a false service_failure.
	delete(s.BlockHashes, 2)
	if err := s.reassignAudit(h, 306); err == nil || h.AuditVerdict != "" {
		t.Fatal("entropy failure treated as pool exhaustion")
	}
	s, _ = testState(t)
	s.Auditors = map[string]bool{}
	if err := s.reassignAudit(h, 306); err != nil || h.AuditVerdict != "service_failure" {
		t.Fatal("empty pool did not fail service")
	}
}
