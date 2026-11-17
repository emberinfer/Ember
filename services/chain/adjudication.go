package chain

import (
	"crypto/sha256"
	pb "ember/proto"
	"encoding/hex"
	"encoding/json"
	"errors"
	"google.golang.org/protobuf/proto"
	"math"
	"strconv"
	"strings"
)

type AdjudicationParameters struct {
	Enabled            bool   `json:"enabled"`
	AppealWindowBlocks int64  `json:"appeal_window_blocks"`
	MaxAuditors        uint32 `json:"max_auditors"`
	BudgetUnits        int64  `json:"budget_units"`
	DepositUnits       int64  `json:"deposit_units"`
}

func (p *AdjudicationParameters) defaults() {
	if *p == (AdjudicationParameters{}) {
		p.Enabled = true
	}
	if p.AppealWindowBlocks == 0 {
		p.AppealWindowBlocks = 39268
	}
	if p.MaxAuditors == 0 {
		p.MaxAuditors = 3
	}
	if p.BudgetUnits == 0 {
		p.BudgetUnits = 3
	}
	if p.DepositUnits == 0 {
		p.DepositUnits = 2
	}
}
func (p AdjudicationParameters) valid() bool {
	return p.AppealWindowBlocks > 0 && p.MaxAuditors == 3 && p.BudgetUnits == 3 && p.DepositUnits == 2
}
func (s *State) migrateAdjudication() { s.Version = 10; s.Parameters.Adjudication.defaults() }

type RejectEvidence struct {
	Auditor          string `json:"auditor"`
	ResultCommitment string `json:"result_commitment"`
	ReplayRecord     string `json:"replay_record"`
	FirstDivergence  *int64 `json:"first_divergence,omitempty"`
	Tolerance        string `json:"tolerance"`
	Cause            string `json:"cause"`
}

func (e RejectEvidence) valid() bool {
	return e.Auditor != "" && e.ResultCommitment != "" && e.ReplayRecord != "" && e.FirstDivergence != nil && *e.FirstDivergence >= 0 && e.Tolerance != "" && e.Cause != ""
}

type AdjudicationOutcome string

const (
	Upheld      AdjudicationOutcome = "upheld"
	Forged      AdjudicationOutcome = "forged"
	NotUpheld   AdjudicationOutcome = "not_upheld"
	PathInvalid AdjudicationOutcome = "path_invalid"
)

type AdjudicationDeposit struct {
	From    string              `json:"from"`
	Locked  int64               `json:"locked"`
	Settled bool                `json:"settled"`
	Outcome AdjudicationOutcome `json:"outcome,omitempty"`
}
type AdjudicationCase struct {
	SecondCompleted       bool                `json:"second_completed"`
	DeliveryState         string              `json:"delivery_state,omitempty"`
	Phase                 string              `json:"phase"`
	Origin                string              `json:"origin"`
	FirstAuditor          string              `json:"first_auditor"`
	SecondAuditor         string              `json:"second_auditor"`
	ThirdAuditor          string              `json:"third_auditor,omitempty"`
	ThirdProof            string              `json:"third_proof,omitempty"`
	Evidences             []RejectEvidence    `json:"evidences,omitempty"`
	Deposit               AdjudicationDeposit `json:"deposit"`
	UnitsUsed             int64               `json:"units_used"`
	ThirdCommitment       string              `json:"third_commitment,omitempty"`
	Revealed              bool                `json:"revealed"`
	DeliveryHeight        int64               `json:"delivery_height"`
	AppealConsumer        string              `json:"appeal_consumer,omitempty"`
	CreatedHeight         int64               `json:"created_height"`
	PenaltyCostsPaid      int64               `json:"penalty_costs_paid,omitempty"`
	PenaltyCosts          int64               `json:"penalty_costs"`
	ForgeryPenaltyApplied bool                `json:"forgery_penalty_applied,omitempty"`
}

// MarshalJSON keeps an uninitialized nested deposit absent, just like the
// enclosing optional adjudication case. Set deposits persist in full.
func (c AdjudicationCase) MarshalJSON() ([]byte, error) {
	type alias AdjudicationCase
	var deposit *AdjudicationDeposit
	if c.Deposit != (AdjudicationDeposit{}) {
		value := c.Deposit
		deposit = &value
	}
	return json.Marshal(struct {
		alias
		Deposit *AdjudicationDeposit `json:"deposit,omitempty"`
	}{alias(c), deposit})
}

func (c *AdjudicationCase) convicted() bool {
	for i, a := range c.Evidences {
		if !a.valid() {
			continue
		}
		for _, b := range c.Evidences[i+1:] {
			if b.valid() && a.Auditor != b.Auditor && a.ReplayRecord != b.ReplayRecord && a.ResultCommitment == b.ResultCommitment && a.Cause == b.Cause {
				return true
			}
		}
	}
	return false
}
func (h *Hold) adjudicationConvicted() bool {
	c := h.Adjudication
	if c == nil {
		return false
	}
	filtered := AdjudicationCase{}
	for _, e := range c.Evidences {
		if e.ResultCommitment == h.ReceiptRef && (e.Auditor == c.FirstAuditor || e.Auditor == c.SecondAuditor || e.Auditor == c.ThirdAuditor) {
			filtered.Evidences = append(filtered.Evidences, e)
		}
	}
	return filtered.convicted()
}
func adjudicationUnit(h *Hold) int64 { return auditBudget(h.Actual, 1500) }
func (s *State) lockAdjudicationDeposit(h *Hold, from string) error {
	if h == nil || h.Adjudication == nil || h.Adjudication.Deposit.From != "" {
		return errors.New("deposit already locked or case missing")
	}
	u := adjudicationUnit(h)
	if u < 0 || u > math.MaxInt64/3 {
		return errors.New("adjudication cost overflow")
	}
	amount := u * 2
	if err := s.debitBalance(from, amount); err != nil {
		return err
	}
	h.Adjudication.Deposit = AdjudicationDeposit{From: from, Locked: amount}
	return nil
}

// PathInvalid is protocol dispute expenditure, booked to the audit pool.
// Upheld costs are a receivable against the existing liable-party penalty;
// no additional slash multiplier is introduced. Forgery needs independently
// verified intentional evidence before the existing penalty hook may run.
func (s *State) settleAdjudicationDeposit(h *Hold, outcome AdjudicationOutcome, height int64) error {
	c := h.Adjudication
	if c == nil || c.Deposit.Settled {
		return nil
	}
	if c.UnitsUsed < 0 || c.UnitsUsed > 3 || adjudicationUnit(h) < 0 || adjudicationUnit(h) > math.MaxInt64/3 {
		return errors.New("invalid adjudication costs")
	}
	costs := adjudicationUnit(h) * c.UnitsUsed
	charged := int64(0)
	switch outcome {
	case Upheld:
		c.PenaltyCosts = costs
	case Forged:
		charged = costs

	case NotUpheld:
		charged = adjudicationUnit(h)
		if c.Origin == "consumer_appeal" {
			charged = costs
		}
	case PathInvalid:
		if err := s.fundAuditPool(h, costs, height); err != nil {
			return err
		}
	default:
		return errors.New("invalid adjudication settlement")
	}
	if charged > c.Deposit.Locked {
		charged = c.Deposit.Locked
	}
	if charged > 0 {
		if err := s.fundAuditPool(h, charged, height); err != nil {
			return err
		}
	}
	if err := s.creditBalance(c.Deposit.From, c.Deposit.Locked-charged); err != nil {
		return err
	}
	if outcome == Forged {
		// This outcome is reserved for independently proven intentional forgery,
		// never ordinary disagreement or an uncorroborated rejection.
		if err := s.slashOffender(h, c.Deposit.From, h.Actual, height); err != nil {
			return err
		}
		c.ForgeryPenaltyApplied = true
	}
	c.Deposit.Settled = true
	c.Deposit.Outcome = outcome
	c.Phase = "resolved"
	return nil
}

// Pipeline/MoE adjudication is gated until their multi-party evidence bindings
// are available. Whole-model recompute continues to use the calibrated path.
func (s *State) adjudicationEnabled(h *Hold) bool {
	return s.Version >= 10 && s.Parameters.Adjudication.Enabled && h.Pipeline == nil && h.Moe == nil
}
func (s *State) drawAdjudicator(h *Hold, height int64) (string, []byte, error) {
	ids := []string{}
	source := s.finalizedState()
	producer := source.ActiveProfile(h.Node, source.Height)
	if len(h.ProducerProfile) > 0 {
		producer = &pb.ResourceProfile{}
		if proto.Unmarshal(h.ProducerProfile, producer) != nil {
			return "", nil, errors.New("invalid frozen producer")
		}
	}
	for id := range source.Nodes {
		if id == h.Node || id == h.FirstAuditor || id == h.Auditor {
			continue
		}
		c := h.Adjudication
		if c != nil && (id == c.FirstAuditor || id == c.SecondAuditor || id == c.ThirdAuditor) {
			continue
		}
		if source.EligibleAuditorProfile(id, h.Node, producer, source.Height) {
			ids = append(ids, id)
		}
	}
	return s.drawAuditor(h, ids, height)
}
func (s *State) maybeEscalateAdjudication(h *Hold, height int64) bool {
	trial, planned, err := s.adjudicationTrial(h)
	if err != nil {
		return false
	}
	if !trial.escalateAdjudication(planned, height) {
		return false
	}
	*s = *trial
	*h = *planned
	s.Holds[h.ID] = h
	return true
}
func (s *State) adjudicationTrial(h *Hold) (*State, *Hold, error) {
	trial, err := s.transactionCopy()
	if err != nil {
		return nil, nil, err
	}
	raw, err := json.Marshal(h)
	if err != nil {
		return nil, nil, err
	}
	planned := &Hold{}
	if err = json.Unmarshal(raw, planned); err != nil {
		return nil, nil, err
	}
	trial.Holds[h.ID] = planned
	return trial, planned, nil
}
func (s *State) escalateAdjudication(h *Hold, height int64) bool {
	c := h.Adjudication
	if !s.adjudicationEnabled(h) || c == nil || !c.SecondCompleted || c.UnitsUsed < 2 || c.UnitsUsed >= 3 || c.ThirdAuditor != "" || h.adjudicationConvicted() {
		return false
	}
	if c.Origin != "consumer_appeal" && (h.Sampling == nil || !h.Sampling.Sampled || h.FirstVerdict != "reject" || len(c.Evidences) == 0 || !c.Evidences[0].valid() || c.Evidences[0].Auditor != h.FirstAuditor || c.Evidences[0].ResultCommitment != h.ReceiptRef) {
		return false
	}
	// Draw before locking; failed admission must not leave work reservations.
	raw, _ := json.Marshal(h)
	next, proof, err := s.drawAdjudicator(h, height)
	if err != nil {
		return false
	}
	if c.Origin != "consumer_appeal" {
		if err = s.lockAdjudicationDeposit(h, h.FirstAuditor); err != nil {
			_ = json.Unmarshal(raw, h)
			return false
		}
	}
	c = h.Adjudication
	c.ThirdAuditor = next
	c.ThirdProof = hex.EncodeToString(proof)
	c.UnitsUsed++
	c.Phase = "third_commit"
	h.Auditor = next
	h.AuditProof = c.ThirdProof
	h.AuditRound = 3
	h.AuditAssignedHeight = height
	h.AuditVerdict = ""
	h.AuditRef, h.EvidenceRef, h.VerdictBytes = "", "", nil
	return true
}
func thirdAdjudicationHash(verdict string, e RejectEvidence) string {
	pos := "-1"
	if e.FirstDivergence != nil {
		pos = strconv.FormatInt(*e.FirstDivergence, 10)
	}
	raw := []byte("ember/third/v1\x00" + strings.Join([]string{verdict, e.Auditor, e.ResultCommitment, e.ReplayRecord, pos, e.Tolerance, e.Cause}, "\x00"))
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

// The assignment payload is only auditor ID + proof. Never include earlier
// conclusions or divergence positions before the third auditor commits.
func (s *State) commitThirdAdjudication(h *Hold, signer, commitment string) error {
	c := h.Adjudication
	b, err := hex.DecodeString(commitment)
	if c == nil || c.ThirdAuditor != signer || c.ThirdCommitment != "" || err != nil || len(b) != 32 {
		return errors.New("invalid third commitment")
	}
	c.ThirdCommitment = commitment
	c.Phase = "third_reveal"
	return nil
}
func (s *State) revealThirdAdjudication(h *Hold, signer, verdict string, e RejectEvidence, height int64) error {
	c := h.Adjudication
	if c == nil || c.ThirdAuditor != signer || c.Revealed || c.ThirdCommitment == "" || c.ThirdCommitment != thirdAdjudicationHash(verdict, e) || e.Auditor != signer {
		return errors.New("invalid third reveal")
	}
	c.Revealed = true
	if verdict == "reject" && e.valid() {
		c.Evidences = append(c.Evidences, e)
	}
	h.AuditVerdict = "service_failure"
	out := NotUpheld
	if h.adjudicationConvicted() {
		h.AuditVerdict = "reject"
		out = Upheld
	}
	return s.settleAdjudicationDeposit(h, out, height)
}
func (s *State) registerConsumerAppeal(h *Hold, consumer string, height int64) error {
	if h == nil {
		return errors.New("hold missing")
	}
	trial, planned, err := s.adjudicationTrial(h)
	if err != nil {
		return err
	}
	if err = trial.registerAppeal(planned, consumer, height); err != nil {
		return err
	}
	*s = *trial
	*h = *planned
	s.Holds[h.ID] = h
	return nil
}
func (s *State) registerAppeal(h *Hold, consumer string, height int64) error {
	if h == nil || !s.adjudicationEnabled(h) || consumer != h.Payer || h.Adjudication != nil || (h.Sampling != nil && h.Sampling.Sampled) || h.Auditor != "" || h.AuditVerdict != "" {
		return errors.New("invalid consumer appeal")
	}
	delivery := h.SettleHeight
	if delivery == 0 {
		delivery = h.SubmitHeight
	}
	if delivery <= 0 || height < delivery || height-delivery > s.Parameters.Adjudication.AppealWindowBlocks {
		return errors.New("appeal window closed")
	}
	raw, _ := json.Marshal(h)
	h.Adjudication = &AdjudicationCase{DeliveryState: h.State, Origin: "consumer_appeal", Phase: "review", DeliveryHeight: delivery, AppealConsumer: consumer, CreatedHeight: height, UnitsUsed: 2}
	a, p, err := s.drawAdjudicator(h, height)
	if err == nil {
		h.Adjudication.FirstAuditor = a
		h.Auditor = a
		h.AuditProof = hex.EncodeToString(p)
		var b string
		b, _, err = s.drawAdjudicator(h, height)
		h.Adjudication.SecondAuditor = b
	}
	if err == nil {
		err = s.lockAdjudicationDeposit(h, consumer)
	}
	if err != nil {
		_ = json.Unmarshal(raw, h)
		return err
	}
	h.Unaudited = false
	h.State = "dispute"
	h.AuditRound = 1
	h.AuditAssignedHeight = height
	return nil
}

func (s *State) recordAdjudicationOutcome(h *Hold, signer, outcome, ref string, at ...int64) error {
	height := s.Height
	if len(at) > 0 {
		height = at[0]
	}
	c := h.Adjudication
	if c == nil {
		c = &AdjudicationCase{Origin: "sampled_escalation", Phase: "review", FirstAuditor: signer, CreatedHeight: height}
		h.Adjudication = c
	}
	e := s.adjudicationEvidence(h, signer, outcome, ref)
	if h.AuditRound == 2 {
		c.SecondCompleted = true
	}
	if h.AuditRound == 3 {
		return s.revealThirdAdjudication(h, signer, outcome, e, height)
	}
	if outcome == "reject" {
		c.Evidences = append(c.Evidences, e)
	}
	if c.Origin == "consumer_appeal" {
		if h.AuditRound == 1 {
			h.FirstAuditor = signer
			h.FirstVerdict = outcome
			h.Auditor = c.SecondAuditor
			h.AuditRound = 2
			h.AuditAssignedHeight = height
			for _, r := range h.Assignments {
				if len(r.AuditorIDs) > 0 && r.AuditorIDs[0] == h.Auditor {
					h.AuditProof = hex.EncodeToString(r.Seed)
				}
			}
			return nil
		}
		if h.adjudicationConvicted() {
			h.AuditVerdict = "reject"
			return s.settleAdjudicationDeposit(h, Upheld, height)
		}
		if outcome != h.FirstVerdict && s.maybeEscalateAdjudication(h, height) {
			return nil
		}
		h.AuditVerdict = "service_failure"
		if outcome == "accept" && h.FirstVerdict == "accept" {
			h.AuditVerdict = "accept"
		}
		return s.settleAdjudicationDeposit(h, NotUpheld, height)
	}
	if h.AuditRound == 1 {
		c.UnitsUsed = 1
		if outcome == "accept" {
			h.AuditVerdict = "accept"
			s.Evidence[ref] = &WorkEvidence{HoldID: h.ID, Node: h.Node, Reference: ref}
			return nil
		}
		if outcome == "reject" {
			h.FirstAuditor = signer
			h.FirstVerdict = outcome
			next, proof, err := s.selectAuditor(h, signer, height)
			if err == nil {
				c.SecondAuditor = next
				c.UnitsUsed = 2
				h.Auditor = next
				h.AuditProof = hex.EncodeToString(proof)
				h.AuditRound = 2
				h.AuditRef, h.EvidenceRef, h.VerdictBytes = "", "", nil
				return nil
			}
		}
		h.AuditVerdict = "service_failure"
		return nil
	}
	if h.adjudicationConvicted() || (outcome == "reject" && h.FirstVerdict == "reject") {
		h.AuditVerdict = "reject"
		return nil
	}
	if s.maybeEscalateAdjudication(h, height) {
		return nil
	}
	h.AuditVerdict = "service_failure"
	return nil
}
