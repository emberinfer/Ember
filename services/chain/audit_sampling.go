package chain

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math/big"
	"sort"
)

type AuditSamplingParameters struct {
	Enabled                   bool      `json:"enabled"`
	Tier1MinAuditedAccepts    uint64    `json:"tier1_min_audited_accepts"`
	Tier2MinAuditedAccepts    uint64    `json:"tier2_min_audited_accepts"`
	TierRatesPPM              [3]uint32 `json:"tier_rates_ppm"`
	AmountRefSubunits         int64     `json:"amount_ref_subunits"`
	ProgressMinActualSubunits int64     `json:"progress_min_actual_subunits"`
	PenaltyBoxRequests        uint64    `json:"penalty_box_requests"`
	PenaltyBoxRepeatRequests  uint64    `json:"penalty_box_repeat_requests"`
	ExposureDivisors          [3]int64  `json:"exposure_divisors"`
	PenaltyExposureDivisor    int64     `json:"penalty_exposure_divisor"`
	CooldownScaleEnabled      bool      `json:"cooldown_scale_enabled"`
	CooldownFallbackEnabled   bool      `json:"cooldown_fallback_enabled"`
	CooldownCountOnCompletion bool      `json:"cooldown_count_on_completion"`
}

func (p *AuditSamplingParameters) defaults() {
	// Initialize the complete absent block; explicit false rollback switches survive.
	if *p == (AuditSamplingParameters{}) || *p == (AuditSamplingParameters{Enabled: true}) {
		p.CooldownScaleEnabled = true
		p.CooldownFallbackEnabled = true
		p.CooldownCountOnCompletion = true
	}
	if p.Tier1MinAuditedAccepts == 0 {
		p.Tier1MinAuditedAccepts = 20
	}
	if p.Tier2MinAuditedAccepts == 0 {
		p.Tier2MinAuditedAccepts = 200
	}
	if p.TierRatesPPM == [3]uint32{} {
		p.TierRatesPPM = [3]uint32{300000, 50000, 20000}
	}
	if p.AmountRefSubunits == 0 {
		p.AmountRefSubunits = 1000
	}
	if p.ProgressMinActualSubunits == 0 {
		p.ProgressMinActualSubunits = 16
	}
	if p.PenaltyBoxRequests == 0 {
		p.PenaltyBoxRequests = 100
	}
	if p.PenaltyBoxRepeatRequests == 0 {
		p.PenaltyBoxRepeatRequests = 500
	}
	if p.ExposureDivisors == [3]int64{} {
		p.ExposureDivisors = [3]int64{5, 4, 3}
	}
	if p.PenaltyExposureDivisor == 0 {
		p.PenaltyExposureDivisor = 10
	}
}
func (p AuditSamplingParameters) valid() bool {
	if p.Tier1MinAuditedAccepts == 0 || p.Tier2MinAuditedAccepts <= p.Tier1MinAuditedAccepts || p.AmountRefSubunits <= 0 || p.ProgressMinActualSubunits <= 0 || p.PenaltyBoxRequests == 0 || p.PenaltyBoxRepeatRequests < p.PenaltyBoxRequests || p.PenaltyExposureDivisor < 1 {
		return false
	}
	for i, r := range p.TierRatesPPM {
		if r == 0 || r > 1000000 || p.ExposureDivisors[i] < 1 || i > 0 && r > p.TierRatesPPM[i-1] {
			return false
		}
	}
	return true
}

type NodeAuditStats struct {
	AuditedAccepts          uint64 `json:"audited_accepts"`
	AuditedRejectsConfirmed uint64 `json:"audited_rejects_confirmed"`
	SettledTotal            uint64 `json:"settled_total"`
	AuditsCompleted         uint64 `json:"audits_completed"`
	AuditsMisjudged         uint64 `json:"audits_misjudged"`
	LastOffenseHeight       int64  `json:"last_offense_height"`
	PenaltyBoxRemaining     uint64 `json:"penalty_box_remaining"`
}

func AuditTier(n NodeAuditStats, p AuditSamplingParameters) uint32 {
	if n.PenaltyBoxRemaining > 0 {
		return 0
	}
	if n.AuditedAccepts >= p.Tier2MinAuditedAccepts {
		return 2
	}
	if n.AuditedAccepts >= p.Tier1MinAuditedAccepts {
		return 1
	}
	return 0
}

type AuditSamplingSnapshot struct {
	Policy          AuditSamplingParameters `json:"policy"`
	Tier            uint32                  `json:"tier"`
	RatePPM         uint32                  `json:"rate_ppm"`
	ExposureDivisor int64                   `json:"exposure_divisor"`
	Nodes           map[string]int64        `json:"node_exposure_divisors"`
}
type SamplingRecord struct {
	Version          uint32                `json:"version"`
	RequestID        string                `json:"request_id"`
	FreezeHeight     int64                 `json:"freeze_height"`
	Snapshot         AuditSamplingSnapshot `json:"snapshot"`
	Amount           int64                 `json:"amount"`
	EffectiveRatePPM uint32                `json:"effective_rate_ppm"`
	Roll             uint64                `json:"roll"`
	SeedBlocks       [][]byte              `json:"seed_blocks"`
	Seed             []byte                `json:"seed"`
	Sampled          bool                  `json:"sampled"`
}

func samplingRate(rate uint32, amount, ref int64) uint32 {
	if amount <= ref {
		return rate
	}
	n := new(big.Int).Mul(big.NewInt(amount), new(big.Int).SetUint64(uint64(rate)))
	n.Quo(n, big.NewInt(ref))
	if n.Cmp(big.NewInt(1000000)) >= 0 {
		return 1000000
	}
	return uint32(n.Uint64())
}
func RecomputeSampling(r SamplingRecord) (bool, error) {
	if r.Version != 1 || !r.Snapshot.Policy.valid() || r.Snapshot.Tier > 2 || r.Amount <= 0 || len(r.SeedBlocks) == 0 || len(r.SeedBlocks) > 64 {
		return false, errors.New("invalid sampling record")
	}
	id, err := hex.DecodeString(r.RequestID)
	if err != nil || len(id) != 32 {
		return false, errors.New("invalid sampling request identity")
	}
	h := sha256.New()
	h.Write(id)
	for _, b := range r.SeedBlocks {
		if len(b) != 32 {
			return false, errors.New("invalid sampling entropy")
		}
		h.Write(b)
	}
	seed := h.Sum(nil)
	roll := binary.BigEndian.Uint64(seed[:8]) % 1000000
	rate := samplingRate(r.Snapshot.RatePPM, r.Amount, r.Snapshot.Policy.AmountRefSubunits)
	if r.Snapshot.RatePPM == 0 || r.Snapshot.RatePPM > 1000000 || !bytes.Equal(seed, r.Seed) || roll != r.Roll || rate != r.EffectiveRatePPM {
		return false, errors.New("sampling input mismatch")
	}
	return roll < uint64(rate), nil
}
func holdExecutors(h *Hold) []string {
	ids := map[string]bool{}
	if h.Node != "" {
		ids[h.Node] = true
	}
	if h.Pipeline != nil {
		for _, a := range h.Pipeline.Authorizations {
			ids[a.NodeID] = true
		}
	}
	if h.Moe != nil {
		for _, a := range h.Moe.Authorizations {
			ids[a.NodeID] = true
		}
	}
	out := []string{}
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
func (s *State) samplingSnapshot(ids []string) *AuditSamplingSnapshot {
	p := s.Parameters.AuditSampling
	if s.Version < 8 || !p.Enabled {
		return nil
	}
	snap := &AuditSamplingSnapshot{Policy: p, RatePPM: p.TierRatesPPM[2], Tier: 2, ExposureDivisor: p.ExposureDivisors[2], Nodes: map[string]int64{}}
	for _, id := range ids {
		n := s.NodeAuditStats[id]
		tier := AuditTier(n, p)
		rate := p.TierRatesPPM[tier]
		div := p.ExposureDivisors[tier]
		if n.PenaltyBoxRemaining > 0 {
			rate = 1000000
			div = p.PenaltyExposureDivisor
		}
		snap.Nodes[id] = div
		if rate > snap.RatePPM || rate == snap.RatePPM && tier < snap.Tier {
			snap.Tier = tier
			snap.RatePPM = rate
			snap.ExposureDivisor = div
		}
	}
	return snap
}
func (s *State) freezeSampling(h *Hold) {
	h.SamplingPolicy = s.samplingSnapshot(holdExecutors(h))
	if s.Version >= 8 {
		for _, id := range holdExecutors(h) {
			s.auditStatsEvent(h, AuditStatsEvent{Kind: "freeze", Node: id, Height: h.FreezeHeight})
		}
	}
}
func (s *State) exposureDivisor(h *Hold, id string) int64 {
	if h != nil && h.SamplingPolicy != nil {
		if div := h.SamplingPolicy.Nodes[id]; div > 0 {
			return div
		}
		return h.SamplingPolicy.ExposureDivisor
	}
	return 5
}
func (s *State) newExposureDivisor(id string) int64 {
	if p := s.samplingSnapshot([]string{id}); p != nil {
		return p.Nodes[id]
	}
	return 5
}
func (s *State) resolveSampling(h *Hold) (bool, error) {
	if h.SamplingPolicy == nil || !h.SamplingPolicy.Policy.Enabled {
		return true, nil
	}
	if h.Sampling != nil {
		return h.Sampling.Sampled, nil
	}
	seed, blocks, err := s.assignmentSeed(h)
	if err != nil {
		return false, err
	}
	r := &SamplingRecord{Version: 1, RequestID: h.RequestID, FreezeHeight: h.FreezeHeight, Snapshot: *h.SamplingPolicy, Amount: h.Amount, Seed: seed, SeedBlocks: blocks}
	r.Roll = binary.BigEndian.Uint64(seed[:8]) % 1000000
	r.EffectiveRatePPM = samplingRate(r.Snapshot.RatePPM, h.Amount, r.Snapshot.Policy.AmountRefSubunits)
	r.Sampled = r.Roll < uint64(r.EffectiveRatePPM)
	h.Sampling = r
	h.Unaudited = !r.Sampled
	return r.Sampled, nil
}

// AuditStatsEvent is the shared counter definition for incremental transitions
// and the once-only migration fold. Events keep exact post-upgrade chronology.
type AuditStatsEvent struct {
	Kind    string `json:"kind"`
	Node    string `json:"node"`
	Height  int64  `json:"height"`
	Actual  int64  `json:"actual,omitempty"`
	Audited bool   `json:"audited,omitempty"`
}

func applyAuditStatsEvent(stats map[string]NodeAuditStats, e AuditStatsEvent, p AuditSamplingParameters, repeat int64) {
	n := stats[e.Node]
	switch e.Kind {
	case "freeze":
		if n.PenaltyBoxRemaining > 0 {
			n.PenaltyBoxRemaining--
		}
	case "settle":
		n.SettledTotal++
		if e.Audited && e.Actual >= p.ProgressMinActualSubunits {
			n.AuditedAccepts++
		}
	case "audit":
		n.AuditsCompleted++
	case "misjudge":
		n.AuditsMisjudged++
	case "slash":
		n.AuditedRejectsConfirmed++
		n.AuditedAccepts = 0 // Confirmed fraud destroys accumulated tier progress.
		box := p.PenaltyBoxRequests
		if n.LastOffenseHeight > 0 && e.Height-n.LastOffenseHeight < repeat {
			box = p.PenaltyBoxRepeatRequests
		}
		n.LastOffenseHeight = e.Height
		n.PenaltyBoxRemaining = box
	}
	stats[e.Node] = n
}
func (s *State) auditStatsEvent(h *Hold, e AuditStatsEvent) {
	if s.Version < 8 {
		return
	}
	if s.NodeAuditStats == nil {
		s.NodeAuditStats = map[string]NodeAuditStats{}
	}
	h.AuditStatsEvents = append(h.AuditStatsEvents, e)
	applyAuditStatsEvent(s.NodeAuditStats, e, s.Parameters.AuditSampling, s.Parameters.SlashRepeatWindow)
}
func (s *State) auditCompleted(h *Hold, height int64) {
	if h.AuditVerdict != "accept" && h.AuditVerdict != "reject" {
		return
	}
	if h.AuditCompletedHeight != 0 {
		return
	}
	if s.Version >= 8 {
		h.AuditCompletedHeight = height
	}
	s.auditStatsEvent(h, AuditStatsEvent{Kind: "audit", Node: h.Auditor, Height: height, Actual: h.Actual})
	if h.AuditVerdict == "accept" && h.FirstVerdict == "reject" && h.FirstAuditor != "" {
		s.auditStatsEvent(h, AuditStatsEvent{Kind: "misjudge", Node: h.FirstAuditor, Height: height})
	}
	if s.cooldownPolicy().CooldownCountOnCompletion {
		s.completePairStreaks(h, h.Auditor)
	}
}
func (s *State) recordSettledStats(h *Hold, height int64) {
	if h.State != "settled" {
		return
	}
	for _, id := range holdExecutors(h) {
		s.auditStatsEvent(h, AuditStatsEvent{Kind: "settle", Node: id, Height: height, Actual: h.Actual, Audited: !h.Unaudited && h.AuditVerdict == "accept"})
	}
}
func (s *State) migrateAuditSampling() {
	s.Parameters.AuditSampling.defaults()
	if s.Parameters.SupplyDemand == (PriceParameters{}) {
		s.Parameters.SupplyDemand = DefaultPriceParameters()
		s.PriceEpochStart = s.Height + 1
	}
	s.Parameters.Misjudge.ConsumerGray.defaults()
	stats := map[string]NodeAuditStats{}
	type entry struct {
		event AuditStatsEvent
		id    string
		index int
	}
	entries := []entry{}
	for id, h := range s.Holds {
		events := h.AuditStatsEvents
		if len(events) == 0 {
			// Legacy holds do not retain actual completion/settlement height. Their
			// persisted SettleHeight is the canonical fold ordering bound; do not
			// fabricate completed audit work for a service failure or an open verdict.
			for _, node := range holdExecutors(h) {
				events = append(events, AuditStatsEvent{Kind: "freeze", Node: node, Height: h.FreezeHeight})
			}
			if h.AuditVerdict == "accept" || h.AuditVerdict == "reject" {
				events = append(events, AuditStatsEvent{Kind: "audit", Node: h.Auditor, Height: h.SettleHeight, Actual: h.Actual})
				if h.AuditVerdict == "accept" && h.FirstVerdict == "reject" {
					events = append(events, AuditStatsEvent{Kind: "misjudge", Node: h.FirstAuditor, Height: h.SettleHeight})
				}
			}
			if h.State == "settled" {
				for _, node := range holdExecutors(h) {
					events = append(events, AuditStatsEvent{Kind: "settle", Node: node, Height: h.SettleHeight, Actual: h.Actual, Audited: !h.Unaudited && h.AuditVerdict == "accept"})
				}
			}
			for _, r := range h.Slashes {
				events = append(events, AuditStatsEvent{Kind: "slash", Node: r.NodeID, Height: r.Height})
			}
		}
		h.AuditStatsEvents = events
		for i, e := range events {
			entries = append(entries, entry{e, id, i})
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.event.Height != b.event.Height {
			return a.event.Height < b.event.Height
		}
		if a.id != b.id {
			return a.id < b.id
		}
		return a.index < b.index
	})
	for _, e := range entries {
		applyAuditStatsEvent(stats, e.event, s.Parameters.AuditSampling, s.Parameters.SlashRepeatWindow)
	}
	s.NodeAuditStats = stats
	// Governors seed (coordinator ruling, 2026-11-15): chains created
	// before governance existed carry an empty Governors set, so after
	// activation no account could ever sign set_pricing or
	// freeze_model_registration. When, and only when, the set is empty,
	// seed it from the chain's coordinators. The seed is a pure function
	// of state, so every validator derives the identical set.
	// Coordinators are fixed at genesis (no action mutates them); on
	// ember-testnet-1 the sole coordinator is
	// 90ef25ac87a1a59c3476123af952075a093354b6545df5117a5393d40fc565d0,
	// so the seeded Governors set there is exactly that account. A
	// non-empty Governors set is never touched.
	if len(s.Governors) == 0 {
		s.Governors = make(map[string]bool, len(s.Coordinators))
		for id, enabled := range s.Coordinators {
			if enabled {
				s.Governors[id] = true
			}
		}
	}
	s.Version = 8
}
func (s *State) cooldownPolicy() AuditSamplingParameters {
	if s.Version < 8 {
		return AuditSamplingParameters{}
	}
	return s.Parameters.AuditSampling
}
func (s *State) completePairStreaks(h *Hold, auditor string) {
	if s.PairStreaks == nil {
		s.PairStreaks = map[string]PairStreak{}
	}
	for _, id := range holdExecutors(h) {
		v := s.PairStreaks[id]
		if v.Auditor == auditor {
			v.Consecutive++
		} else {
			v = PairStreak{Auditor: auditor, Consecutive: 1}
		}
		s.PairStreaks[id] = v
	}
}
