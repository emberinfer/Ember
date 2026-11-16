package chain

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"testing"
)

func samplingState(t *testing.T) (*State, map[string]actor) {
	s, a := testState(t)
	s.Parameters.AuditSampling.Enabled = true
	return s, a
}
func sampleHold(s *State, a map[string]actor, i int) *Hold {
	h := &Hold{ID: fmt.Sprint("sample-", i), RequestID: RequestID(a["payer"].id, uint64(i), []byte("sealed")), Payer: a["payer"].id, Node: a["node"].id, ModelID: "ember-test-model", ProducerProfile: s.Nodes[a["node"].id].Profile, Amount: 100, Actual: 100, State: "dispute", FreezeHeight: 1, SubmitHeight: 3, SettleHeight: 6, AssignmentPolicy: s.Parameters.Assignment, AuditWorkPolicy: s.Parameters.AuditWork}
	s.freezeSampling(h)
	s.Holds[h.ID] = h
	return h
}
func TestSamplingRecomputeProperties(t *testing.T) {
	s, a := samplingState(t)
	rng := rand.New(rand.NewSource(31))
	for i := 0; i < 2000; i++ {
		h := sampleHold(s, a, i)
		h.Amount = rng.Int63n(100000) + 1
		h.SamplingPolicy.RatePPM = uint32(rng.Intn(1000000) + 1)
		sampled, err := s.resolveSampling(h)
		if err != nil {
			t.Fatal(err)
		}
		got, err := RecomputeSampling(*h.Sampling)
		if err != nil || got != sampled {
			t.Fatal("recompute", i, err)
		}
		seed, _, _ := s.assignmentSeed(h)
		if !bytes.Equal(seed, h.Sampling.Seed) {
			t.Fatal("different entropy domain")
		}
		h.Sampling.Roll ^= 1
		if _, err := RecomputeSampling(*h.Sampling); err == nil {
			t.Fatal("tampered roll accepted")
		}
	}
}
func TestSamplingTierBoundariesAndAmountGrid(t *testing.T) {
	p := AuditSamplingParameters{}
	p.defaults()
	for i, n := range []uint64{19, 20, 199, 200} {
		want := []uint32{0, 1, 1, 2}[i]
		if got := AuditTier(NodeAuditStats{AuditedAccepts: n}, p); got != want {
			t.Fatal(n, got)
		}
	}
	amounts := []int64{100, 1000, 3400, 20000, 50000, 100000, math.MaxInt64}
	for tier, rate := range p.TierRatesPPM {
		for _, amount := range amounts {
			want := math.Min(1000000, float64(rate)*math.Max(1, float64(amount)/1000))
			if got := samplingRate(rate, amount, 1000); got != uint32(want) {
				t.Fatal(tier, amount, got, want)
			}
		}
	}
	if !p.valid() || p.Enabled || p.TierRatesPPM[2] != 20000 {
		t.Fatal("defaults")
	}
	p.TierRatesPPM[2] = 0
	if p.valid() {
		t.Fatal("zero probability floor")
	}
}
func TestSamplingUnsampledSettlementThreePolicies(t *testing.T) {
	for _, mode := range []string{"unpriced", "pricing", "comp"} {
		t.Run(mode, func(t *testing.T) {
			s, a := samplingState(t)
			m := s.Models["ember-test-model"]
			if mode == "pricing" {
				m.Pricing = &RolePricing{1, 1, 80, 15, 5}
			}
			s.Models[m.ID] = m
			var h *Hold
			for i := 0; ; i++ {
				h = sampleHold(s, a, i)
				sampled, err := s.resolveSampling(h)
				if err != nil {
					t.Fatal(err)
				}
				if !sampled {
					break
				}
				delete(s.Holds, h.ID)
			}
			if mode == "comp" {
				h.AuditCompPolicy = auditCompFixture(t)
			}
			payer, node := s.Accounts[h.Payer].Balance, s.Accounts[h.Node].Balance
			rejected(t, s, a["coordinator"], Action{Type: "settle", HoldID: h.ID, Outcome: "accept"}, 5)
			rejected(t, s, a["coordinator"], Action{Type: "settle", HoldID: h.ID, Outcome: "reject"}, 6)
			apply(t, s, a["coordinator"], 1, Action{Type: "settle", HoldID: h.ID, Outcome: "accept"}, 6)
			h = s.Holds[h.ID]
			wantNode, wantPayer, wantPool := int64(100), int64(0), int64(0)
			if mode != "unpriced" {
				wantNode, wantPayer, wantPool = 80, 5, 15
			}
			if s.Accounts[h.Node].Balance-node != wantNode || s.Accounts[h.Payer].Balance-payer != wantPayer || s.AuditPool[h.ModelID][0] != wantPool || h.AuditEscrow != 0 || h.Auditor != "" {
				t.Fatalf("accounting %+v", h)
			}
			if s.NodeAuditStats[h.Node].AuditedAccepts != 0 || s.NodeAuditStats[h.Node].SettledTotal != 1 {
				t.Fatal("unsampled farmed tier")
			}
		})
	}
}
func TestSamplingDisabledLeavesNoSamplingRecord(t *testing.T) {
	s, a := testState(t)
	h := sampleHold(s, a, 0)
	if h.SamplingPolicy != nil {
		t.Fatal("disabled snapshot")
	}
	yes, err := s.resolveSampling(h)
	if err != nil || !yes || h.Sampling != nil || h.Unaudited {
		t.Fatal("disabled changed path")
	}
	p := Parameters{}
	raw, _ := json.Marshal(p)
	if bytes.Contains(raw, []byte("audit_sampling")) {
		t.Fatal("zero block changed legacy bytes")
	}
	raw, _ = json.Marshal(Hold{})
	if bytes.Contains(raw, []byte("sampling")) {
		t.Fatal("empty hold added sampling fields")
	}
}
func TestSamplingPenaltyStatsFoldAndDust(t *testing.T) {
	s, a := samplingState(t)
	h := sampleHold(s, a, 1)
	id := h.Node
	p := s.Parameters.AuditSampling
	for i := 0; i < 200; i++ {
		s.auditStatsEvent(h, AuditStatsEvent{Kind: "settle", Node: id, Height: 2, Actual: 16, Audited: true})
	}
	s.auditStatsEvent(h, AuditStatsEvent{Kind: "settle", Node: id, Height: 2, Actual: 15, Audited: true})
	if s.NodeAuditStats[id].AuditedAccepts != 200 {
		t.Fatal("dust counted")
	}
	if err := s.slashOffender(h, id, 1, 3); err != nil {
		t.Fatal(err)
	}
	n := s.NodeAuditStats[id]
	if n.AuditedAccepts != 0 || n.PenaltyBoxRemaining != 100 || AuditTier(n, p) != 0 {
		t.Fatal(n)
	}
	for i := 0; i < 10; i++ {
		snap := s.samplingSnapshot([]string{id})
		if samplingRate(snap.RatePPM, int64(i+1), 1000) != 1000000 || snap.ExposureDivisor != 10 {
			t.Fatal("penalty not mandatory")
		}
	}
	second := sampleHold(s, a, 2)
	if err := s.slashOffender(second, id, 1, 4); err != nil {
		t.Fatal(err)
	}
	if s.NodeAuditStats[id].PenaltyBoxRemaining != 500 {
		t.Fatal("repeat box")
	}
	before := s.NodeAuditStats
	s.migrateAuditSampling()
	if !reflect.DeepEqual(before, s.NodeAuditStats) {
		t.Fatal("incremental/fold disagreement", before, s.NodeAuditStats)
	}
	for i := 0; i < 500; i++ {
		s.auditStatsEvent(h, AuditStatsEvent{Kind: "freeze", Node: id, Height: int64(i + 5)})
	}
	if s.NodeAuditStats[id].PenaltyBoxRemaining != 0 || s.samplingSnapshot([]string{id}).RatePPM != 300000 {
		t.Fatal("penalty did not expire")
	}
}
func TestSamplingWeakestPipelineAndMoe(t *testing.T) {
	s, a := samplingState(t)
	ids := []string{a["node"].id, a["auditor"].id}
	s.NodeAuditStats = map[string]NodeAuditStats{ids[0]: {AuditedAccepts: 200}}
	snap := s.samplingSnapshot(ids)
	if snap.Tier != 0 || snap.RatePPM != 300000 || snap.Nodes[ids[0]] != 3 || snap.Nodes[ids[1]] != 5 {
		t.Fatal(snap)
	}
}
func TestSamplingSingleAuditorFortyCompletedRequests(t *testing.T) {
	s, a := samplingState(t)
	s.Parameters.Assignment.CooldownRequests = 8
	for i := 0; i < 40; i++ {
		h := sampleHold(s, a, i)
		h.SamplingPolicy = nil
		who, _, err := s.drawAuditor(h, []string{a["auditor"].id})
		if err != nil {
			t.Fatal(i, err)
		}
		if len(h.Assignments) != 1 || h.Assignments[0].FallbackCooldown != (i >= 8) {
			t.Fatal("single pool fallback")
		}
		if s.PairStreaks[h.Node].Consecutive != uint64(i) {
			t.Fatal("draw burned streak")
		}
		h.Auditor = who
		h.AuditVerdict = "accept"
		s.auditCompleted(h, int64(i+1))
	}
	if s.PairStreaks[a["node"].id].Consecutive != 40 {
		t.Fatal("completion streak")
	}
}
func TestSamplingCooldownFallbackKeepsHardRules(t *testing.T) {
	s, a := samplingState(t)
	h := sampleHold(s, a, 1)
	h.Node = "executor1"
	h.Pipeline = &PipelineHold{Authorizations: []PipelineAuthorization{{NodeID: "executor1"}, {NodeID: "executor2"}}}
	x, y := a["auditor"].id, a["auditor2"].id
	s.PairStreaks = map[string]PairStreak{"executor1": {Auditor: x, Consecutive: 4}, "executor2": {Auditor: y, Consecutive: 6}}
	who, _, err := s.drawAuditor(h, []string{x, y})
	if err != nil || who != x || !h.Assignments[0].FallbackCooldown {
		t.Fatal(who, err)
	}
	h2 := sampleHold(s, a, 2)
	h2.Assignments = []AssignmentRecord{{AuditorIDs: []string{x}}}
	if _, _, err := s.drawAuditor(h2, []string{x, h2.Node}); err == nil {
		t.Fatal("fallback bypassed prior/self exclusion")
	}
}
func TestSamplingStatisticalAcceptanceBands(t *testing.T) {
	for _, tc := range []struct {
		tier      uint64
		n, lo, hi int
	}{{200, 1000, 8, 32}, {0, 300, 66, 114}} {
		s, a := samplingState(t)
		s.NodeAuditStats = map[string]NodeAuditStats{a["node"].id: {AuditedAccepts: tc.tier}}
		hits := 0
		for i := 0; i < tc.n; i++ {
			h := sampleHold(s, a, i)
			for j := int64(2); j <= 4; j++ {
				b := sha256.Sum256([]byte(fmt.Sprintf("future-%d-%d", i, j)))
				s.BlockHashes[j] = b[:]
			}
			yes, err := s.resolveSampling(h)
			if err != nil {
				t.Fatal(err)
			}
			if yes {
				hits++
			}
			if binary.BigEndian.Uint64(h.Sampling.Seed[:8])%1000000 != h.Sampling.Roll {
				t.Fatal("roll")
			}
		}
		if hits < tc.lo || hits > tc.hi {
			t.Fatal("predeclared band", tc, hits)
		}
	}
}

func TestSamplingExposureFrozenPolicyAndBoundary(t *testing.T) {
	for _, tc := range []struct {
		accepts uint64
		div     int64
	}{{19, 5}, {20, 4}, {199, 4}, {200, 3}} {
		for _, extra := range []int64{0, 1} {
			s, a := samplingState(t)
			id := a["node"].id
			s.Accounts[id].Stake = 900
			s.Accounts[a["payer"].id].Balance = 10000
			s.NodeAuditStats = map[string]NodeAuditStats{id: {AuditedAccepts: tc.accepts}}
			action := Action{Type: "freeze", HoldID: "exposure", Payer: a["payer"].id, Node: id, Amount: 900/tc.div + extra}
			if extra == 1 {
				rejected(t, s, a["payer"], action, 1)
				continue
			}
			apply(t, s, a["payer"], 1, action, 1)
			h := s.Holds[action.HoldID]
			s.NodeAuditStats[id] = NodeAuditStats{}
			if s.exposureDivisor(h, id) != tc.div {
				t.Fatal("inflight policy changed", tc)
			}
		}
	}
}
func TestSamplingCompletionSwitchAndServiceFailure(t *testing.T) {
	s, a := samplingState(t)
	h := sampleHold(s, a, 1)
	x, y := a["auditor"].id, a["auditor2"].id
	s.Parameters.Assignment.CooldownRequests = 8
	h.Auditor = x
	if _, _, err := s.drawAuditor(h, []string{x}); err != nil {
		t.Fatal(err)
	}
	if s.PairStreaks[h.Node].Consecutive != 0 {
		t.Fatal("unfinished draw counted")
	}
	h.Auditor = y
	h.AuditVerdict = "service_failure"
	s.auditCompleted(h, 5)
	if s.PairStreaks[h.Node].Consecutive != 0 {
		t.Fatal("failure counted")
	}
	h.AuditVerdict = "accept"
	h.FirstVerdict = "reject"
	h.FirstAuditor = x
	s.auditCompleted(h, 6)
	if s.PairStreaks[h.Node].Auditor != y || s.PairStreaks[h.Node].Consecutive != 1 || s.NodeAuditStats[x].AuditsMisjudged != 1 {
		t.Fatal("final auditor/misjudge accounting")
	}
}
