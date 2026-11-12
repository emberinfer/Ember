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
	Version      uint32   `json:"version"`
	RequestID    string   `json:"request_id"`
	FreezeHeight int64    `json:"freeze_height"`
	SeedBlocks   [][]byte `json:"seed_blocks"`
	Pool         []string `json:"pool"`
	EligibleMask []bool   `json:"eligible_mask"`
	AuditorIDs   []string `json:"auditor_ids"`
	Seed         []byte   `json:"seed"`
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

func isFreeze(a Action) bool { return a.Type == "freeze" || a.Type == "freeze_pipeline" }

// Freeze identifiers used by existing clients remain lookup aliases. The
// consensus request identity is fixed once, when the payer's freeze is accepted.
func (s *State) pinRequest(env Envelope, raw []byte) {
	h := s.Holds[env.Action.HoldID]
	h.FreezePayload = freezePayload(env.Action)
	h.FreezeTransaction = append([]byte(nil), raw...)
	commitment := env.Action.PricingCommitment
	if len(commitment) == 0 {
		commitment = h.FreezePayload
	}
	h.RequestID = RequestID(h.Payer, env.Nonce, commitment)
	h.AssignmentPolicy = s.Parameters.Assignment
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
		r.EligibleMask[i] = true
		for _, executor := range executors {
			streak := s.PairStreaks[executor]
			if id == streak.Auditor && streak.Consecutive >= policy.CooldownRequests {
				r.EligibleMask[i] = false
			}
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
	if s.PairStreaks == nil {
		s.PairStreaks = map[string]PairStreak{}
	}
	for _, executor := range executors {
		streak := s.PairStreaks[executor]
		if streak.Auditor == auditor {
			streak.Consecutive++
		} else {
			streak = PairStreak{Auditor: auditor, Consecutive: 1}
		}
		s.PairStreaks[executor] = streak
	}
	return auditor, seed, nil
}
