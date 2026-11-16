package chain

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"testing"

	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

const goldenRecompute = "88e96e597a68286610b962ace54195d153dd36be3486644fdd1417a330ab020d"
const goldenThird = "f0c07b43f9118a55a410d697d9a8bed5cca2e9f9e0e5e00a6f470340f8f45bf6"

func goldenVerdict() *pb.AuditVerdict {
	p := int64(7)
	c, e := make([]byte, 32), make([]byte, 32)
	for i := range c {
		c[i], e[i] = byte(i), byte(i+32)
	}
	return &pb.AuditVerdict{AuditorId: "auditor-α", ChallengeRef: c, EvidenceRef: e, Verdict: "reject", FirstDivergence: &p, Tolerance: "token-equality-v1", NOutBilled: 16, Distances: []float64{0, 1.25, math.Copysign(0, -1)}}
}
func TestEvidenceBindingGoldenVectors(t *testing.T) {
	v := goldenVerdict()
	ref, err := recomputeReference(v)
	if err != nil || hex.EncodeToString(ref) != goldenRecompute {
		t.Fatalf("recompute %x %v", ref, err)
	}
	e := RejectEvidence{Auditor: v.AuditorId, ResultCommitment: repeatHex("ab", 32), ReplayRecord: goldenRecompute, FirstDivergence: v.FirstDivergence, Tolerance: v.Tolerance, Cause: "token::token-equality-v1"}
	if got := thirdAdjudicationHash("reject", e); got != goldenThird {
		t.Fatal(got)
	}
	v.FirstDivergence = nil
	absent, _ := recomputeReference(v)
	if bytes.Equal(absent, ref) {
		t.Fatal("absent divergence was not bound")
	}
}
func repeatHex(s string, n int) string { return string(bytes.Repeat([]byte(s), n)) }

func TestEvidenceBindingFromSignedVerdict(t *testing.T) {
	s, _ := testState(t)
	a := newActor(71)
	base := goldenVerdict()
	base.AuditorId = a.id
	base.Offense = &pb.OffenseGrade{Class: "token", Band: "token-equality-v1"}
	h := &Hold{ReceiptRef: "result", AuditOutputTokens: 16, FirstGrade: base.Offense}
	for _, name := range []string{"valid", "tampered tolerance", "class mismatch", "wrong ref", "out of range", "negative", "nan", "infinity", "legacy", "missing offense", "short ref"} {
		t.Run(name, func(t *testing.T) {
			v := proto.Clone(base).(*pb.AuditVerdict)
			v.RecomputeRef, _ = recomputeReference(v)
			switch name {
			case "tampered tolerance":
				v.Tolerance = "other"
			case "class mismatch":
				v.Tolerance = "other"
				v.RecomputeRef, _ = recomputeReference(v)
			case "wrong ref":
				v.RecomputeRef[0] ^= 1
			case "out of range":
				p := int64(16)
				v.FirstDivergence = &p
				v.RecomputeRef, _ = recomputeReference(v)
			case "negative":
				p := int64(-2)
				v.FirstDivergence = &p
				v.RecomputeRef, _ = recomputeReference(v)
			case "nan":
				v.Distances = []float64{math.NaN()}
			case "infinity":
				v.Distances = []float64{math.Inf(1)}
			case "legacy":
				v.RecomputeRef = nil
				v.FirstDivergence = nil
				v.Tolerance = ""
			case "missing offense":
				v.Offense = nil
			case "short ref":
				v.RecomputeRef = []byte{1}
			}
			if err := core.Sign(v, a.key); err != nil {
				t.Fatal(err)
			}
			if err := core.Verify(v); err != nil {
				t.Fatal(err)
			}
			h.VerdictBytes, _ = proto.Marshal(v)
			e := s.adjudicationEvidence(h, a.id, "reject", "fallback")
			if e.valid() != (name == "valid") {
				t.Fatalf("%s: %+v", name, e)
			}
			if name == "valid" && e.ReplayRecord != hex.EncodeToString(v.RecomputeRef) {
				t.Fatal("wrong replay record")
			}
			if name == "legacy" && e.ReplayRecord != "fallback" {
				t.Fatal("legacy fallback lost")
			}
			if name != "valid" && name != "legacy" && e.ReplayRecord != "" {
				t.Fatal("invalid binding borrowed fallback")
			}
		})
	}
}

// All lifecycle steps use signed transactions, registered nodes and actual draws.
func evidenceDrillStart(t *testing.T, enabled bool) (*State, map[string]actor, []byte) {
	t.Helper()
	s, a := liveFixture(t)
	s.Parameters.Adjudication.Enabled = enabled
	s.Parameters.AuditSampling.Enabled = true
	s.Parameters.AuditSampling.TierRatesPPM = [3]uint32{1000000, 1000000, 1000000}
	id := "adj-tx-drill"
	apply(t, s, a["payer"], s.Accounts[a["payer"].id].Nonce+1, Action{Type: "freeze", HoldID: id, Payer: a["payer"].id, Node: a["node"].id, Amount: 100}, 3)
	receipt := signedReceipt(t, id, a["node"])
	apply(t, s, a["coordinator"], s.Accounts[a["coordinator"].id].Nonce+1, Action{Type: "submit", HoldID: id, Actual: 100, Receipt: receipt}, 4)
	apply(t, s, a["coordinator"], s.Accounts[a["coordinator"].id].Nonce+1, Action{Type: "assign_audit", HoldID: id}, 5)
	if h := s.Holds[id]; h.Sampling == nil || !h.Sampling.Sampled {
		t.Fatal("not sampled")
	}
	return s, a, receipt
}
func evidenceDrillAudit(t *testing.T, s *State, a map[string]actor, receipt []byte, outcome string, bound bool) (actor, []byte, []byte) {
	t.Helper()
	auditor, proof := assignedActor(t, s, a, "adj-tx-drill")
	c, raw, _ := signedAudit(t, receipt, auditor, outcome, proof)
	if bound {
		v := &pb.AuditVerdict{}
		proto.Unmarshal(raw, v)
		v.NOutBilled = 1
		v.Tolerance = "token-equality-v1"
		if outcome == "reject" {
			p := int64(0)
			v.FirstDivergence = &p
			v.Tolerance = "exact"
		}
		v.RecomputeRef, _ = recomputeReference(v)
		if err := core.Sign(v, auditor.key); err != nil {
			t.Fatal(err)
		}
		raw, _ = proto.Marshal(v)
	}
	return auditor, c, raw
}
func evidenceApplyAudit(t *testing.T, s *State, a actor, c, v []byte, height int64) {
	t.Helper()
	apply(t, s, a, s.Accounts[a.id].Nonce+1, Action{Type: "audit", HoldID: "adj-tx-drill", Challenge: c, Verdict: v}, height)
}
func evidenceDrillThird(t *testing.T, thirdOutcome string) (*State, *Hold, int64) {
	t.Helper()
	s, a, receipt := evidenceDrillStart(t, true)
	first, c, v := evidenceDrillAudit(t, s, a, receipt, "reject", true)
	before := s.Accounts[first.id].Balance
	evidenceApplyAudit(t, s, first, c, v, 6)
	second, c, v := evidenceDrillAudit(t, s, a, receipt, "accept", true)
	evidenceApplyAudit(t, s, second, c, v, 7)
	h := s.Holds["adj-tx-drill"]
	if h.AuditRound != 3 || h.Auditor == h.Node || h.Auditor == first.id || h.Auditor == second.id || h.Adjudication.Deposit.From != first.id || s.Accounts[first.id].Balance != before-30 {
		t.Fatalf("no independent escalation: %+v", h)
	}
	third, c, v := evidenceDrillAudit(t, s, a, receipt, thirdOutcome, true)
	planned := *h
	planned.VerdictBytes = v
	e := s.adjudicationEvidence(&planned, third.id, thirdOutcome, "")
	commitment := thirdAdjudicationHash(thirdOutcome, e)
	apply(t, s, third, s.Accounts[third.id].Nonce+1, Action{Type: "commit_third_adjudication", HoldID: h.ID, AdjudicationCommitment: commitment}, 8)
	evidenceApplyAudit(t, s, third, c, v, 9)
	return s, s.Holds[h.ID], before
}
func TestAdjudicationTxDrillEscalationConviction(t *testing.T) {
	s, h, _ := evidenceDrillThird(t, "reject")
	if h.AuditVerdict != "reject" || h.Adjudication.Deposit.Outcome != Upheld {
		t.Fatalf("not convicted: %+v", h.Adjudication)
	}
	payer := s.Accounts[h.Payer].Balance
	pool := s.AuditPool[h.ModelID][10/s.Parameters.AuditWork.EpochBlocks]
	// The coordinator is the same deterministic fixture identity.
	co := newActor(3)
	apply(t, s, co, s.Accounts[co.id].Nonce+1, Action{Type: "settle", HoldID: h.ID, Outcome: "reject"}, 10)
	h = s.Holds[h.ID]
	costs := h.Adjudication.PenaltyCosts
	if h.State != "slashed" || len(h.Slashes) != 1 || costs != 45 || h.Adjudication.PenaltyCostsPaid != costs || s.AuditPool[h.ModelID][10/s.Parameters.AuditWork.EpochBlocks]-pool != costs || s.Accounts[h.Payer].Balance-payer-h.Amount != h.Slashes[0].Amount-costs {
		t.Fatalf("waterfall: %+v", h)
	}
}
func TestAdjudicationTxDrillThirdAcquits(t *testing.T) {
	s, h, before := evidenceDrillThird(t, "accept")
	if h.AuditVerdict != "service_failure" || h.Adjudication.Deposit.Outcome != NotUpheld || !h.Adjudication.Deposit.Settled || s.Accounts[h.FirstAuditor].Balance != before-15 || s.AuditPool[h.ModelID][9/s.Parameters.AuditWork.EpochBlocks] != 15 {
		t.Fatalf("acquittal: %+v", h.Adjudication)
	}
}
func TestAdjudicationUnboundLegacyByteIdentical(t *testing.T) {
	var snapshots [][]byte
	for _, enabled := range []bool{false, true} {
		s, a, receipt := evidenceDrillStart(t, enabled)
		for round := int64(0); round < 2; round++ {
			who, c, v := evidenceDrillAudit(t, s, a, receipt, "reject", false)
			evidenceApplyAudit(t, s, who, c, v, 6+round)
		}
		if s.Holds["adj-tx-drill"].AuditVerdict != "reject" {
			t.Fatal("lost legacy conviction")
		}
		raw, err := json.Marshal([]any{s.Holds, s.Accounts, s.Evidence})
		if err != nil {
			t.Fatal(err)
		}
		snapshots = append(snapshots, raw)
	}
	if !bytes.Equal(snapshots[0], snapshots[1]) {
		t.Fatalf("legacy state differs\n%s\n%s", snapshots[0], snapshots[1])
	}
}
func TestPenaltyCostsPartialConsumption(t *testing.T) {
	s, h := adjFixture(t)
	s.Accounts[h.Node].Stake = 10
	h.Adjudication.PenaltyCosts = 100
	before := s.Accounts[h.Payer].Balance
	if err := s.slashWithPenaltyCosts(h, h.Node, 100, 10); err != nil {
		t.Fatal(err)
	}
	collected := h.Slashes[0].Amount
	if collected <= 0 || h.Adjudication.PenaltyCostsPaid != collected || s.Accounts[h.Payer].Balance != before || h.Adjudication.PenaltyCosts-h.Adjudication.PenaltyCostsPaid <= 0 || s.AuditPool[h.ModelID][10/s.Parameters.AuditWork.EpochBlocks] != collected {
		t.Fatal("partial collection", h.Adjudication)
	}
	for _, hasCase := range []bool{false, true} {
		left, lh := adjFixture(t)
		right, rh := adjFixture(t)
		if hasCase {
			lh.Adjudication.PenaltyCosts = 7
			lh.Adjudication.PenaltyCostsPaid = 7
		} else {
			lh.Adjudication = nil
		}
		if err := left.slashWithPenaltyCosts(lh, lh.Node, 100, 10); err != nil {
			t.Fatal(err)
		}
		if err := right.slashOffender(rh, rh.Node, 100, 10); err != nil {
			t.Fatal(err)
		}
		l, _ := json.Marshal(left.Accounts)
		r, _ := json.Marshal(right.Accounts)
		if !bytes.Equal(l, r) {
			t.Fatal("zero receivable changed balances")
		}
	}
}

func TestAdjudicationMixedBindingsLegacyCorroboration(t *testing.T) {
	s, a, receipt := evidenceDrillStart(t, true)
	first, c, v := evidenceDrillAudit(t, s, a, receipt, "reject", true)
	evidenceApplyAudit(t, s, first, c, v, 6)
	if s.Holds["adj-tx-drill"].Adjudication == nil {
		t.Fatal("bound first review did not start case")
	}
	second, c, v := evidenceDrillAudit(t, s, a, receipt, "reject", false)
	evidenceApplyAudit(t, s, second, c, v, 7)
	h := s.Holds["adj-tx-drill"]
	if h.adjudicationConvicted() || h.AuditVerdict != "reject" || h.AuditRound != 2 {
		t.Fatal("legacy corroboration lost in bound case")
	}
}

func TestPenaltyCostsAlreadySettledAppeal(t *testing.T) {
	s, h := adjFixture(t)
	h.Adjudication.Origin = "consumer_appeal"
	h.Adjudication.DeliveryState = "settled"
	h.Adjudication.PenaltyCosts = 45
	h.FirstGrade = &pb.OffenseGrade{Class: "objective", Fault: "hash"}
	h.VerdictBytes, _ = proto.Marshal(&pb.AuditVerdict{Offense: h.FirstGrade})
	before := s.Accounts[h.Payer].Balance
	if err := s.settleAt(h, "reject", 10); err != nil {
		t.Fatal(err)
	}
	if len(h.Slashes) != 1 || h.Adjudication.PenaltyCostsPaid != 45 || s.Accounts[h.Payer].Balance-before != h.Slashes[0].Amount-45 || s.AuditPool[h.ModelID][10/s.Parameters.AuditWork.EpochBlocks] != 45 {
		t.Fatal("appeal waterfall did not consume costs")
	}
}

func TestPenaltyCostsNoSlashRemainsOutstanding(t *testing.T) {
	for _, mode := range []string{"gray_gate", "refund_only"} {
		t.Run(mode, func(t *testing.T) {
			s, h := adjFixture(t)
			h.Adjudication.Origin = "consumer_appeal"
			h.Adjudication.DeliveryState = "settled"
			h.Adjudication.PenaltyCosts = 45
			s.Parameters.Misjudge.SlashMode = mode
			h.AuditOutputTokens = 1
			h.MisjudgeThresholds = []ThresholdPair{{Model: h.ModelID, Band: "band", LengthClass: "short", MinOutput: 1, MaxOutput: 10, Soft: 0.1, Hard: 0.9}}
			h.FirstGrade = &pb.OffenseGrade{Class: "numerical", Band: "band", LengthClass: "short", Score: 0.5}
			h.VerdictBytes, _ = proto.Marshal(&pb.AuditVerdict{Offense: h.FirstGrade, Distances: []float64{0.5}})
			stake, balance := s.Accounts[h.Node].Stake, s.Accounts[h.Payer].Balance
			if err := s.settleAt(h, "reject", 10); err != nil {
				t.Fatal(err)
			}
			if h.Adjudication.PenaltyCostsPaid != 0 || s.Accounts[h.Node].Stake != stake || s.Accounts[h.Payer].Balance != balance || len(h.Slashes) != 0 || s.AuditPool[h.ModelID][10/s.Parameters.AuditWork.EpochBlocks] != 0 {
				t.Fatal("uncollected receivable consumed")
			}
		})
	}
}

// The same deterministic Python-generated envelopes are pinned by node tests.
// Verify actual Go transaction signing bytes, not just Python's own verifier.
func TestEvidenceBindingPythonEnvelopes(t *testing.T) {
	raw, err := os.ReadFile("testdata/adjudication_envelopes.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]string
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pre_envelope", "envelope"} {
		raw, err = base64.StdEncoding.DecodeString(fixture[name])
		if err != nil {
			t.Fatal(err)
		}
		env, _, err := DecodeAndVerify(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if name == "pre_envelope" {
			if env.Nonce != 41 || env.Action.Type != "commit_third_adjudication" || env.Action.AdjudicationCommitment != goldenThird {
				t.Fatal("wrong commitment envelope")
			}
		} else {
			v := &pb.AuditVerdict{}
			if env.Nonce != 42 || env.Action.Type != "audit" || proto.Unmarshal(env.Action.Verdict, v) != nil || core.Verify(v) != nil {
				t.Fatal("wrong audit envelope")
			}
			ref, err := recomputeReference(v)
			if err != nil || hex.EncodeToString(ref) != goldenRecompute || !bytes.Equal(ref, v.RecomputeRef) {
				t.Fatal("wrong signed recompute")
			}
		}
	}
}
