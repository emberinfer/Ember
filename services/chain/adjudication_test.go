package chain

import (
	"bytes"
	"context"
	pb "ember/proto"
	abci "github.com/cometbft/cometbft/abci/types"
	"google.golang.org/protobuf/proto"
	"reflect"
	"testing"
)

func adjEvidence(a, cause string) RejectEvidence {
	p := int64(0)
	return RejectEvidence{Auditor: a, ResultCommitment: "result", ReplayRecord: "replay-" + a, FirstDivergence: &p, Tolerance: "0.5", Cause: cause}
}
func adjFixture(t *testing.T) (*State, *Hold) {
	s, a := testState(t)
	s.Version = 10
	s.Parameters.Adjudication.defaults()
	s.Parameters.Adjudication.Enabled = true
	h := sampleHold(s, a, 1)
	h.ReceiptRef = "result"
	h.FirstAuditor = a["auditor"].id
	h.FirstVerdict = "reject"
	h.Auditor = a["auditor2"].id
	h.AuditRound = 2
	h.Sampling = &SamplingRecord{Sampled: true}
	h.Adjudication = &AdjudicationCase{FirstAuditor: h.FirstAuditor, SecondAuditor: h.Auditor, SecondCompleted: true, Origin: "sampled_escalation", UnitsUsed: 2, Evidences: []RejectEvidence{adjEvidence(h.FirstAuditor, "cause")}}
	return s, h
}
func TestAdjudicationNotSampled(t *testing.T) {
	s, h := adjFixture(t)
	h.Sampling.Sampled = false
	if s.maybeEscalateAdjudication(h, 10) {
		t.Fatal("escalated")
	}
}
func TestAdjudicationMissingBinding(t *testing.T) {
	s, h := adjFixture(t)
	h.Adjudication.Evidences[0].Tolerance = ""
	if s.maybeEscalateAdjudication(h, 10) {
		t.Fatal("escalated")
	}
}
func TestAdjudicationAlreadyCorroborated(t *testing.T) {
	s, h := adjFixture(t)
	h.Adjudication.Evidences = append(h.Adjudication.Evidences, adjEvidence(h.Auditor, "cause"))
	if s.maybeEscalateAdjudication(h, 10) {
		t.Fatal("escalated")
	}
}
func TestAdjudicationNoFourthQualifiedNode(t *testing.T) {
	s, h := adjFixture(t)
	if s.maybeEscalateAdjudication(h, 10) {
		t.Fatal("escalated")
	}
	if err := s.recordAuditOutcome(h, h.Auditor, "accept", "ref", 10); err != nil || h.AuditVerdict != "service_failure" {
		t.Fatal(err, h.AuditVerdict)
	}
}
func TestAdjudicationDuplicateAuditor(t *testing.T) {
	c := AdjudicationCase{Evidences: []RejectEvidence{adjEvidence("a", "x"), adjEvidence("a", "x")}}
	if c.convicted() {
		t.Fatal("duplicate convicted")
	}
}
func TestAdjudicationDifferentCauses(t *testing.T) {
	c := AdjudicationCase{Evidences: []RejectEvidence{adjEvidence("a", "x"), adjEvidence("b", "y")}}
	if c.convicted() {
		t.Fatal("different causes convicted")
	}
}
func TestAdjudicationDifferentCommitments(t *testing.T) {
	a, b := adjEvidence("a", "x"), adjEvidence("b", "x")
	b.ResultCommitment = "other"
	if (&AdjudicationCase{Evidences: []RejectEvidence{a, b}}).convicted() {
		t.Fatal("different sources convicted")
	}
}
func TestAdjudicationDoubleEvidence(t *testing.T) {
	if !(&AdjudicationCase{Evidences: []RejectEvidence{adjEvidence("a", "x"), adjEvidence("b", "x")}}).convicted() {
		t.Fatal("not convicted")
	}
}
func adjSettlement(t *testing.T, out AdjudicationOutcome, wantRefund int64) {
	s, h := adjFixture(t)
	h.Adjudication.UnitsUsed = 3
	from := h.Payer
	before := s.Accounts[from].Balance
	if err := s.lockAdjudicationDeposit(h, from); err != nil {
		t.Fatal(err)
	}
	if err := s.settleAdjudicationDeposit(h, out, 10); err != nil {
		t.Fatal(err)
	}
	if got := s.Accounts[from].Balance; got != before-30+wantRefund {
		t.Fatal(got)
	}
	if !h.Adjudication.Deposit.Settled {
		t.Fatal("unsettled")
	}
	if out == Upheld && h.Adjudication.PenaltyCosts != 45 {
		t.Fatal("penalty booking")
	}
	if out == Forged && !h.Adjudication.ForgeryPenaltyApplied {
		t.Fatal("penalty hook")
	}
	if out == PathInvalid && s.AuditPool[h.ModelID][10/s.Parameters.AuditWork.EpochBlocks] != 45 {
		t.Fatal("protocol budget")
	}
}
func TestAdjudicationDepositUpheld(t *testing.T)      { adjSettlement(t, Upheld, 30) }
func TestAdjudicationDepositForged(t *testing.T)      { adjSettlement(t, Forged, 0) }
func TestAdjudicationDepositNotUpheld(t *testing.T)   { adjSettlement(t, NotUpheld, 15) }
func TestAdjudicationDepositPathInvalid(t *testing.T) { adjSettlement(t, PathInvalid, 30) }
func TestAdjudicationBudgetCap(t *testing.T) {
	s, h := adjFixture(t)
	h.Adjudication.UnitsUsed = 3
	if s.maybeEscalateAdjudication(h, 10) {
		t.Fatal("budget exceeded")
	}
}
func TestAdjudicationRevealWithoutCommitment(t *testing.T) {
	s, h := adjFixture(t)
	h.Adjudication.ThirdAuditor = "third"
	if s.revealThirdAdjudication(h, "third", "reject", adjEvidence("third", "cause"), 10) == nil {
		t.Fatal("revealed")
	}
}
func TestAdjudicationCommitmentMismatch(t *testing.T) {
	s, h := adjFixture(t)
	h.Adjudication.ThirdAuditor = "third"
	if err := s.commitThirdAdjudication(h, "third", thirdAdjudicationHash("accept", adjEvidence("third", "cause"))); err != nil {
		t.Fatal(err)
	}
	if s.revealThirdAdjudication(h, "third", "reject", adjEvidence("third", "cause"), 10) == nil {
		t.Fatal("mismatch accepted")
	}
}
func TestAdjudicationMigration(t *testing.T) {
	s, _ := testState(t)
	s.Version = 9
	s.Parameters.Adjudication = AdjudicationParameters{}
	before := s.Parameters
	s.migrateAdjudication()
	if s.Version != 10 || !s.Parameters.Adjudication.valid() {
		t.Fatal("defaults")
	}
	before.Adjudication = s.Parameters.Adjudication
	if !reflect.DeepEqual(before, s.Parameters) {
		t.Fatal("existing parameters changed")
	}
}
func TestAdjudicationAppealAfterWindow(t *testing.T) {
	s, h := adjFixture(t)
	h.Adjudication = nil
	h.Sampling = nil
	h.Auditor = ""
	if s.registerConsumerAppeal(h, h.Payer, h.SettleHeight+39269) == nil {
		t.Fatal("late appeal")
	}
}
func TestAdjudicationAppealTierExcluded(t *testing.T) {
	s, h := adjFixture(t)
	h.Adjudication.Origin = "consumer_appeal"
	s.auditStatsEvent(h, AuditStatsEvent{Kind: "settle", Node: h.Node, Height: 10, Actual: s.Parameters.AuditSampling.ProgressMinActualSubunits, Audited: true})
	if s.NodeAuditStats[h.Node].AuditedAccepts != 0 {
		t.Fatal("appeal farmed tier")
	}
	h.Adjudication = nil
	s.auditStatsEvent(h, AuditStatsEvent{Kind: "settle", Node: h.Node, Height: 11, Actual: s.Parameters.AuditSampling.ProgressMinActualSubunits, Audited: true})
	if s.NodeAuditStats[h.Node].AuditedAccepts != 1 {
		t.Fatal("sampled control")
	}
}

func TestAdjudicationAppealWithinWindow(t *testing.T) {
	s, a := liveFixture(t)
	s.Parameters.Adjudication.Enabled = true
	h := sampleHold(s, a, 15)
	h.Sampling = &SamplingRecord{Sampled: false}
	h.Unaudited = true
	before := s.Accounts[h.Payer].Balance
	if err := s.registerConsumerAppeal(h, h.Payer, h.SettleHeight+39268); err != nil {
		t.Fatal(err)
	}
	if s.Accounts[h.Payer].Balance != before-30 || h.Adjudication.UnitsUsed != 2 || h.Adjudication.FirstAuditor == h.Adjudication.SecondAuditor {
		t.Fatal("invalid appeal draw/deposit")
	}
}
func TestAdjudicationThirdSuccessfulReveal(t *testing.T) {
	s, h := adjFixture(t)
	h.Adjudication.ThirdAuditor = "third"
	h.Adjudication.UnitsUsed = 3
	if err := s.lockAdjudicationDeposit(h, h.Payer); err != nil {
		t.Fatal(err)
	}
	e := adjEvidence("third", "cause")
	if err := s.commitThirdAdjudication(h, "third", thirdAdjudicationHash("reject", e)); err != nil {
		t.Fatal(err)
	}
	if err := s.revealThirdAdjudication(h, "third", "reject", e, 10); err != nil {
		t.Fatal(err)
	}
	if h.AuditVerdict != "reject" || h.Adjudication.Deposit.Outcome != Upheld {
		t.Fatal("not upheld")
	}
}
func TestAdjudicationSecondIncomplete(t *testing.T) {
	s, h := adjFixture(t)
	h.Adjudication.SecondCompleted = false
	if s.maybeEscalateAdjudication(h, 10) {
		t.Fatal("premature escalation")
	}
}

func TestAdjudicationEscalatesOnlyOnce(t *testing.T) {
	s, a := liveFixture(t)
	s.Parameters.Adjudication.Enabled = true
	h := sampleHold(s, a, 18)
	h.ReceiptRef = "result"
	h.Sampling = &SamplingRecord{Sampled: true}
	h.FirstAuditor = a["auditor"].id
	h.Auditor = a["auditor2"].id
	h.FirstVerdict = "reject"
	h.AuditRound = 2
	h.Adjudication = &AdjudicationCase{Origin: "sampled_escalation", SecondCompleted: true, FirstAuditor: h.FirstAuditor, SecondAuditor: h.Auditor, UnitsUsed: 2, Evidences: []RejectEvidence{adjEvidence(h.FirstAuditor, "cause")}}
	before := s.Accounts[h.FirstAuditor].Balance
	if !s.maybeEscalateAdjudication(h, 10) {
		t.Fatal("no escalation")
	}
	if h.AuditRound != 3 || h.Adjudication.UnitsUsed != 3 || s.Accounts[h.FirstAuditor].Balance != before-30 {
		t.Fatal("invalid escalation")
	}
	if s.maybeEscalateAdjudication(h, 11) {
		t.Fatal("recursive escalation")
	}
}
func TestAdjudicationInsufficientDeposit(t *testing.T) {
	s, a := liveFixture(t)
	s.Parameters.Adjudication.Enabled = true
	h := sampleHold(s, a, 19)
	h.ReceiptRef = "result"
	h.Sampling = &SamplingRecord{Sampled: true}
	h.FirstAuditor = a["auditor"].id
	h.Auditor = a["auditor2"].id
	h.FirstVerdict = "reject"
	h.Adjudication = &AdjudicationCase{Origin: "sampled_escalation", SecondCompleted: true, UnitsUsed: 2, Evidences: []RejectEvidence{adjEvidence(h.FirstAuditor, "cause")}}
	s.Accounts[h.FirstAuditor].Balance = 0
	before, _ := s.Hash()
	if s.maybeEscalateAdjudication(h, 10) {
		t.Fatal("unfunded escalation")
	}
	after, _ := s.Hash()
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed escalation mutated state")
	}
}

func TestAdjudicationNineToTenActivation(t *testing.T) {
	s, _ := testState(t)
	s.Version = 9
	s.Height = 8
	s.Parameters.Adjudication = AdjudicationParameters{}
	app := &Application{committed: s}
	if err := app.ConfigureMembershipUpgrade(MembershipUpgrade{ChainID: s.Parameters.ChainID, ActivationHeight: 10}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Hash()
	if err := app.activateMembership(s, &abci.FinalizeBlockRequest{Height: 9}); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Hash()
	if !bytes.Equal(before, after) {
		t.Fatal("early migration")
	}
	if err := app.activateMembership(s, &abci.FinalizeBlockRequest{Height: 11}); err == nil {
		t.Fatal("missed activation accepted")
	}
	res, err := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: 10})
	if err != nil {
		t.Fatal(err)
	}
	if app.staged.Version != 10 || !app.staged.Parameters.Adjudication.valid() || res.ConsensusParamUpdates == nil || res.ConsensusParamUpdates.Version.App != 10 {
		t.Fatal("migration/version update")
	}
}
func TestAdjudicationMigrationPreservesConfiguredFields(t *testing.T) {
	s, _ := testState(t)
	s.Version = 9
	s.Parameters.Adjudication = AdjudicationParameters{Enabled: false, AppealWindowBlocks: 77}
	s.migrateAdjudication()
	if s.Parameters.Adjudication.Enabled || s.Parameters.Adjudication.AppealWindowBlocks != 77 {
		t.Fatal("configuration overwritten")
	}
}
func TestAdjudicationAppealFailureAtomic(t *testing.T) {
	s, a := liveFixture(t)
	s.Parameters.Adjudication.Enabled = true
	h := sampleHold(s, a, 20)
	h.Sampling = nil
	s.Accounts[h.Payer].Balance = 0
	before, _ := s.Hash()
	if s.registerConsumerAppeal(h, h.Payer, 10) == nil {
		t.Fatal("unfunded appeal")
	}
	after, _ := s.Hash()
	if !bytes.Equal(before, after) {
		t.Fatal("failed appeal mutated state")
	}
}

func TestAdjudicationSameReplaySource(t *testing.T) {
	a, b := adjEvidence("a", "cause"), adjEvidence("b", "cause")
	b.ReplayRecord = a.ReplayRecord
	if (&AdjudicationCase{Evidences: []RejectEvidence{a, b}}).convicted() {
		t.Fatal("reused evidence convicted")
	}
}

func TestAdjudicationAppealCitesFirstReservedReview(t *testing.T) {
	h := &Hold{Adjudication: &AdjudicationCase{Origin: "consumer_appeal"}, AuditCompPolicy: &AuditCompParameters{}, AuditOutputTokens: 1, AuditWorks: []AuditWork{{Auditor: "first"}, {Auditor: "second"}}}
	v, _ := proto.Marshal(&pb.AuditVerdict{AuditorId: "first", NOutBilled: 1})
	c, _ := proto.Marshal(&pb.AuditChallenge{Positions: []uint32{0}})
	if err := h.citeAudit(v, c); err != nil {
		t.Fatal(err)
	}
	if !h.AuditWorks[0].Replayed || h.AuditWorks[1].Replayed {
		t.Fatal("wrong reserved review cited")
	}
}
func TestAdjudicationTimeoutRefundsWithoutFourthSeat(t *testing.T) {
	s, h := adjFixture(t)
	if err := s.lockAdjudicationDeposit(h, h.Payer); err != nil {
		t.Fatal(err)
	}
	h.AuditAssignedHeight = 10
	if err := s.reassignAudit(h, 11+s.Parameters.Liveness.AuditTimeout); err != nil {
		t.Fatal(err)
	}
	if h.AuditVerdict != "service_failure" || h.Adjudication.Deposit.Outcome != PathInvalid || h.Adjudication.ThirdAuditor != "" {
		t.Fatal("timeout appended seat or retained deposit")
	}
}
