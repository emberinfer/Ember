package chain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
)

// AssignmentParameters are consensus policy; defaults await staging calibration.
type AssignmentParameters struct {
	MBlocks          int64  `json:"m_blocks"`
	CooldownRequests uint64 `json:"cooldown_requests"`
}

// AssignmentRecord v1 carries the complete draw input, including excluded pool
// entries. SeedBlocks has M entries (three under the default governance policy).
type AssignmentRecord struct {
	FallbackCooldown     bool            `json:"fallback_cooldown,omitempty"`
	ExecutedStageIndices []uint32        `json:"executed_stage_indices,omitempty"`
	DrawnStageCount      uint32          `json:"drawn_stage_count,omitempty"`
	RangeProducerDevice  string          `json:"range_producer_device,omitempty"`
	RangeReferenceDevice string          `json:"range_reference_device,omitempty"`
	RangeThresholds      []ThresholdPair `json:"range_thresholds,omitempty"`
	DrawnStage           *DrawnStage     `json:"drawn_stage,omitempty"`
	Version              uint32          `json:"version"`
	RequestID            string          `json:"request_id"`
	FreezeHeight         int64           `json:"freeze_height"`
	SeedBlocks           [][]byte        `json:"seed_blocks"`
	Pool                 []string        `json:"pool"`
	EligibleMask         []bool          `json:"eligible_mask"`
	AuditorIDs           []string        `json:"auditor_ids"`
	Seed                 []byte          `json:"seed"`
}

type PairStreak struct {
	Auditor     string `json:"auditor"`
	Consecutive uint64 `json:"consecutive"`
}

// RequestID commits to the payer, request nonce and exact commitment bytes.
// Length framing avoids ambiguous concatenation; the nonce is big endian uint64.
func RequestID(payer string, nonce uint64, commitment []byte) string {
	h := sha256.New()
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(payer)))
	h.Write(n[:])
	h.Write([]byte(payer))
	binary.BigEndian.PutUint64(n[:], nonce)
	h.Write(n[:])
	h.Write(commitment)
	return hex.EncodeToString(h.Sum(nil))
}

func freezePayload(a Action) []byte {
	raw, _ := json.Marshal(a)
	return raw
}

func isFreeze(a Action) bool {
	return a.Type == "freeze" || a.Type == "freeze_pipeline" || a.Type == "freeze_moe"
}

// Freeze identifiers used by existing clients remain lookup aliases. The
// consensus request identity is fixed once, when the payer's freeze is accepted.
func (s *State) pinRequest(env Envelope, raw []byte) {
	h := s.Holds[env.Action.HoldID]
	h.ContributionProfile = h.ModelID
	if p := s.ActiveProfile(h.Node, h.FreezeHeight); p != nil {
		h.ContributionProfile = p.ChainId
	}
	h.FreezePayload = freezePayload(env.Action)
	h.FreezeTransaction = append([]byte(nil), raw...)
	commitment := env.Action.PricingCommitment
	if len(commitment) == 0 {
		commitment = h.FreezePayload
	}
	h.RequestID = RequestID(h.Payer, env.Nonce, commitment)
	h.AssignmentPolicy = s.Parameters.Assignment
	s.freezeSampling(h)
}

func (s *State) assignmentSeed(h *Hold) ([]byte, [][]byte, error) {
	policy := h.AssignmentPolicy
	if policy.MBlocks == 0 {
		policy = s.Parameters.Assignment
	}
	if policy.MBlocks < 1 || policy.MBlocks > 64 {
		return nil, nil, errors.New("invalid assignment entropy window")
	}
	request := h.RequestID
	if request == "" {
		return nil, nil, errors.New("request identity not frozen")
	}
	requestBytes, err := hex.DecodeString(request)
	if err != nil || len(requestBytes) != sha256.Size {
		return nil, nil, errors.New("invalid frozen request identity")
	}
	hash := sha256.New()
	hash.Write(requestBytes)
	blocks := make([][]byte, policy.MBlocks)
	for i := range blocks {
		block := s.BlockHashes[h.FreezeHeight+int64(i)+1]
		if len(block) != sha256.Size {
			return nil, nil, errors.New("future assignment entropy not finalized")
		}
		blocks[i] = append([]byte(nil), block...)
		hash.Write(block)
	}
	return hash.Sum(nil), blocks, nil
}

// RecomputeAssignment validates a record and returns its deterministic winner.
func RecomputeAssignment(r AssignmentRecord) (string, error) {
	if r.Version != 1 || len(r.Pool) != len(r.EligibleMask) || len(r.SeedBlocks) == 0 || !sort.StringsAreSorted(r.Pool) {
		return "", errors.New("invalid assignment record")
	}
	request, err := hex.DecodeString(r.RequestID)
	if err != nil || len(request) != sha256.Size {
		return "", errors.New("invalid request identity")
	}
	hash := sha256.New()
	hash.Write(request)
	for _, block := range r.SeedBlocks {
		if len(block) != sha256.Size {
			return "", errors.New("invalid seed block")
		}
		hash.Write(block)
	}
	seed := hash.Sum(nil)
	if r.DrawnStage != nil {
		if r.DrawnStageCount == 0 {
			return "", errors.New("drawn stage seed mismatch")
		}
		index := uint32(binary.BigEndian.Uint64(seed[:8]) % uint64(r.DrawnStageCount))
		if len(r.ExecutedStageIndices) > 0 {
			if len(r.ExecutedStageIndices) != int(r.DrawnStageCount) {
				return "", errors.New("invalid executed stage count")
			}
			for i, v := range r.ExecutedStageIndices {
				if i > 0 && v <= r.ExecutedStageIndices[i-1] {
					return "", errors.New("invalid executed stage indices")
				}
			}
			index = r.ExecutedStageIndices[index]
		}
		if r.DrawnStage.Index != index {
			return "", errors.New("drawn stage seed mismatch")
		}
	}
	if !equal(seed, r.Seed) {
		return "", errors.New("assignment seed mismatch")
	}
	eligible := []string{}
	for i, id := range r.Pool {
		if i > 0 && id == r.Pool[i-1] {
			return "", errors.New("duplicate pool member")
		}
		if r.EligibleMask[i] {
			eligible = append(eligible, id)
		}
	}
	if len(eligible) == 0 {
		return "", errors.New("no independent auditor available after cooldown")
	}
	return eligible[binary.BigEndian.Uint64(seed[:8])%uint64(len(eligible))], nil
}

func (s *State) drawAuditor(h *Hold, ids []string, at ...int64) (string, []byte, error) {
	seed, blocks, err := s.assignmentSeed(h)
	if err != nil {
		return "", nil, err
	}
	sort.Strings(ids)
	policy := h.AssignmentPolicy
	if policy.CooldownRequests == 0 {
		policy = s.Parameters.Assignment
	}
	executors := []string{h.Node}
	if h.Moe != nil {
		seen := map[string]bool{}
		executors = nil
		for _, a := range h.Moe.Authorizations {
			if !seen[a.NodeID] {
				seen[a.NodeID] = true
				executors = append(executors, a.NodeID)
			}
		}
		sort.Strings(executors)
	}
	if h.Pipeline != nil {
		seen := map[string]bool{}
		executors = nil
		for _, a := range h.Pipeline.Authorizations {
			if !seen[a.NodeID] {
				seen[a.NodeID] = true
				executors = append(executors, a.NodeID)
			}
		}
		sort.Strings(executors)
	}
	r := AssignmentRecord{Version: 1, RequestID: h.RequestID, FreezeHeight: h.FreezeHeight, SeedBlocks: blocks, Pool: ids, EligibleMask: make([]bool, len(ids)), Seed: seed}
	for i, id := range ids {
		height := s.Height
		if len(at) > 0 {
			height = at[0]
		}
		model := h.ContributionProfile
		if model == "" {
			model = h.ModelID
		}
		if model == "" {
			if p := s.ActiveProfile(h.Node, height); p != nil {
				model = p.ChainId
			}
		}
		r.EligibleMask[i] = s.livenessEligible(id) && s.ContributionAvailable(id, model, height, 1)
		if s.Version >= 8 {
			for _, executor := range executors {
				if id == executor {
					r.EligibleMask[i] = false
				}
			}
		}
		for _, prior := range h.Assignments {
			for _, selected := range prior.AuditorIDs {
				if selected == id {
					r.EligibleMask[i] = false
				}
			}
		}
	}
	hard := append([]bool(nil), r.EligibleMask...)
	n := uint64(0)
	for _, ok := range hard {
		if ok {
			n++
		}
	}
	cp := s.cooldownPolicy()
	cooldown := policy.CooldownRequests
	if cp.CooldownScaleEnabled {
		if n == 1 {
			// Persist relief even when scaling prevents mask construction.
			for i, id := range ids {
				if !hard[i] {
					continue
				}
				for _, executor := range executors {
					streak := s.PairStreaks[executor]
					if id == streak.Auditor && streak.Consecutive >= policy.CooldownRequests {
						r.FallbackCooldown = true
					}
				}
			}
			cooldown = 0
		} else if n < cooldown {
			cooldown = n
		}
	}
	for i, id := range ids {
		if !hard[i] {
			continue
		}
		for _, executor := range executors {
			streak := s.PairStreaks[executor]
			if cooldown > 0 && id == streak.Auditor && streak.Consecutive >= cooldown {
				r.EligibleMask[i] = false
			}
		}
	}
	any := false
	for _, ok := range r.EligibleMask {
		any = any || ok
	}
	if !any && n > 0 && cp.CooldownFallbackEnabled {
		// Only hard-eligible candidates can enter fallback; minimize the greatest
		// executor-pair streak, with ties resolved by the existing seeded ordering.
		best := ^uint64(0)
		counts := make([]uint64, len(ids))
		for i, id := range ids {
			if !hard[i] {
				continue
			}
			for _, executor := range executors {
				streak := s.PairStreaks[executor]
				if streak.Auditor == id && streak.Consecutive > counts[i] {
					counts[i] = streak.Consecutive
				}
			}
			if counts[i] < best {
				best = counts[i]
			}
		}
		for i := range ids {
			r.EligibleMask[i] = hard[i] && counts[i] == best
		}
		r.FallbackCooldown = true
	}
	if stage, _, err := s.drawPipelineStage(h); err != nil {
		return "", nil, err
	} else {
		r.DrawnStage = stage
		if stage != nil {
			indices, err := executedRangeIndices(h)
			if err != nil {
				return "", nil, err
			}
			r.ExecutedStageIndices = indices
			r.DrawnStageCount = uint32(len(indices))
		}
	}
	auditor, err := RecomputeAssignment(r)
	if err != nil {
		return "", nil, err
	}
	if err := s.reserveAuditWork(h, executors, auditor, at...); err != nil {
		return "", nil, err
	}
	r.AuditorIDs = []string{auditor}
	h.Assignments = append(h.Assignments, r)
	if !s.cooldownPolicy().CooldownCountOnCompletion {
		s.completePairStreaks(h, auditor)
	}
	return auditor, seed, nil
}
