package chain

import (
	"bytes"
	"crypto/sha256"
	pb "ember/proto"
	"encoding/hex"
	"encoding/json"
	"errors"
	"google.golang.org/protobuf/proto"
)

type PrivacyOperator struct {
	PolicyHash string          `json:"policy_hash"`
	WrapKey    []byte          `json:"wrap_key"`
	Policy     json.RawMessage `json:"policy"`
}
type PrivacyEpoch struct {
	WrapKeys  map[string][]byte `json:"wrap_keys"`
	ID        string            `json:"id"`
	Threshold int               `json:"threshold"`
	Members   []string          `json:"members"`
	Ended     bool              `json:"ended"`
}
type PrivacyMigration struct {
	ID         string `json:"id"`
	From       string `json:"from"`
	To         string `json:"to"`
	WrapKey    []byte `json:"wrap_key"`
	WrappedKey []byte `json:"wrapped_key,omitempty"`
	Finalized  bool   `json:"finalized"`
}
type PrivacySegment struct {
	Root      string `json:"root"`
	BundleRef string `json:"bundle_ref"`
	Tokens    uint32 `json:"tokens"`
}
type PrivacyAudit struct {
	Auditor   string `json:"auditor"`
	Verdict   string `json:"verdict"`
	StatsHash string `json:"stats_hash"`
	Height    int64  `json:"height"`
}
type PrivacyRequest struct {
	Audits       []PrivacyAudit     `json:"audits,omitempty"`
	Migrations   []PrivacyMigration `json:"migrations,omitempty"`
	Segments     []PrivacySegment   `json:"segments,omitempty"`
	ID           string             `json:"id"`
	Consumer     string             `json:"consumer"`
	Executor     string             `json:"executor"`
	Auditor      string             `json:"auditor"`
	Tier         string             `json:"tier"`
	ModelID      string             `json:"model_id"`
	Epoch        string             `json:"epoch"`
	EnvelopeHash string             `json:"envelope_hash"`
	Root         string             `json:"root"`
	BundleRef    string             `json:"bundle_ref"`
	Verdict      string             `json:"verdict"`
	StatsHash    string             `json:"stats_hash"`
	Status       string             `json:"status"`
	FinalHeight  int64              `json:"final_height"`
}
type PrivacyEvent struct {
	Kind      string `json:"kind"`
	RequestID string `json:"request_id"`
	Epoch     string `json:"epoch"`
	Actor     string `json:"actor"`
	Reason    string `json:"reason"`
	Height    int64  `json:"height"`
}
type PrivacyState struct {
	Operators map[string]PrivacyOperator `json:"operators"`
	Epochs    map[string]*PrivacyEpoch   `json:"epochs"`
	Requests  map[string]*PrivacyRequest `json:"requests"`
	Events    []PrivacyEvent             `json:"events"`
}
type PrivacyAction struct {
	Offense           *pb.OffenseGrade `json:"offense,omitempty"`
	Amount            int64            `json:"amount,omitempty"`
	Actual            int64            `json:"actual,omitempty"`
	InputTokens       uint32           `json:"input_tokens,omitempty"`
	OutputTokens      uint32           `json:"output_tokens,omitempty"`
	PricingCommitment []byte           `json:"pricing_commitment,omitempty"`
	MigrationID       string           `json:"migration_id,omitempty"`
	WrappedKey        []byte           `json:"wrapped_key,omitempty"`
	Op                string           `json:"op"`
	RequestID         string           `json:"request_id,omitempty"`
	Executor          string           `json:"executor,omitempty"`
	Auditor           string           `json:"auditor,omitempty"`
	Tier              string           `json:"tier,omitempty"`
	ModelID           string           `json:"model_id,omitempty"`
	Epoch             string           `json:"epoch,omitempty"`
	Threshold         int              `json:"threshold,omitempty"`
	Members           []string         `json:"members,omitempty"`
	EnvelopeHash      string           `json:"envelope_hash,omitempty"`
	Root              string           `json:"root,omitempty"`
	BundleRef         string           `json:"bundle_ref,omitempty"`
	Verdict           string           `json:"verdict,omitempty"`
	StatsHash         string           `json:"stats_hash,omitempty"`
	Kind              string           `json:"kind,omitempty"`
	Reason            string           `json:"reason,omitempty"`
	WrapKey           []byte           `json:"wrap_key,omitempty"`
	Policy            json.RawMessage  `json:"policy,omitempty"`
}

func hash32(s string) bool { b, e := hex.DecodeString(s); return e == nil && len(b) == 32 }
func (s *State) applyPrivacy(signer string, a *PrivacyAction, height int64) error {
	if a == nil {
		return errors.New("missing privacy action")
	}
	if s.Privacy == nil {
		s.Privacy = &PrivacyState{Operators: map[string]PrivacyOperator{}, Epochs: map[string]*PrivacyEpoch{}, Requests: map[string]*PrivacyRequest{}}
	}
	p := s.Privacy
	r := p.Requests[a.RequestID]
	switch a.Op {
	case "operator":
		var policy struct {
			RetentionSeconds *int64 `json:"retention_seconds"`
			DeletionPractice string `json:"deletion_practice"`
			EvidenceHandling string `json:"evidence_handling"`
		}
		if len(a.WrapKey) != 32 || json.Unmarshal(a.Policy, &policy) != nil || policy.RetentionSeconds == nil || *policy.RetentionSeconds < 0 || policy.DeletionPractice == "" || policy.EvidenceHandling == "" {
			return errors.New("invalid operator policy")
		}
		for _, epoch := range p.Epochs {
			if !epoch.Ended && len(epoch.WrapKeys[signer]) > 0 && !bytes.Equal(epoch.WrapKeys[signer], a.WrapKey) {
				return errors.New("rotate custodian key only after epoch end")
			}
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, a.Policy); err != nil {
			return err
		}
		policyHash := sha256.Sum256(compact.Bytes())
		p.Operators[signer] = PrivacyOperator{PolicyHash: hex.EncodeToString(policyHash[:]), WrapKey: a.WrapKey, Policy: a.Policy}
	case "epoch":
		if !s.Governors[signer] || a.Epoch == "" || p.Epochs[a.Epoch] != nil || a.Threshold < 2 || a.Threshold > len(a.Members) || len(a.Members) > 255 {
			return errors.New("invalid custodian epoch")
		}
		seen := map[string]bool{}
		for _, id := range a.Members {
			if seen[id] || len(p.Operators[id].WrapKey) != 32 {
				return errors.New("invalid custodian")
			}
			seen[id] = true
		}
		keys := map[string][]byte{}
		for _, id := range a.Members {
			for _, old := range p.Epochs {
				if bytes.Equal(old.WrapKeys[id], p.Operators[id].WrapKey) {
					return errors.New("new epoch requires rotated custodian keys")
				}
			}
			keys[id] = append([]byte(nil), p.Operators[id].WrapKey...)
		}
		p.Epochs[a.Epoch] = &PrivacyEpoch{WrapKeys: keys, ID: a.Epoch, Threshold: a.Threshold, Members: a.Members}
	case "end_epoch":
		e := p.Epochs[a.Epoch]
		if !s.Governors[signer] || e == nil || e.Ended {
			return errors.New("invalid epoch end")
		}
		for _, v := range p.Requests {
			if v.Epoch == a.Epoch && v.Status != "final" {
				return errors.New("epoch still has pending requests")
			}
		}
		e.Ended = true
	case "request":
		e := p.Epochs[a.Epoch]
		if a.Tier == "P1" {
			return errors.New("TIER_UNAVAILABLE")
		}
		if a.Tier != "P0" || a.RequestID == "" || r != nil || e == nil || e.Ended || !hash32(a.EnvelopeHash) || s.Models[a.ModelID].ID == "" || len(p.Operators[a.Executor].WrapKey) != 32 {
			return errors.New("invalid privacy request")
		}
		if err := s.applyActionInternal(signer, Action{Type: "freeze", HoldID: a.RequestID, Payer: signer, Node: a.Executor, Amount: a.Amount, PricingCommitment: a.PricingCommitment}, height); err != nil {
			return err
		}
		h := s.Holds[a.RequestID]
		policy := s.Models[a.ModelID].Pricing
		if h.TokenFee == nil || len(h.TokenFee.Commitment) == 0 || policy == nil || policy.ExecutorWeight != 80 || policy.AuditorWeight != 15 || policy.ConsumerWeight != 5 {
			return errors.New("encrypted requests require pricing-v2 commitment and 80/15/5 policy")
		}
		if profile := s.ActiveProfile(a.Executor, height); profile == nil || profile.ChainId != a.ModelID {
			return errors.New("executor model mismatch")
		}
		p.Requests[a.RequestID] = &PrivacyRequest{ID: a.RequestID, Consumer: signer, Executor: a.Executor, Tier: a.Tier, ModelID: a.ModelID, Epoch: a.Epoch, EnvelopeHash: a.EnvelopeHash, Status: "requested"}
	case "checkpoint", "commit":
		if r == nil || signer != r.Executor || (r.Status != "requested" && r.Status != "checkpoint") || !hash32(a.Root) || !hash32(a.BundleRef) || a.OutputTokens == 0 {
			return errors.New("invalid privacy commitment")
		}
		if len(r.Migrations) > 0 && !r.Migrations[len(r.Migrations)-1].Finalized {
			return errors.New("migration pending")
		}
		if len(r.Segments) > 0 && a.OutputTokens <= r.Segments[len(r.Segments)-1].Tokens {
			return errors.New("non-increasing output")
		}
		h := s.Holds[r.ID]
		if h == nil || h.State != "frozen" || height >= h.ExpireHeight {
			return errors.New("invalid privacy hold")
		}
		if h.TokenFee != nil {
			c := &pb.ExecutionCommitment{}
			if proto.Unmarshal(h.TokenFee.Commitment, c) != nil || len(h.TokenFee.Commitment) == 0 || c.InputTokens != a.InputTokens || a.OutputTokens > c.MaxOutputTokens {
				return errors.New("counts differ from frozen tokenization")
			}
		}
		r.Root, r.BundleRef = a.Root, a.BundleRef
		if a.Op == "checkpoint" {
			r.Segments = append(r.Segments, PrivacySegment{Root: a.Root, BundleRef: a.BundleRef, Tokens: a.OutputTokens})
			r.Status = "checkpoint"
			return nil
		}
		if a.Actual < 0 || a.Actual > h.Amount {
			return errors.New("invalid actual fee")
		}
		if err := verifyTokenFee(h.TokenFee, a.InputTokens, a.OutputTokens, a.Actual, nil); err != nil {
			return err
		}
		if h.TokenFee != nil {
			h.TokenFee.OutputTokens = a.OutputTokens
		}
		h.AuditInputTokens, h.AuditOutputTokens = a.InputTokens, a.OutputTokens
		h.Actual, h.ReceiptRef, h.State, h.SubmitHeight, h.SettleHeight = a.Actual, a.Root, "dispute", height, height+s.Parameters.DisputeBlocks
		r.Status = "committed"
	case "assign":
		if r == nil || !s.Coordinators[signer] || r.Status != "committed" || a.Auditor != "" {
			return errors.New("invalid privacy assignment")
		}
		if err := s.applyActionInternal(signer, Action{Type: "assign_audit", HoldID: r.ID}, height); err != nil {
			return err
		}
		h := s.Holds[r.ID]
		if len(p.Operators[h.Auditor].WrapKey) != 32 {
			return errors.New("selected auditor has no wrapping key")
		}
		r.Auditor, r.Status = h.Auditor, "assigned"
	case "verdict":
		if r == nil || signer != r.Auditor || r.Status != "assigned" || (a.Verdict != "accept" && a.Verdict != "reject" && a.Verdict != "inconclusive") || !hash32(a.StatsHash) {
			return errors.New("invalid private audit verdict")
		}
		r.Audits = append(r.Audits, PrivacyAudit{Auditor: signer, Verdict: a.Verdict, StatsHash: a.StatsHash, Height: height})
		h := s.Holds[r.ID]
		// The outer privacy transaction authenticates the assigned auditor and
		// its grade; keep the common signed-verdict representation for settlement.
		verdict := &pb.AuditVerdict{AuditorId: signer, Verdict: a.Verdict, Offense: a.Offense}
		raw, err := proto.Marshal(verdict)
		if err != nil {
			return err
		}
		if a.Verdict == "reject" {
			grade, err := s.validateGrade(h, raw)
			if err != nil {
				return err
			}
			if h.AuditRound == 1 {
				h.FirstGrade = proto.Clone(grade).(*pb.OffenseGrade)
			} else if h.FirstGrade == nil || grade.Class != h.FirstGrade.Class || grade.Fault != h.FirstGrade.Fault || grade.Band != h.FirstGrade.Band || grade.LengthClass != h.FirstGrade.LengthClass {
				return errors.New("confirming offense grades disagree")
			}
		}
		h.VerdictBytes = raw
		h.AuditRef, h.EvidenceRef = a.StatsHash, r.BundleRef
		if err := s.recordAuditOutcome(h, signer, a.Verdict, r.BundleRef, height); err != nil {
			return err
		}
		if h.AuditVerdict == "" {
			if len(p.Operators[h.Auditor].WrapKey) != 32 {
				return errors.New("selected auditor has no wrapping key")
			}
			r.Auditor = h.Auditor
			return nil
		}
		r.Verdict, r.StatsHash, r.Status = h.AuditVerdict, a.StatsHash, "audited"
		r.FinalHeight = h.SettleHeight
	case "finalize":
		if r == nil || r.Status != "audited" || height < r.FinalHeight {
			return errors.New("not final")
		}
		if err := s.applyActionInternal(signer, Action{Type: "settle", HoldID: r.ID, Outcome: r.Verdict}, height); err != nil {
			return err
		}
		r.Status = "final"
	case "cancel", "expire":
		if r == nil || r.Status == "final" {
			return errors.New("inactive privacy request")
		}
		op := "cancel"
		if a.Op == "expire" {
			op = "expire_hold"
		}
		if err := s.applyActionInternal(signer, Action{Type: op, HoldID: r.ID}, height); err != nil {
			return err
		}
		r.Status, r.Verdict, r.FinalHeight = "final", "service_failure", height
	case "migrate":
		if r == nil || signer != r.Consumer || (r.Status != "requested" && r.Status != "checkpoint") || !hash32(a.MigrationID) || a.Executor == r.Executor || a.Executor == r.Consumer || len(p.Operators[a.Executor].WrapKey) != 32 {
			return errors.New("invalid migration")
		}
		for _, m := range r.Migrations {
			if m.ID == a.MigrationID || m.From == a.Executor || !m.Finalized {
				return errors.New("stale migration or retired executor")
			}
		}
		target := s.ActiveProfile(a.Executor, height)
		h := s.Holds[r.ID]
		if h == nil || h.State != "frozen" || height >= h.ExpireHeight || target == nil || target.ChainId != r.ModelID || s.Accounts[a.Executor].Stake < s.Parameters.MinNodeStake || h.Amount > s.Accounts[a.Executor].Stake/5-s.exposure(a.Executor) {
			return errors.New("ineligible migration target")
		}
		r.Migrations = append(r.Migrations, PrivacyMigration{ID: a.MigrationID, From: r.Executor, To: a.Executor, WrapKey: append([]byte(nil), p.Operators[a.Executor].WrapKey...)})
	case "rewrap":
		if r == nil || signer != r.Consumer || len(r.Migrations) == 0 || (r.Status != "requested" && r.Status != "checkpoint") {
			return errors.New("invalid rewrap")
		}
		m := &r.Migrations[len(r.Migrations)-1]
		if m.Finalized || m.ID != a.MigrationID || m.To != a.Executor || len(a.WrappedKey) == 0 || len(a.WrappedKey) > 4096 || !bytes.Equal(m.WrapKey, p.Operators[m.To].WrapKey) {
			return errors.New("stale or wrong-target rewrap")
		}
		target := s.ActiveProfile(m.To, height)
		h := s.Holds[r.ID]
		if h == nil || h.State != "frozen" || height >= h.ExpireHeight || target == nil || target.ChainId != r.ModelID || s.Accounts[m.To].Stake < s.Parameters.MinNodeStake || h.Amount > s.Accounts[m.To].Stake/5-s.exposure(m.To) {
			return errors.New("ineligible migration target")
		}
		m.WrappedKey = append([]byte(nil), a.WrappedKey...)
		m.Finalized = true
		r.Executor = m.To
		h.Node = m.To
		h.ProducerProfile = append([]byte(nil), s.Nodes[m.To].Profile...)
	case "event":
		e := p.Epochs[a.Epoch]
		if e == nil {
			return errors.New("unknown event epoch")
		}
		member := false
		for _, id := range e.Members {
			if id == signer {
				member = true
			}
		}
		switch a.Kind {
		case "share_release", "share_refusal":
			if !member {
				return errors.New("not epoch custodian")
			}
			if a.Kind == "share_release" && (r == nil || r.Epoch != a.Epoch || r.Status != "assigned" || e.Ended) {
				return errors.New("release not authorized")
			}
			if a.Kind == "share_refusal" && a.Reason == "" {
				return errors.New("refusal reason required")
			}
		case "shares_destroyed":
			if !member || !e.Ended {
				return errors.New("epoch not ended")
			}
		case "migration_deleted", "migration_delete_failed":
			authorized := false
			if r != nil && r.Epoch == a.Epoch {
				for _, m := range r.Migrations {
					if m.From == signer && m.Finalized {
						authorized = true
					}
				}
			}
			if !authorized {
				return errors.New("not retired executor")
			}
		case "bundle_deleted":
			if r == nil || r.Epoch != a.Epoch || signer != r.Executor || r.Status != "final" {
				return errors.New("deletion not authorized")
			}
		default:
			return errors.New("unsupported privacy event")
		}
		p.Events = append(p.Events, PrivacyEvent{Kind: a.Kind, RequestID: a.RequestID, Epoch: a.Epoch, Actor: signer, Reason: a.Reason, Height: height})
	default:
		return errors.New("unknown privacy operation")
	}
	return nil
}

// AssignmentProof binds the current finalized assignment, request and custodian epoch.
func AssignmentProof(chainID string, r *PrivacyRequest) string {
	raw, _ := json.Marshal([]string{"ember/privacy/assignment/v2", chainID, r.ID, r.Epoch, r.Auditor, r.Root, r.EnvelopeHash})
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}
