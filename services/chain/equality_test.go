package chain

import (
	core "ember/core-go"
	pb "ember/proto"
	"encoding/json"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestEqualityEligibility(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, mismatch := range []bool{false, true} {
			for _, change := range []string{"device", "runtime", "version", "score", "all"} {
				s, a := liveFixture(t)
				s.Parameters.AuditEquality = AuditEqualityParameters{Enabled: enabled, Epsilon: 1, MaxRounds: 3}
				p := s.ActiveProfile(a["auditor"].id, 0)
				if change == "device" || change == "all" {
					p.ActualDevice = "cpu"
					producer := s.ActiveProfile(a["node"].id, 0)
					producer.ActualDevice = "cuda"
					core.Sign(producer, a["node"].key)
					s.Nodes[a["node"].id].Profile, _ = proto.Marshal(producer)
				}
				if change == "runtime" || change == "all" {
					p.EngineCapabilities.EngineVersion = "other"
				}
				if change == "version" || change == "all" {
					p.ExecutionProfileVer = "other"
				}
				if change == "score" || change == "all" {
					p.EngineCapabilities.ScoreKind = "other"
				}
				if mismatch {
					p.ManifestRoot = []byte("other")
				}
				core.Sign(p, a["auditor"].key)
				s.Nodes[a["auditor"].id].Profile, _ = proto.Marshal(p)
				got := s.EligibleAuditor(a["auditor"].id, a["node"].id, 0)
				if got != (enabled && !mismatch) {
					t.Fatalf("%v %v %s: %v", enabled, mismatch, change, got)
				}
				t.Logf("enabled=%v manifestMismatch=%v change=%s eligible=%v", enabled, mismatch, change, got)
			}
		}
	}
}
func TestEqualityLadder(t *testing.T) {
	cases := []struct {
		seq  []string
		want string
	}{{[]string{"reject", "reject"}, "reject"}, {[]string{"reject", "accept"}, "accept"}, {[]string{"inconclusive", "accept"}, "accept"}, {[]string{"inconclusive", "reject", "reject"}, "reject"}, {[]string{"inconclusive", "inconclusive", "inconclusive"}, "service_failure"}, {[]string{"inconclusive", "inconclusive", "reject"}, "service_failure"}, {[]string{"accept"}, "accept"}, {[]string{"inconclusive", "inconclusive", "accept"}, "accept"}}
	for _, c := range cases {
		s, a := setupMisjudge(t)
		s.Parameters.AuditEquality = AuditEqualityParameters{true, 1, 3}
		h := &Hold{ID: "ladder", Node: a["node"].id, State: "dispute", AuditRound: 1, Auditor: a["auditor"].id}
		for _, v := range c.seq {
			if err := s.recordAuditOutcome(h, h.Auditor, v, "evidence", 10); err != nil {
				t.Fatal(err)
			}
		}
		if h.AuditVerdict != c.want {
			t.Fatalf("%v got %s", c.seq, h.AuditVerdict)
		}
		t.Logf("%v => %s rounds=%d", c.seq, h.AuditVerdict, h.AuditRound)
	}
}
func TestEqualitySettlement(t *testing.T) {
	for _, mode := range []string{"off", "refund_only", "gray_gate", "full"} {
		for _, class := range []string{"objective", "token"} {
			s, a := setupMisjudge(t)
			s.Parameters.AuditEquality = AuditEqualityParameters{true, 1, 3}
			s.Parameters.Misjudge.SlashMode = mode
			g := &pb.OffenseGrade{Class: class, Fault: "hash"}
			if class == "token" {
				g.Fault = ""
				g.Band = "token-equality-v1"
				g.Score = 2
			}
			h := gradedRequest(t, s, a, "eq", 1, g)
			r := s.OffenseRecords[len(s.OffenseRecords)-1]
			want := "slash"
			if mode == "off" {
				want = "off"
			}
			if r.Status != want || (mode != "off" && (len(h.Slashes) != 1 || h.Slashes[0].Amount != 50)) {
				t.Fatalf("%s %s %+v", mode, class, r)
			}
			t.Logf("mode=%s class=%s status=%s slashes=%v", mode, class, r.Status, h.Slashes)
		}
	}
}
func TestEqualityMigration(t *testing.T) {
	s, _ := testState(t)
	s.Version = 5
	s.Parameters.Liveness = LivenessParameters{}
	s.Parameters.AuditEquality = AuditEqualityParameters{}
	before, _ := json.Marshal(s.Parameters)
	var m map[string]any
	json.Unmarshal(before, &m)
	if _, ok := m["audit_equality"]; ok {
		t.Fatal("preactivation serialization drift")
	}
	s.migrateLiveness(12)
	if s.Version != 6 || s.Parameters.Liveness.ProofInterval != 240 || s.Parameters.AuditEquality != (AuditEqualityParameters{true, 1, 3}) {
		t.Fatal("migration")
	}
}

func TestEqualitySignedLateReject(t *testing.T) {
	s, a := setupMisjudge(t)
	a["third"] = newActor(33)
	s.Accounts[a["third"].id] = &Account{}
	s.Auditors[a["third"].id] = true
	s.Parameters.AuditEquality = AuditEqualityParameters{true, 1, 3}
	apply(t, s, a["payer"], 1, Action{Type: "freeze", HoldID: "late", Payer: a["payer"].id, Node: a["node"].id, Amount: 5}, 1)
	receipt := signedReceipt(t, "late", a["node"])
	apply(t, s, a["coordinator"], 1, Action{Type: "submit", HoldID: "late", Actual: 1, Receipt: receipt}, 2)
	apply(t, s, a["coordinator"], 2, Action{Type: "assign_audit", HoldID: "late"}, 3)
	for i, outcome := range []string{"inconclusive", "reject", "reject"} {
		auditor, proof := assignedActor(t, s, a, "late")
		challenge, raw, _ := signedAudit(t, receipt, auditor, outcome, proof)
		if outcome == "reject" {
			v := &pb.AuditVerdict{}
			proto.Unmarshal(raw, v)
			v.Offense = &pb.OffenseGrade{Class: "token", Band: "token-equality-v1", Score: float64(4 - i)}
			core.Sign(v, auditor.key)
			raw, _ = proto.Marshal(v)
		}
		apply(t, s, auditor, s.Accounts[auditor.id].Nonce+1, Action{Type: "audit", HoldID: "late", Challenge: challenge, Verdict: raw}, int64(4+i))
	}
	h := s.Holds["late"]
	if h.AuditVerdict != "reject" || h.FirstGrade.Score != 3 {
		t.Fatal("late first grade")
	}
	apply(t, s, a["coordinator"], 3, Action{Type: "settle", HoldID: "late", Outcome: "reject"}, 8)
	if len(s.OffenseRecords) != 1 || s.OffenseRecords[0].Score != 2 || s.OffenseRecords[0].Status != "slash" {
		t.Fatal(s.OffenseRecords)
	}
}

func TestEqualitySnapshotAndValidation(t *testing.T) {
	s, _ := testState(t)
	before, err := s.Clone()
	if err != nil {
		t.Fatal(err)
	}
	s.migrateLiveness(12)
	s.finalized = before
	if s.finalizedState().Parameters.AuditEquality.Enabled {
		t.Fatal("old finalized snapshot changed")
	}
	after, err := s.Clone()
	if err != nil {
		t.Fatal(err)
	}
	s.finalized = after
	if s.finalizedState().Parameters.AuditEquality != (AuditEqualityParameters{true, 1, 3}) {
		t.Fatal("snapshot lost parameters")
	}
	for _, p := range []AuditEqualityParameters{{true, -1, 3}, {true, 1, 1}, {true, 1, 5}} {
		if p.valid() {
			t.Fatal("invalid parameters accepted", p)
		}
	}
	for _, g := range []*pb.OffenseGrade{{Class: "token", Score: -1, Band: "token-equality-v1"}, {Class: "token", Band: "wrong"}, {Class: "token", Band: "token-equality-v1", Fault: "hash"}, {Class: "token", Band: "token-equality-v1", LengthClass: "short"}} {
		raw, _ := proto.Marshal(&pb.AuditVerdict{Offense: g})
		if _, err := s.validateGrade(&Hold{}, raw); err == nil {
			t.Fatal("invalid token grade accepted")
		}
	}
}
