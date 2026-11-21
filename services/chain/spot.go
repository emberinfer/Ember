package chain

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sort"

	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

const (
	SPOT_OPEN_ROUND_BLOCKS    int64 = 120
	SPOT_FIRST_VERDICT_BLOCKS int64 = 60
	SPOT_SUBSTITUTE_BLOCKS    int64 = 120
	// Single-dispute bounded package: (1+15)*4.5MB (ddcfb1f7: 424MB/94) ~=72MB.
	// 72/0.357 ~=202s transfer +224s CPU + margin ~=460s;
	// 600*1.76=1056s gives >=2.3x margin. Total 120+60+120+60+600
	// =960 blocks, comfortably inside the 5000-block dispute window.
	SPOT_ESCALATION_BLOCKS int64 = 600
	SPOT_ESCALATION_K            = 15
	AVAIL_MINOR_WINDOW     int64 = 50000
	AVAIL_MAJOR_WINDOW     int64 = 200000
	AVAIL_T1_MINOR               = 4
	AVAIL_T1_MAJOR               = 2
	AVAIL_T2_MINOR               = 8
	AVAIL_T2_MAJOR               = 4
	AVAIL_T3_MINOR               = 12
	AVAIL_T3_MAJOR               = 6
	AVAIL_PAUSE_BLOCKS     int64 = 30000
	AVAIL_FINE_SUB         int64 = 1000
)

type AvailabilityEvent struct {
	Height int64 `json:"height"`
	Major  bool  `json:"major"`
}
type AvailabilityCounters struct {
	ChallengedTotal uint64 `json:"challenged_total"`
	UnopenedTotal   uint64 `json:"unopened_total"`
}
type AvailabilityRecord struct {
	Events     []AvailabilityEvent  `json:"events,omitempty"`
	Minor      int                  `json:"minor"`
	Major      int                  `json:"major"`
	Level      int                  `json:"level"`
	PauseUntil int64                `json:"pause_until"`
	Executor   AvailabilityCounters `json:"executor"`
	Auditor    AvailabilityCounters `json:"auditor"`
}
type SpotChallenge struct {
	EvidenceResolved bool              `json:"evidence_resolved,omitempty"`
	RequestID        string            `json:"request_id"`
	Auditor          string            `json:"auditor"`
	StepSet          []uint64          `json:"step_set"`
	ChallengeHeight  int64             `json:"challenge_height"`
	Round            string            `json:"round"`
	Deadline         int64             `json:"deadline"`
	Received         bool              `json:"received"`
	VerdictDeadline  int64             `json:"verdict_deadline,omitempty"`
	Accusation       *SpoolAttestation `json:"accusation,omitempty"`
	AccusedAt        int64             `json:"accused_at,omitempty"`
	History          []SpotChallenge   `json:"history,omitempty"`
	Substitute       bool              `json:"substitute,omitempty"`
	ReplaceAfter     int64             `json:"replace_after,omitempty"`
	FirstVerdict     []byte            `json:"first_verdict,omitempty"`
	GoodFaithAuditor string            `json:"good_faith_auditor,omitempty"`
}

// Signature covers domain-separated canonical Go JSON of all other fields.
// Public key must be the registered gateway ResourceProfile signer.
type SpoolAttestation struct {
	RequestID       string `json:"request_id"`
	ChallengeHeight int64  `json:"challenge_height"`
	StepSetHash     []byte `json:"step_set_hash"`
	Gateway         string `json:"gateway"`
	Received        bool   `json:"received"`
	Height          int64  `json:"height"`
	Signature       []byte `json:"signature,omitempty"`
}

func SpoolSigningBytes(a SpoolAttestation) []byte {
	a.Signature = nil
	b, _ := json.Marshal(a)
	return append([]byte("EMBER-SPOT-SPOOL-V1\x00"), b...)
}
func SpotStepSetHash(steps []uint64) []byte {
	h := sha256.New()
	for _, step := range steps {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], step)
		h.Write(b[:])
	}
	return h.Sum(nil)
}
func SpotK(tier uint32, n uint64) uint64 {
	k := uint64(1)
	if tier == 1 {
		k = 6
	}
	if tier >= 2 {
		k = 15
	}
	if k > n {
		k = n
	}
	return k
}

// Partial Fisher-Yates with rejection sampling avoids modulo bias. Domains are
// independent of both auditor selection and all Merkle/spool domains.
func DeriveSpotSteps(seed []byte, n, k uint64) ([]uint64, error) {
	if len(seed) != 32 || n == 0 || n > 1000000 || k == 0 || k > n {
		return nil, errors.New("invalid spot draw")
	}
	pool := make([]uint64, n)
	for i := range pool {
		pool[i] = uint64(i) + 1
	}
	counter := uint64(0)
	for i := uint64(0); i < k; i++ {
		bound := n - i
		var x uint64
		for {
			var b [8]byte
			binary.BigEndian.PutUint64(b[:], counter)
			counter++
			h := sha256.New()
			h.Write([]byte("EMBER-SPOT-STEPS-V1"))
			h.Write(seed)
			h.Write(b[:])
			x = binary.BigEndian.Uint64(h.Sum(nil)[:8])
			if x >= (-bound)%bound {
				break
			}
		}
		j := i + x%bound
		pool[i], pool[j] = pool[j], pool[i]
	}
	out := append([]uint64(nil), pool[:k]...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// Additional draws use the same fixed post-submission finalized entropy as the
// initial challenge, domain separated and never redrawn by an auditor. k=15
// matches the strongest initial tier: with >=2 bad steps, seeking a second bad
// step deters multi-step cheating while bounding the opening package.
func DeriveBoundedSpotSteps(seed []byte, n uint64, initial []uint64, first *pb.AuditVerdict) ([]uint64, error) {
	h := sha256.New()
	h.Write([]byte("EMBER-SPOT-ESC-BOUNDED-V15"))
	h.Write(seed)
	if len(seed) != 32 {
		return nil, errors.New("invalid escalation seed")
	}
	if n == 0 || n > 1000000 {
		return nil, errors.New("invalid escalation count")
	}
	excluded := map[uint64]bool{}
	for _, step := range initial {
		excluded[step] = true
	}
	out := []uint64{}
	for _, p := range first.PerStep {
		if p.Outcome != "match" {
			out = append(out, p.Step)
			excluded[p.Step] = true
		}
	}
	// Sample ranks in the canonical eligible population, excluding all initial steps.
	available := []uint64{}
	for step := uint64(1); step <= n; step++ {
		if !excluded[step] {
			available = append(available, step)
		}
	}
	if len(available) > 0 {
		k := uint64(SPOT_ESCALATION_K)
		if k > uint64(len(available)) {
			k = uint64(len(available))
		}
		ranks, err := DeriveSpotSteps(h.Sum(nil), uint64(len(available)), k)
		if err != nil {
			return nil, err
		}
		for _, rank := range ranks {
			out = append(out, available[rank-1])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func boundedSpotConviction(first, second *pb.AuditVerdict) bool {
	initial, disputed := map[uint64]bool{}, map[uint64]bool{}
	for _, p := range first.PerStep {
		initial[p.Step] = true
		disputed[p.Step] = p.Outcome != "match"
	}
	corroborated, additional := false, false
	for _, p := range second.PerStep {
		if p.Outcome != "match" {
			corroborated = corroborated || disputed[p.Step]
			additional = additional || !initial[p.Step]
		}
	}
	return corroborated && additional
}

// Aggregate opening obligations belong to credited execution participants, not
// merely authorized standby nodes. Preserve canonical sorted unique identities.
func spotExecutors(h *Hold) []string {
	ids := []string{}
	seen := map[string]bool{}
	for _, credit := range h.Pipeline.Credits {
		if !seen[credit.NodeID] {
			seen[credit.NodeID] = true
			ids = append(ids, credit.NodeID)
		}
	}
	sort.Strings(ids)
	return ids
}

func (s *State) availability(id string) *AvailabilityRecord {
	if s.Availability == nil {
		s.Availability = map[string]*AvailabilityRecord{}
	}
	if s.Availability[id] == nil {
		s.Availability[id] = &AvailabilityRecord{}
	}
	return s.Availability[id]
}
func (r *AvailabilityRecord) refresh(height int64) {
	kept := r.Events[:0]
	r.Minor = 0
	r.Major = 0
	for _, e := range r.Events {
		window := AVAIL_MINOR_WINDOW
		if e.Major {
			window = AVAIL_MAJOR_WINDOW
		}
		if e.Height <= height && height-e.Height < window {
			kept = append(kept, e)
			if e.Major {
				r.Major++
			} else {
				r.Minor++
			}
		}
	}
	r.Events = kept
	r.Level = 0
	if r.Minor >= AVAIL_T1_MINOR || r.Major >= AVAIL_T1_MAJOR {
		r.Level = 1
	}
	if r.Minor >= AVAIL_T2_MINOR || r.Major >= AVAIL_T2_MAJOR {
		r.Level = 2
	}
	if r.Minor >= AVAIL_T3_MINOR || r.Major >= AVAIL_T3_MAJOR {
		r.Level = 3
	}
}
func (s *State) availabilityEvent(id string, major bool, height int64) {
	r := s.availability(id)
	r.Events = append(r.Events, AvailabilityEvent{height, major})
	r.refresh(height)
	// Pause begins/extends only on a new violation at T2/T3, never per block.
	if r.Level >= 2 {
		r.PauseUntil = height + AVAIL_PAUSE_BLOCKS
	}
	// Each new major event whose post-event level is T3 burns min(stake,1000).
	// Minor events and block refreshes never charge; insufficient stake floors at zero.
	if major && r.Level == 3 {
		if a := s.Accounts[id]; a != nil {
			fine := AVAIL_FINE_SUB
			if a.Stake < fine {
				fine = a.Stake
			}
			a.Stake -= fine
		}
	}
}

func (s *State) startSpot(h *Hold, height int64) error {
	if h.Pipeline.Spot != nil {
		return errors.New("challenge already fixed")
	}
	seed, _, err := s.assignmentSeed(h)
	if err != nil {
		return err
	}
	tier := uint32(0)
	if h.SamplingPolicy != nil {
		tier = uint32(h.SamplingPolicy.Tier)
	}
	steps, err := DeriveSpotSteps(seed, h.Pipeline.SpotStepCount, SpotK(tier, h.Pipeline.SpotStepCount))
	if err != nil {
		return err
	}
	h.Pipeline.Spot = &SpotChallenge{RequestID: h.ID, Auditor: h.Auditor, StepSet: steps, ChallengeHeight: height, Round: "1", Deadline: height + SPOT_OPEN_ROUND_BLOCKS, VerdictDeadline: height + SPOT_OPEN_ROUND_BLOCKS + SPOT_FIRST_VERDICT_BLOCKS}
	for _, id := range spotExecutors(h) {
		s.availability(id).Executor.ChallengedTotal++
	}
	s.availability(h.Auditor).Auditor.ChallengedTotal++
	return nil
}
func (s *State) verifySpool(h *Hold, a *SpoolAttestation, height int64) error {
	return s.verifySpotSpool(h.Pipeline.Spot, a, height)
}
func (s *State) verifySpotSpool(c *SpotChallenge, a *SpoolAttestation, height int64) error {
	if a == nil || a.RequestID != c.RequestID || a.ChallengeHeight != c.ChallengeHeight || !bytes.Equal(a.StepSetHash, SpotStepSetHash(c.StepSet)) || a.Height > height || a.Height < c.ChallengeHeight {
		return errors.New("spool challenge binding mismatch")
	}
	p := s.ActiveProfile(a.Gateway, height)
	if p == nil || !HasRole(p, "gateway") || len(p.Signer) != ed25519.PublicKeySize || core.NodeID(p.Signer) != a.Gateway || !ed25519.Verify(p.Signer, SpoolSigningBytes(*a), a.Signature) {
		return errors.New("invalid registered gateway signature")
	}
	return nil
}
func (s *State) spotReceived(h *Hold, height int64) {
	c := h.Pipeline.Spot
	if c.Received {
		return
	}
	c.Received = true
	c.Accusation = nil
	c.AccusedAt = 0
	if c.Round == "2" {
		for _, id := range spotExecutors(h) {
			s.availabilityEvent(id, false, height)
		}
	}
	c.VerdictDeadline = height + SPOT_FIRST_VERDICT_BLOCKS
	if c.Round == "esc" {
		c.VerdictDeadline = c.Deadline
	}
	if c.Substitute {
		c.VerdictDeadline = h.AuditAssignedHeight + SPOT_SUBSTITUTE_BLOCKS
		if c.Round == "esc" {
			c.VerdictDeadline = min(c.VerdictDeadline, c.Deadline)
		}
	}
}
func (s *State) spotAction(signer string, a Action, height int64) error {
	h := s.Holds[a.HoldID]
	if s.Version >= 14 && h != nil && h.Pipeline != nil && h.Pipeline.Spot != nil && a.Spool != nil {
		c := h.Pipeline.Spot
		for i := range c.History {
			old := &c.History[i]
			if old.ChallengeHeight == a.Spool.ChallengeHeight {
				return s.historicalSpotAction(h, old, signer, a, height)
			}
		}
		if h.AuditVerdict == "unconfirmed_delivery_timeout" && c.ChallengeHeight == a.Spool.ChallengeHeight {
			return s.historicalSpotAction(h, c, signer, a, height)
		}
	}
	if s.Version < 14 || h == nil || h.State != "dispute" || h.Pipeline == nil || h.Pipeline.Spot == nil || h.AuditVerdict != "" {
		return errors.New("invalid spot transition")
	}
	c := h.Pipeline.Spot
	switch a.Type {
	case "spot_received":
		if signer != h.Auditor || c.Accusation != nil || height > c.VerdictDeadline {
			return errors.New("invalid receipt attestation")
		}
		s.spotReceived(h, height)
	case "spot_unopened":
		if signer != h.Auditor || c.Received || c.Accusation != nil || c.ReplaceAfter != 0 || height <= c.Deadline {
			return errors.New("invalid unopened accusation")
		}
		if err := s.verifySpool(h, a.Spool, height); err != nil {
			return err
		}
		if a.Spool.Received || a.Spool.Height < c.Deadline {
			return errors.New("invalid absence interval")
		}
		cp := *a.Spool
		c.Accusation = &cp
		c.AccusedAt = height
	case "spot_counterproof":
		found := false
		for _, id := range spotExecutors(h) {
			found = found || id == signer
		}
		if !found || c.Accusation == nil || height > c.AccusedAt+SPOT_FIRST_VERDICT_BLOCKS {
			return errors.New("invalid counterproof")
		}
		if err := s.verifySpool(h, a.Spool, height); err != nil {
			return err
		}
		if !a.Spool.Received || a.Spool.Height > c.Deadline {
			return errors.New("not a timely spool receipt")
		}
		s.availabilityEvent(c.Auditor, true, height)
		s.spotReceived(h, height)
	}
	return nil
}
func (s *State) rejectUnopened(h *Hold, height int64) error {
	for _, id := range spotExecutors(h) {
		s.availabilityEvent(id, true, height)
	}
	h.AuditVerdict = "reject_unopened"
	return s.settleAt(h, "service_failure", height)
}

// resendSpot preserves the committed step set and snapshots the prior challenge.
func (s *State) resendSpot(h *Hold, height int64) {
	c := h.Pipeline.Spot
	old := *c
	old.History = nil
	c.History = append(c.History, old)
	c.Round = "2"
	c.EvidenceResolved = false
	c.ChallengeHeight = height
	c.Deadline = height + SPOT_OPEN_ROUND_BLOCKS
	c.VerdictDeadline = c.Deadline + SPOT_FIRST_VERDICT_BLOCKS
	c.Substitute = false
	c.Accusation = nil
	c.AccusedAt = 0
	for _, id := range spotExecutors(h) {
		s.availability(id).Executor.ChallengedTotal++
	}
}
func (s *State) advanceSpot(height int64) error {
	if s.Version < 14 {
		return nil
	}
	for _, r := range s.Availability {
		r.refresh(height)
	}
	ids := make([]string, 0, len(s.Holds))
	for id := range s.Holds {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		h := s.Holds[id]
		if h.State == "dispute" && h.Pipeline != nil && len(h.Pipeline.CommitmentRoot) == 32 && h.AuditVerdict == "" && height >= h.SettleHeight {
			h.AuditVerdict = "service_failure"
			if err := s.settleAt(h, "service_failure", height); err != nil {
				return err
			}
			s.advanceHistoricalSpot(h, height)
			continue
		}
		s.advanceHistoricalSpot(h, height)
		if h.State != "dispute" || h.Pipeline == nil || h.Pipeline.Spot == nil || h.AuditVerdict != "" {
			continue
		}
		c := h.Pipeline.Spot
		if c.Accusation != nil && height > c.AccusedAt+SPOT_FIRST_VERDICT_BLOCKS {
			for _, id := range spotExecutors(h) {
				s.availability(id).Executor.UnopenedTotal++
			}
			c.EvidenceResolved = true
			if c.Round != "1" {
				if err := s.rejectUnopened(h, height); err != nil {
					return err
				}
				continue
			}
			s.resendSpot(h, height)
		}
		// Pending evidence owns this round until its counterproof window closes.
		// A missing delivery confirmation advances funds, never attribution.
		if !c.Received && c.Accusation == nil && height > c.Deadline {
			if c.Round == "1" {
				s.resendSpot(h, height)
			} else {
				h.AuditVerdict = "unconfirmed_delivery_timeout"
				if err := s.settleAt(h, "service_failure", height); err != nil {
					return err
				}
			}
			continue
		}
		if c.Received && c.ReplaceAfter > 0 && height > c.ReplaceAfter+1 {
			// The escalation budget belongs to the segment, not each auditor.
			// Do not assign an already-expired budget and blame the new auditor.
			if c.Round == "esc" && height > c.Deadline {
				h.AuditVerdict = "service_failure"
				if err := s.settleAt(h, "service_failure", height); err != nil {
					return err
				}
				continue
			}
			next, proof, err := s.selectAuditor(h, h.Auditor, height)
			if err != nil {
				continue
			}
			h.Auditor = next
			h.AuditProof = hex.EncodeToString(proof)
			h.AuditAssignedHeight = height
			c.Auditor = next
			c.VerdictDeadline = height + SPOT_SUBSTITUTE_BLOCKS
			if c.Round == "esc" {
				c.VerdictDeadline = min(c.VerdictDeadline, c.Deadline)
			}
			c.ReplaceAfter = 0
			c.Substitute = true
			s.availability(next).Auditor.ChallengedTotal++
		}
		if c.Received && c.Accusation == nil && c.ReplaceAfter == 0 && height > c.VerdictDeadline {
			s.availabilityEvent(h.Auditor, false, height)
			s.availability(h.Auditor).Auditor.UnopenedTotal++
			c.ReplaceAfter = height
		}
	}
	return nil
}
func (s *State) validateSpotVerdict(h *Hold, raw []byte, height int64) error {
	c := h.Pipeline.Spot
	v := &pb.AuditVerdict{}
	if proto.Unmarshal(raw, v) != nil || c.Accusation != nil || c.ReplaceAfter != 0 {
		return errors.New("invalid spot verdict state")
	}
	if !c.Received {
		if height > c.VerdictDeadline {
			return errors.New("verdict deadline passed")
		}
		s.spotReceived(h, height)
	}
	if height > c.VerdictDeadline {
		return errors.New("verdict deadline passed")
	}
	expected := c.StepSet
	if c.Round == "esc" && s.Version < 15 {
		expected = make([]uint64, h.Pipeline.SpotStepCount)
		for i := range expected {
			expected[i] = uint64(i) + 1
		}
	}
	if len(v.PerStep) != len(expected) {
		return errors.New("incomplete per_step")
	}
	max := float64(0)
	bad := false
	for i, p := range v.PerStep {
		if p.Step != expected[i] || math.IsNaN(p.FirstDivergenceDistance) || math.IsInf(p.FirstDivergenceDistance, 0) || p.FirstDivergenceDistance < 0 {
			return errors.New("invalid step result")
		}
		switch p.Outcome {
		case "match":
		case "mismatch", "binding_fail", "format_fail":
			bad = true
		default:
			return errors.New("invalid step outcome")
		}
		if p.FirstDivergenceDistance > max {
			max = p.FirstDivergenceDistance
		}
	}
	kind := "accept"
	verdict := "accept"
	if bad {
		kind = "reject_step"
		verdict = "reject"
	}
	if c.Round == "esc" {
		kind = "escalation_full_accept"
		if bad {
			kind = "escalation_full_reject"
		}
	}
	if c.Round == "esc" && s.Version >= 15 {
		verdict, kind = "accept", "escalation_bounded_accept"
		first := &pb.AuditVerdict{}
		if proto.Unmarshal(c.FirstVerdict, first) != nil {
			return errors.New("invalid first verdict")
		}
		if boundedSpotConviction(first, v) {
			verdict, kind = "reject", "escalation_bounded_reject"
		}
	}
	if v.VerdictKind != kind || v.VerdictDistance != max || v.Verdict != verdict {
		return errors.New("inconsistent spot verdict")
	}
	return nil
}
func (s *State) spotOutcome(h *Hold, signer, outcome, evidence string, at ...int64) error {
	height := s.Height
	if len(at) > 0 {
		height = at[0]
	}
	c := h.Pipeline.Spot
	if c.Round == "esc" {
		if outcome == "accept" {
			c.GoodFaithAuditor = h.FirstAuditor
			h.AuditVerdict = "accept"
			s.Evidence[evidence] = &WorkEvidence{HoldID: h.ID, Node: h.Node, Reference: evidence}
			return nil
		}
		first := &pb.AuditVerdict{}
		second := &pb.AuditVerdict{}
		if proto.Unmarshal(c.FirstVerdict, first) != nil || proto.Unmarshal(h.VerdictBytes, second) != nil {
			return errors.New("invalid escalation evidence")
		}
		same := false
		for _, a := range first.PerStep {
			for _, b := range second.PerStep {
				if a.Step == b.Step && a.Outcome != "match" && b.Outcome != "match" {
					same = true
				}
			}
		}
		if !same {
			return errors.New("second reject does not corroborate same step")
		}
		if s.Version >= 15 && !boundedSpotConviction(first, second) {
			return errors.New("bounded reject lacks an additional bad step")
		}
		h.AuditVerdict = "reject"
		return nil
	}
	if outcome == "accept" {
		h.AuditVerdict = "accept"
		s.Evidence[evidence] = &WorkEvidence{HoldID: h.ID, Node: h.Node, Reference: evidence}
		return nil
	}
	h.FirstAuditor = signer
	h.FirstVerdict = "reject"
	c.FirstVerdict = append([]byte(nil), h.VerdictBytes...)
	next, proof, err := s.selectAuditor(h, signer, height)
	if err != nil {
		h.AuditVerdict = "service_failure"
		return nil
	}
	old := *c
	old.History = nil
	c.History = append(c.History, old)
	if s.Version >= 15 {
		seed, _, err := s.assignmentSeed(h)
		if err != nil {
			return err
		}
		first := &pb.AuditVerdict{}
		if proto.Unmarshal(c.FirstVerdict, first) != nil {
			return errors.New("invalid first verdict")
		}
		steps, err := DeriveBoundedSpotSteps(seed, h.Pipeline.SpotStepCount, c.StepSet, first)
		if err != nil {
			return err
		}
		c.StepSet = steps
	} else {
		c.StepSet = make([]uint64, h.Pipeline.SpotStepCount)
		for i := range c.StepSet {
			c.StepSet[i] = uint64(i) + 1
		}
	}
	c.Round = "esc"
	c.Auditor = next
	c.ChallengeHeight = height
	c.Deadline = height + SPOT_ESCALATION_BLOCKS
	if s.Version < 15 {
		c.Deadline = height + 350
	}
	c.Received = false
	c.VerdictDeadline = c.Deadline
	c.Substitute = false
	h.Auditor = next
	h.AuditProof = hex.EncodeToString(proof)
	h.AuditRound = 2
	h.AuditAssignedHeight = height
	h.VerdictBytes = nil
	h.AuditRef = ""
	h.EvidenceRef = ""
	s.availability(next).Auditor.ChallengedTotal++
	for _, id := range spotExecutors(h) {
		s.availability(id).Executor.ChallengedTotal++
	}
	return nil
}

// Both signed verdicts have already passed Apply's identity and challenge
// checks. Recheck their positive, same-step corroboration before settlement.
func confirmedSpotConviction(h *Hold) bool {
	if h.Pipeline == nil || h.Pipeline.Spot == nil || h.Pipeline.Spot.Round != "esc" || h.AuditVerdict != "reject" || h.FirstAuditor == "" || h.FirstAuditor == h.Auditor {
		return false
	}
	first, second := &pb.AuditVerdict{}, &pb.AuditVerdict{}
	if proto.Unmarshal(h.Pipeline.Spot.FirstVerdict, first) != nil || proto.Unmarshal(h.VerdictBytes, second) != nil || first.Verdict != "reject" || second.Verdict != "reject" {
		return false
	}
	if second.VerdictKind == "escalation_bounded_reject" {
		return boundedSpotConviction(first, second)
	}
	for _, a := range first.PerStep {
		for _, b := range second.PerStep {
			if a.Step == b.Step && a.Outcome != "match" && b.Outcome != "match" {
				return true
			}
		}
	}
	return false
}

// Historical evidence affects attribution only: never revive or repay a hold.
// New claims close 60 blocks before SettleHeight, so every claim retains its
// complete counterproof window within the existing total dispute budget.
func (s *State) historicalSpotAction(h *Hold, c *SpotChallenge, signer string, a Action, height int64) error {
	if height >= h.SettleHeight || (a.Type == "spot_unopened" && height >= h.SettleHeight-SPOT_FIRST_VERDICT_BLOCKS) || c.EvidenceResolved || c.Received {
		return errors.New("historical evidence closed")
	}
	if err := s.verifySpotSpool(c, a.Spool, height); err != nil {
		return err
	}
	switch a.Type {
	case "spot_unopened":
		if signer != c.Auditor || c.Accusation != nil || height <= c.Deadline || a.Spool.Received || a.Spool.Height < c.Deadline {
			return errors.New("invalid historical accusation")
		}
		cp := *a.Spool
		c.Accusation = &cp
		c.AccusedAt = height
	case "spot_counterproof":
		executor := false
		for _, id := range spotExecutors(h) {
			executor = executor || signer == id
		}
		if !executor || c.Accusation == nil || height > c.AccusedAt+SPOT_FIRST_VERDICT_BLOCKS || !a.Spool.Received || a.Spool.Height > c.Deadline {
			return errors.New("invalid historical counterproof")
		}
		s.availabilityEvent(c.Auditor, true, height)
		c.Accusation = nil
		c.AccusedAt = 0
		c.Received = true
		c.EvidenceResolved = true
	default:
		return errors.New("invalid historical action")
	}
	return nil
}
func (s *State) advanceHistoricalSpot(h *Hold, height int64) {
	if h.Pipeline == nil || h.Pipeline.Spot == nil {
		return
	}
	current := h.Pipeline.Spot
	resolve := func(c *SpotChallenge) {
		if c.EvidenceResolved || c.Accusation == nil || height <= c.AccusedAt+SPOT_FIRST_VERDICT_BLOCKS {
			return
		}
		c.EvidenceResolved = true
		for _, id := range spotExecutors(h) {
			s.availability(id).Executor.UnopenedTotal++
		}
		if c.Round != "1" {
			for _, id := range spotExecutors(h) {
				s.availabilityEvent(id, true, height)
			}
			if h.AuditVerdict == "unconfirmed_delivery_timeout" {
				h.AuditVerdict = "reject_unopened"
			}
		}
	}
	for i := range current.History {
		resolve(&current.History[i])
	}
	if h.State != "dispute" {
		resolve(current)
	}
}
