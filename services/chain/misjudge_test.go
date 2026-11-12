package chain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

func misjudgeFixture(t *testing.T) ThresholdPair {
	t.Helper()
	raw, err := os.ReadFile("testdata/misjudge_placeholder.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Threshold ThresholdPair `json:"threshold"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture.Threshold
}
func gradedRequest(t *testing.T, s *State, a map[string]actor, id string, height int64, grade *pb.OffenseGrade) *Hold {
	t.Helper()
	apply(t, s, a["payer"], s.Accounts[a["payer"].id].Nonce+1, Action{Type: "freeze", HoldID: id, Payer: a["payer"].id, Node: a["node"].id, Amount: 5}, height)
	receipt := signedReceipt(t, id, a["node"])
	apply(t, s, a["coordinator"], s.Accounts[a["coordinator"].id].Nonce+1, Action{Type: "submit", HoldID: id, Actual: 1, Receipt: receipt}, height+1)
	apply(t, s, a["coordinator"], s.Accounts[a["coordinator"].id].Nonce+1, Action{Type: "assign_audit", HoldID: id}, height+2)
	for round := int64(0); round < 2; round++ {
		auditor, proof := assignedActor(t, s, a, id)
		challenge, raw, _ := signedAudit(t, receipt, auditor, "reject", proof)
		v := &pb.AuditVerdict{}
		proto.Unmarshal(raw, v)
		v.Offense = proto.Clone(grade).(*pb.OffenseGrade)
		if grade.Class == "numerical" {
			v.Distances = []float64{grade.Score}
		}
		if err := core.Sign(v, auditor.key); err != nil {
			t.Fatal(err)
		}
		raw, _ = proto.Marshal(v)
		apply(t, s, auditor, s.Accounts[auditor.id].Nonce+1, Action{Type: "audit", HoldID: id, Challenge: challenge, Verdict: raw}, height+3+round)
	}
	apply(t, s, a["coordinator"], s.Accounts[a["coordinator"].id].Nonce+1, Action{Type: "settle", HoldID: id, Outcome: "reject"}, height+5)
	return s.Holds[id]
}
func numericGrade(score float64) *pb.OffenseGrade {
	return &pb.OffenseGrade{Class: "numerical", Score: score, Band: "gb10-to-thor-test", LengthClass: "short"}
}
func setupMisjudge(t *testing.T) (*State, map[string]actor) {
	s, a := testState(t)
	s.Parameters.Misjudge.Thresholds = []ThresholdPair{misjudgeFixture(t)}
	s.Nodes[a["node"].id].ExpiresHeight = 10000
	for i := int64(1); i < 10000; i++ {
		hash := sha256.Sum256([]byte(fmt.Sprint(i)))
		s.BlockHashes[i] = hash[:]
	}
	return s, a
}
func TestMisjudgePerturbationInjection(t *testing.T) {
	s, a := setupMisjudge(t)
	raw, err := os.ReadFile("testdata/misjudge_empirical.json")
	if err != nil {
		t.Fatal(err)
	}
	var empirical struct {
		Scores []float64 `json:"scores"`
	}
	if err := json.Unmarshal(raw, &empirical); err != nil {
		t.Fatal(err)
	}
	scores := empirical.Scores
	for i, score := range scores {
		h := gradedRequest(t, s, a, fmt.Sprint(i), int64(i*6+1), numericGrade(score))
		if len(h.Slashes) != 0 {
			t.Fatal("honest perturbation slashed", i, score)
		}
	}
	if s.Accounts[a["node"].id].Stake != 5000 || s.Accounts[a["payer"].id].Balance != 1000 {
		t.Fatal("refund/stake conservation")
	}
	h := gradedRequest(t, s, a, "objective", int64(len(scores)*6+1), &pb.OffenseGrade{Class: "objective", Fault: "hash"})
	if len(h.Slashes) != 1 || h.Slashes[0].Amount != 50 {
		t.Fatal("objective fault did not slash", h)
	}
	t.Logf("measured empirical cross-hardware-class injections=%d numerical slashes=0 objective slashes=1 objective amount=%d gray observations=%d (5 measured request rows + 2 historical Thor group maxima)", len(scores), h.Slashes[0].Amount, s.GrayObservations[a["node"].id])
}
func TestMisjudgeModesAndGovernance(t *testing.T) {
	for _, mode := range []string{"off", "refund_only", "gray_gate", "full"} {
		for _, class := range []string{"numerical", "objective"} {
			t.Run(mode+"/"+class, func(t *testing.T) {
				s, a := setupMisjudge(t)
				p := s.Parameters.Misjudge
				p.SlashMode = mode
				rejected(t, s, a["payer"], Action{Type: "set_misjudge", Misjudge: &p}, 1)
				apply(t, s, a["governor"], 1, Action{Type: "set_misjudge", Misjudge: &p}, 1)
				g := numericGrade(.45)
				if class == "objective" {
					g = &pb.OffenseGrade{Class: "objective", Fault: "format"}
				}
				h := gradedRequest(t, s, a, "mode", 2, g)
				want := mode != "off" && (class == "objective" || mode == "full")
				if (len(h.Slashes) > 0) != want {
					t.Fatal("mode semantics", mode, class, h.Slashes)
				}
				if !want && s.Accounts[a["payer"].id].Balance != 1000 {
					t.Fatal("missing refund")
				}
				if len(s.OffenseRecords) != 1 || s.OffenseRecords[0].Version != 2 {
					t.Fatal("missing offense v2")
				}
			})
		}
	}
	s, a := setupMisjudge(t)
	for i, score := range []float64{.4, .45, .499999, .6} {
		h := gradedRequest(t, s, a, fmt.Sprint(i), int64(i*6+1), numericGrade(score))
		want := "cancelled"
		if i == 3 {
			want = "pending-review"
		}
		if h.State != want || len(h.Slashes) != 0 {
			t.Fatal("gray repeat safety", i, h.State)
		}
	}
	if s.GrayObservations[a["node"].id] != 4 {
		t.Fatal("gray observations")
	}
	h := gradedRequest(t, s, a, "expired", 1100, numericGrade(.5))
	if len(h.Slashes) != 1 {
		t.Fatal("window boundary/hard threshold")
	}
	// Missing class, NaN/unknown policy, and N_in objective claims fail closed.
	hold := s.Holds["expired"]
	for _, g := range []*pb.OffenseGrade{nil, {Class: "objective", Fault: "n_in"}, {Class: "numerical", Score: .6, Band: "unknown", LengthClass: "short"}} {
		raw, _ := proto.Marshal(&pb.AuditVerdict{Offense: g})
		if _, err := s.validateGrade(hold, raw); err == nil {
			t.Fatal("ungraded/unknown fault accepted", g)
		}
	}
}
func TestMisjudgeSameBlockCapAndMergedLadder(t *testing.T) {
	s, a := testState(t)
	node := a["node"].id
	s.Accounts[node].Stake = 10001
	s.Accounts[a["payer"].id].Balance = 0
	for i := 0; i < 8; i++ {
		h := &Hold{ID: fmt.Sprint(i), Payer: a["payer"].id, Node: node, State: "dispute", AuditVerdict: "reject", Actual: 100}
		h.VerdictBytes, _ = proto.Marshal(&pb.AuditVerdict{Offense: &pb.OffenseGrade{Class: "objective", Fault: "hash"}})
		s.Holds[h.ID] = h
		apply(t, s, a["coordinator"], s.Accounts[a["coordinator"].id].Nonce+1, Action{Type: "settle", HoldID: h.ID, Outcome: "reject"}, 10)
		if s.Holds[h.ID].Slashes[0].Multiplier != 1 {
			t.Fatal("same-block ladder escalated")
		}
	}
	if len(s.Offenses[node]) != 1 || s.BlockSlashes[node].Paid != 500 || s.Accounts[node].Stake != 9501 || s.Accounts[a["payer"].id].Balance != 500 {
		t.Fatal("subunit cap/merge/conservation", s.BlockSlashes[node], s.Offenses[node])
	}
	next := &Hold{Payer: a["payer"].id}
	if err := s.slashOffender(next, node, 0, 11); err != nil {
		t.Fatal(err)
	}
	if next.Slashes[0].Multiplier != 2 || next.Slashes[0].Amount != 192 {
		t.Fatal("next block ladder", next.Slashes)
	}
	// A one-subunit stake cannot have its protected ceil floor rounded to zero.
	s.Accounts[node].Stake = 1
	h := &Hold{Payer: a["payer"].id}
	if err := s.slashOffender(h, node, 0, 12); err != nil || h.Slashes[0].Amount != 1 {
		t.Fatal("floor protection", err, h.Slashes)
	}
	t.Log("stake=10001 cap=500, 8 offenses -> 1 ladder step; next-block multiplier=2 amount=192; stake=1 protected floor=1")
}

func TestMisjudgeSignedGradeAndFrozenThresholds(t *testing.T) {
	s, a := setupMisjudge(t)
	apply(t, s, a["payer"], 1, Action{Type: "freeze", HoldID: "frozen", Payer: a["payer"].id, Node: a["node"].id, Amount: 5}, 1)
	p := s.Parameters.Misjudge
	p.Thresholds = []ThresholdPair{misjudgeFixture(t)}
	p.Thresholds[0].Hard = .9
	apply(t, s, a["governor"], 1, Action{Type: "set_misjudge", Misjudge: &p}, 2)
	if s.Holds["frozen"].MisjudgeThresholds[0].Hard != .5 {
		t.Fatal("governance mutated frozen threshold")
	}
	receipt := signedReceipt(t, "frozen", a["node"])
	apply(t, s, a["coordinator"], 1, Action{Type: "submit", HoldID: "frozen", Actual: 1, Receipt: receipt}, 2)
	apply(t, s, a["coordinator"], 2, Action{Type: "assign_audit", HoldID: "frozen"}, 3)
	auditor, proof := assignedActor(t, s, a, "frozen")
	challenge, raw, _ := signedAudit(t, receipt, auditor, "reject", proof)
	v := &pb.AuditVerdict{}
	proto.Unmarshal(raw, v)
	v.Offense = numericGrade(.45)
	// Changing the signed classification without re-signing must fail at consensus.
	bad, _ := proto.Marshal(v)
	rejected(t, s, auditor, Action{Type: "audit", HoldID: "frozen", Challenge: challenge, Verdict: bad}, 4)
	core.Sign(v, auditor.key)
	raw, _ = proto.Marshal(v)
	apply(t, s, auditor, s.Accounts[auditor.id].Nonce+1, Action{Type: "audit", HoldID: "frozen", Challenge: challenge, Verdict: raw}, 4)
	auditor, proof = assignedActor(t, s, a, "frozen")
	challenge, raw, _ = signedAudit(t, receipt, auditor, "reject", proof)
	// Correctly signed objective metadata still cannot confirm a numerical accusation.
	rejected(t, s, auditor, Action{Type: "audit", HoldID: "frozen", Challenge: challenge, Verdict: raw}, 5)
	proto.Unmarshal(raw, v)
	v.Offense = numericGrade(.45)
	core.Sign(v, auditor.key)
	raw, _ = proto.Marshal(v)
	apply(t, s, auditor, s.Accounts[auditor.id].Nonce+1, Action{Type: "audit", HoldID: "frozen", Challenge: challenge, Verdict: raw}, 5)
	clone, err := s.Clone()
	if err != nil {
		t.Fatal(err)
	}
	apply(t, clone, a["coordinator"], 3, Action{Type: "settle", HoldID: "frozen", Outcome: "reject"}, 6)
	if len(clone.Holds["frozen"].Slashes) != 0 || clone.OffenseRecords[0].ThetaPair.Hard != .5 {
		t.Fatal("persisted policy/grade lost")
	}
}

func TestMisjudgePipelineRefundOnly(t *testing.T) {
	s, a, nodes, auth, receipt := pipelineState(t)
	pair := misjudgeFixture(t)
	pair.Model = receipt.StageReceipts[0].ChainId
	s.Parameters.Misjudge.Thresholds = []ThresholdPair{pair}
	apply(t, s, a["payer"], 1, Action{Type: "freeze_pipeline", HoldID: "units", Payer: a["payer"].id, Amount: 6, TotalLayers: 4, PipelineAuthorizations: auth}, 2)
	raw, _ := proto.Marshal(receipt)
	apply(t, s, a["coordinator"], 1, Action{Type: "submit_pipeline", HoldID: "units", Actual: 6, Receipt: raw}, 3)
	apply(t, s, a["coordinator"], 2, Action{Type: "assign_audit", HoldID: "units"}, 4)
	for round := 0; round < 2; round++ {
		h := s.Holds["units"]
		auditor, proof := assignedActor(t, s, a, "units")
		ref, _ := hex.DecodeString(h.ReceiptRef)
		challenge := &pb.AuditChallenge{ReceiptRef: ref, VrfProof: proof}
		core.Sign(challenge, auditor.key)
		cr, _ := core.Digest(challenge)
		v := &pb.AuditVerdict{ChallengeRef: cr, AuditorId: auditor.id, Verdict: "reject", EvidenceRef: make([]byte, 32), FaultNodeIds: []string{nodes[1].id}, Offense: numericGrade(.45)}
		core.Sign(v, auditor.key)
		craw, _ := proto.Marshal(challenge)
		vraw, _ := proto.Marshal(v)
		apply(t, s, auditor, s.Accounts[auditor.id].Nonce+1, Action{Type: "audit", HoldID: "units", Challenge: craw, Verdict: vraw}, int64(5+round))
	}
	apply(t, s, a["coordinator"], 3, Action{Type: "settle", HoldID: "units", Outcome: "reject"}, 7)
	if len(s.Holds["units"].Slashes) != 0 || s.Accounts[a["payer"].id].Balance != 1000 || len(s.OffenseRecords) != 1 || s.OffenseRecords[0].NodeID != nodes[1].id {
		t.Fatal("pipeline gray refund/attribution")
	}
	for _, node := range nodes {
		if s.Accounts[node.id].Stake != 5000 {
			t.Fatal("pipeline gray slashed")
		}
	}
}
