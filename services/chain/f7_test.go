package chain

import (
	core "ember/core-go"
	pb "ember/proto"
	"encoding/hex"
	"fmt"
	"google.golang.org/protobuf/proto"
	"strings"
	"testing"
)

func TestF7BidirectionalObjectiveOffense(t *testing.T) {
	for _, recomputed := range []uint32{32, 34} {
		t.Run(fmt.Sprint(recomputed), func(t *testing.T) {
			s, a := privacyFixture(t)
			s.Parameters.Misjudge.SlashMode = "refund_only"
			privacyRequest(t, s, a, "f7", 33, 1)
			privacySend(t, s, a["node"], PrivacyAction{Op: "commit", RequestID: "f7", Root: strings.Repeat("b", 64), BundleRef: strings.Repeat("c", 64), InputTokens: 33, OutputTokens: 1, Actual: 69})
			privacySend(t, s, a["coordinator"], PrivacyAction{Op: "assign", RequestID: "f7"})
			grade := &pb.OffenseGrade{Class: "objective", Fault: "n_in", TokenizerVersion: "unit-tokenizer-v1", RecomputedInputTokens: recomputed}
			bad := proto.Clone(grade).(*pb.OffenseGrade)
			bad.TokenizerVersion = "different-version"
			privacyFail(t, s, privacyActor(t, s, a, "f7"), PrivacyAction{Op: "verdict", RequestID: "f7", Verdict: "reject", StatsHash: strings.Repeat("d", 64), Offense: bad})
			bad.TokenizerVersion = grade.TokenizerVersion
			bad.RecomputedInputTokens = 33
			privacyFail(t, s, privacyActor(t, s, a, "f7"), PrivacyAction{Op: "verdict", RequestID: "f7", Verdict: "reject", StatsHash: strings.Repeat("d", 64), Offense: bad})
			first := privacyActor(t, s, a, "f7")
			privacySend(t, s, first, PrivacyAction{Op: "verdict", RequestID: "f7", Verdict: "reject", StatsHash: strings.Repeat("d", 64), Offense: grade})
			second := privacyActor(t, s, a, "f7")
			if first.id == second.id {
				t.Fatal("same auditor confirmed")
			}
			disagree := proto.Clone(grade).(*pb.OffenseGrade)
			disagree.RecomputedInputTokens++
			privacyFail(t, s, second, PrivacyAction{Op: "verdict", RequestID: "f7", Verdict: "reject", StatsHash: strings.Repeat("d", 64), Offense: disagree})
			privacySend(t, s, second, PrivacyAction{Op: "verdict", RequestID: "f7", Verdict: "reject", StatsHash: strings.Repeat("e", 64), Offense: grade})
			stake := s.Accounts[a["node"].id].Stake
			privacySend(t, s, a["payer"], PrivacyAction{Op: "finalize", RequestID: "f7"})
			h := s.Holds["f7"]
			if h.State != "slashed" || s.Accounts[a["node"].id].Stake >= stake || len(s.OffenseRecords) != 1 || s.OffenseRecords[0].Class != "objective" || s.OffenseRecords[0].Fault != "n_in" {
				t.Fatal("bidirectional objective fault did not slash", h, s.OffenseRecords)
			}
		})
	}
}
func TestF7UnavailableRefundWithoutPenalty(t *testing.T) {
	for _, reason := range []string{"artifact-missing", "tokenizer-missing", "version-mismatch"} {
		t.Run(reason, func(t *testing.T) {
			s, a := privacyFixture(t)
			privacyRequest(t, s, a, "unavailable", 33, 1)
			privacySend(t, s, a["node"], PrivacyAction{Op: "commit", RequestID: "unavailable", Root: strings.Repeat("b", 64), BundleRef: strings.Repeat("c", 64), InputTokens: 33, OutputTokens: 1, Actual: 69})
			privacySend(t, s, a["coordinator"], PrivacyAction{Op: "assign", RequestID: "unavailable"})
			stake := s.Accounts[a["node"].id].Stake
			privacySend(t, s, privacyActor(t, s, a, "unavailable"), PrivacyAction{Op: "verdict", RequestID: "unavailable", Verdict: "inconclusive", StatsHash: strings.Repeat("d", 64)})
			privacySend(t, s, a["payer"], PrivacyAction{Op: "finalize", RequestID: "unavailable"})
			if s.Holds["unavailable"].State != "cancelled" || s.Accounts[a["payer"].id].Balance != 1000 || s.Accounts[a["node"].id].Stake != stake || len(s.OffenseRecords) != 0 {
				t.Fatal("unavailable work penalized or not refunded")
			}
		})
	}
}
func TestF7RejectsUnpinnedPricing(t *testing.T) {
	s, a, n, _, _ := pricedFixture(t)
	c := &pb.ExecutionCommitment{}
	proto.Unmarshal(tokenCommitment(t, n[0], 33, 100), c)
	c.TokenizerVersion = ""
	core.Sign(c, n[0].key)
	raw, _ := proto.Marshal(c)
	rejected(t, s, a["payer"], Action{Type: "freeze", HoldID: "units", Payer: a["payer"].id, Node: n[0].id, Amount: 369, PricingCommitment: raw}, 2)
	if len(s.Holds) != 0 || s.Accounts[a["payer"].id].Balance != 1000 {
		t.Fatal("rejected tokenization reserved funds or hold")
	}
}

func TestF7PipelineChargesTokenizerSigner(t *testing.T) {
	s, a, nodes, auth, r := pricedFixture(t)
	s.Parameters.Misjudge.SlashMode = "refund_only"
	s.Accounts[a["coordinator"].id].Stake = 5000
	c := tokenCommitment(t, a["coordinator"], 2, 3)
	apply(t, s, a["payer"], 1, Action{Type: "freeze_pipeline", HoldID: "units", Payer: a["payer"].id, Amount: 13, TotalLayers: 4, PipelineAuthorizations: auth, PricingCommitment: c}, 2)
	raw, _ := proto.Marshal(r)
	apply(t, s, a["coordinator"], 1, Action{Type: "submit_pipeline", HoldID: "units", Actual: 7, Receipt: raw}, 3)
	apply(t, s, a["coordinator"], 2, Action{Type: "assign_audit", HoldID: "units"}, 4)
	for round := 0; round < 2; round++ {
		h := s.Holds["units"]
		auditor, proof := assignedActor(t, s, a, "units")
		ref, _ := hex.DecodeString(h.ReceiptRef)
		challenge := &pb.AuditChallenge{ReceiptRef: ref, VrfProof: proof}
		core.Sign(challenge, auditor.key)
		cr, _ := core.Digest(challenge)
		v := &pb.AuditVerdict{ChallengeRef: cr, AuditorId: auditor.id, Verdict: "reject", EvidenceRef: make([]byte, 32), FaultNodeIds: []string{a["coordinator"].id}, Offense: &pb.OffenseGrade{Class: "objective", Fault: "n_in", TokenizerVersion: "unit-tokenizer-v1", RecomputedInputTokens: 3}}
		core.Sign(v, auditor.key)
		cb, _ := proto.Marshal(challenge)
		vb, _ := proto.Marshal(v)
		apply(t, s, auditor, s.Accounts[auditor.id].Nonce+1, Action{Type: "audit", HoldID: "units", Challenge: cb, Verdict: vb}, int64(5+round))
	}
	apply(t, s, a["coordinator"], 3, Action{Type: "settle", HoldID: "units", Outcome: "reject"}, 7)
	if len(s.OffenseRecords) != 1 || s.OffenseRecords[0].NodeID != a["coordinator"].id || s.Accounts[a["coordinator"].id].Stake >= 5000 {
		t.Fatal("tokenizer signer not penalized")
	}
	for _, node := range nodes {
		if s.Accounts[node.id].Stake != 5000 {
			t.Fatal("unrelated executor penalized")
		}
	}
}
