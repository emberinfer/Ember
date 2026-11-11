package chain

import (
	"bytes"
	"errors"
	"math/big"
	"sort"

	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

const ScarcityScale int64 = 1_000_000

// RolePricing is an immutable per-model governance constant. Multipliers are
// capped at 2x; all allocations redistribute the existing request fee.
type RolePricing struct {
	TargetExecutors int64 `json:"target_executors"`
	TargetAuditors  int64 `json:"target_auditors"`
	ExecutorWeight  int64 `json:"executor_weight"`
	AuditorWeight   int64 `json:"auditor_weight"`
	ConsumerWeight  int64 `json:"consumer_weight"`
}

func (p *RolePricing) valid() bool {
	if p == nil {
		return true
	}
	for _, v := range []int64{p.TargetExecutors, p.TargetAuditors, p.ExecutorWeight, p.AuditorWeight, p.ConsumerWeight} {
		if v < 1 || v > 1_000_000 {
			return false
		}
	}
	return true
}

type CoverageSnapshot struct {
	Height             int64  `json:"height"`
	Profile            string `json:"profile"`
	Runtime            string `json:"runtime"`
	Executors          int64  `json:"executors"`
	Auditors           int64  `json:"auditors"`
	TargetExecutors    int64  `json:"target_executors"`
	TargetAuditors     int64  `json:"target_auditors"`
	ExecutorMultiplier int64  `json:"executor_multiplier_ppm"`
	AuditorMultiplier  int64  `json:"auditor_multiplier_ppm"`
}
type SettlementShares struct {
	Fee      int64            `json:"fee"`
	Executor int64            `json:"executor"`
	Auditor  int64            `json:"auditor"`
	Consumer int64            `json:"consumer"`
	Coverage CoverageSnapshot `json:"coverage"`
}

func multiplier(target, actual int64) int64 {
	if actual >= target {
		return ScarcityScale
	}
	return ScarcityScale + (target-actual)*ScarcityScale/target
}

func (s *State) coverage(producer string, p *pb.ResourceProfile, policy *RolePricing) CoverageSnapshot {
	c := CoverageSnapshot{Height: s.Height, Profile: p.ChainId, Runtime: RuntimeID(p), TargetExecutors: policy.TargetExecutors, TargetAuditors: policy.TargetAuditors}
	for id := range s.Nodes {
		r := s.ActiveProfile(id, s.Height)
		if r == nil || r.ChainId != p.ChainId || r.ExecutionProfileVer != p.ExecutionProfileVer || !bytes.Equal(r.ManifestRoot, p.ManifestRoot) {
			continue
		}
		if HasRole(r, "model") || HasRole(r, "executor") {
			c.Executors++
		}
		if s.EligibleAuditorProfile(id, producer, p, s.Height) {
			c.Auditors++
		}
	}
	c.ExecutorMultiplier = multiplier(policy.TargetExecutors, c.Executors)
	c.AuditorMultiplier = multiplier(policy.TargetAuditors, c.Auditors)
	return c
}

func (s *State) CoverageForNode(id string) (CoverageSnapshot, error) {
	r := s.Nodes[id]
	p := &pb.ResourceProfile{}
	if r == nil || proto.Unmarshal(r.Profile, p) != nil {
		return CoverageSnapshot{}, errors.New("unknown registered profile")
	}
	policy := s.Models[p.ChainId].Pricing
	if policy == nil {
		return CoverageSnapshot{}, errors.New("profile has no scarcity pricing policy")
	}
	return s.coverage(id, p, policy), nil
}

func (s *State) finalizedState() *State {
	if s.finalized != nil {
		return s.finalized
	}
	return s
}

func (s *State) priceShares(hold *Hold, policy *RolePricing) (*SettlementShares, error) {
	source := s.finalizedState()
	p := &pb.ResourceProfile{}
	if proto.Unmarshal(hold.ProducerProfile, p) != nil || p.NodeId != hold.Node || p.ChainId != hold.ModelID {
		return nil, errors.New("missing frozen pricing profile")
	}
	coverage := source.coverage(hold.Node, p, policy)
	weights := [3]int64{policy.ExecutorWeight * coverage.ExecutorMultiplier, policy.AuditorWeight * coverage.AuditorMultiplier, policy.ConsumerWeight * ScarcityScale}
	amounts := allocateFee(hold.Actual, weights)
	return &SettlementShares{Fee: hold.Actual, Executor: amounts[0], Auditor: amounts[1], Consumer: amounts[2], Coverage: coverage}, nil
}

// Largest remainder allocation preserves every integer subunit, including fees
// of one subunit. Fixed role order breaks ties; big integers avoid price overflow.
func allocateFee(fee int64, weights [3]int64) [3]int64 {
	total := weights[0] + weights[1] + weights[2]
	amounts := [3]int64{}
	remainders := [3]int64{}
	left := fee
	for i, w := range weights {
		product := new(big.Int).Mul(big.NewInt(fee), big.NewInt(w))
		q, r := new(big.Int), new(big.Int)
		q.QuoRem(product, big.NewInt(total), r)
		amounts[i] = q.Int64()
		remainders[i] = r.Int64()
		left -= amounts[i]
	}
	order := []int{0, 1, 2}
	sort.SliceStable(order, func(i, j int) bool { return remainders[order[i]] > remainders[order[j]] })
	for i := int64(0); i < left; i++ {
		amounts[order[i]]++
	}
	return amounts
}

// A live registration cannot withdraw coverage by changing role/runtime or
// bringing its expiry forward. Additions are allowed; removal waits for TTL.
func registrationPreservesCoverage(old, new *pb.ResourceProfile) bool {
	if old.ChainId != new.ChainId || old.ExecutionProfileVer != new.ExecutionProfileVer || !bytes.Equal(old.ManifestRoot, new.ManifestRoot) || RuntimeID(old) != RuntimeID(new) || old.ActualDevice != new.ActualDevice {
		return false
	}
	if old.ShardDynamicRange && (!new.ShardDynamicRange || old.ShardStartLayer != new.ShardStartLayer || old.ShardEndLayer != new.ShardEndLayer || !bytes.Equal(old.ShardParametersHash, new.ShardParametersHash)) {
		return false
	}
	if old.ShardTotalLayers > 0 && (new.ShardTotalLayers != old.ShardTotalLayers || new.ShardStartLayer > old.ShardStartLayer || new.ShardEndLayer < old.ShardEndLayer || (old.ShardHasEmbedding && !new.ShardHasEmbedding) || (old.ShardHasLmHead && !new.ShardHasLmHead) || old.ShardUnitKind != new.ShardUnitKind) {
		return false
	}
	if old.EngineCapabilities == nil {
		return true
	}
	if new.EngineCapabilities == nil {
		return false
	}
	a := proto.Clone(old.EngineCapabilities).(*pb.EngineCapabilities)
	b := proto.Clone(new.EngineCapabilities).(*pb.EngineCapabilities)
	for _, role := range a.Roles {
		if !contains(b.Roles, role) {
			return false
		}
	}
	a.Roles = nil
	b.Roles = nil
	return proto.Equal(a, b)
}
