package chain

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

// Liveness is binary eligibility, never a proof-count lottery weight.
type LivenessParameters struct {
	ProofInterval    int64  `json:"proof_interval"`
	ProofMinInterval int64  `json:"proof_min_interval"`
	LivenessWindow   int64  `json:"liveness_window"`
	AuditTimeout     int64  `json:"audit_timeout"`
	MaxAuditReassign uint32 `json:"max_audit_reassign"`
}

func (p *LivenessParameters) defaults() {
	if p.ProofInterval == 0 {
		p.ProofInterval = 240
	}
	if p.ProofMinInterval == 0 {
		p.ProofMinInterval = 60
	}
	if p.LivenessWindow == 0 {
		p.LivenessWindow = 960
	}
	if p.AuditTimeout == 0 {
		p.AuditTimeout = 300
	}
	if p.MaxAuditReassign == 0 {
		p.MaxAuditReassign = 3
	}
}
func (p LivenessParameters) valid() bool {
	return p.ProofMinInterval > 0 && p.ProofInterval >= p.ProofMinInterval && p.LivenessWindow >= p.ProofInterval && p.AuditTimeout > 0 && p.MaxAuditReassign > 0
}
func (s *State) livenessEligible(id string) bool {
	if s.Version < 6 || !s.Parameters.RegisteredAuditors {
		return true
	}
	source := s.finalizedState()
	// The activation block still reads a v5 finalized snapshot; its facts have
	// not acquired proof semantics yet. The next snapshot carries the new grace.
	if source.Version < 6 {
		return true
	}
	r := source.Nodes[id]
	return r != nil && r.LastProofHeight <= source.Height && source.Height-r.LastProofHeight <= source.Parameters.Liveness.LivenessWindow
}
func (s *State) migrateLiveness(height int64) {
	s.Parameters.AuditEquality = AuditEqualityParameters{Enabled: true}
	s.Parameters.AuditEquality.defaults()
	s.Parameters.Liveness = LivenessParameters{}
	s.Parameters.Liveness.defaults()
	// Existing registrations receive the same full first-proof window as new ones.
	for _, r := range s.Nodes {
		r.LastProofHeight = height
	}
	for _, h := range s.Holds {
		if h.State == "dispute" && h.Auditor != "" && h.AuditVerdict == "" {
			h.AuditAssignedHeight = height
		}
	}
	// This migration is the 5 -> 6 step: it lands on version 6 even when the
	// running binary's AppVersion is newer. Reaching 7 from 6 is a separate
	// manifest (see migrateAuditEligibility).
	s.Version = 6
}
func (s *State) reassignAudit(h *Hold, height int64) error {
	if h == nil || h.State != "dispute" || h.Auditor == "" || h.AuditVerdict != "" || height <= h.AuditAssignedHeight || height-h.AuditAssignedHeight <= s.Parameters.Liveness.AuditTimeout {
		return errors.New("invalid or premature audit reassignment")
	}
	// Anyone can trigger exhaustion with this same transaction; settlement remains
	// the existing explicit settle transition, with no timer or trusted caller.
	if h.AuditReassignments >= s.Parameters.Liveness.MaxAuditReassign {
		h.AuditVerdict = "service_failure"
		return nil
	}
	next, proof, err := s.selectAuditor(h, h.Auditor, height)
	if err != nil {
		if !strings.HasPrefix(err.Error(), "no independent auditor") {
			return err
		}
		h.AuditVerdict = "service_failure"
		return nil
	}
	h.AuditReassignments++
	h.Auditor, h.AuditProof, h.AuditAssignedHeight = next, hex.EncodeToString(proof), height
	h.AuditRef, h.EvidenceRef, h.VerdictBytes = "", "", nil
	return nil
}

// Keep legacy serialized parameter bytes unchanged before coordinated activation.
func (p Parameters) MarshalJSON() ([]byte, error) {
	type alias Parameters
	var price *PriceParameters
	if p.SupplyDemand != (PriceParameters{}) {
		v := p.SupplyDemand
		price = &v
	}
	var sampling *AuditSamplingParameters
	if p.AuditSampling != (AuditSamplingParameters{}) {
		v := p.AuditSampling
		sampling = &v
	}
	var eq *AuditEqualityParameters
	if p.AuditEquality != (AuditEqualityParameters{}) {
		value := p.AuditEquality
		eq = &value
	}
	var live *LivenessParameters
	if p.Liveness != (LivenessParameters{}) {
		value := p.Liveness
		live = &value
	}
	return json.Marshal(struct {
		alias
		SupplyDemand  *PriceParameters         `json:"supply_demand,omitempty"`
		AuditSampling *AuditSamplingParameters `json:"audit_sampling,omitempty"`
		Liveness      *LivenessParameters      `json:"liveness,omitempty"`
		AuditEquality *AuditEqualityParameters `json:"audit_equality,omitempty"`
	}{alias(p), price, sampling, live, eq})
}
