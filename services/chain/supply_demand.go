package chain

import (
	"errors"
	"math"
	"math/big"

	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

// PriceParameters are consensus governance values. A queued update takes effect
// only when the current epoch closes, never part way through a pricing window.
type PriceParameters struct {
	EpochBlocks       int64  `json:"epoch_blocks"`
	MinPPM            int64  `json:"min_ppm"`
	MaxPPM            int64  `json:"max_ppm"`
	StepPPM           int64  `json:"step_ppm"`
	MinSettledSamples uint64 `json:"min_settled_samples"`
}

func DefaultPriceParameters() PriceParameters {
	return PriceParameters{EpochBlocks: 100, MinPPM: 800000, MaxPPM: 2000000, StepPPM: 100000, MinSettledSamples: 10}
}
func (p PriceParameters) valid() bool {
	return p.EpochBlocks > 0 && p.MinPPM > 0 && p.MinPPM <= PriceScale && p.MaxPPM >= PriceScale && p.MaxPPM <= 100*PriceScale && p.StepPPM > 0 && p.StepPPM <= PriceScale && p.MinSettledSamples > 0
}

type PriceStamp struct {
	MultiplierPPM   int64 `json:"multiplier_ppm"`
	EffectiveHeight int64 `json:"effective_height"`
}

type SettledDemand struct {
	Requests uint64 `json:"requests"`
	Tokens   uint64 `json:"tokens"`
}
type ProfilePrice struct {
	MultiplierPPM     int64         `json:"multiplier_ppm"`
	EffectiveHeight   int64         `json:"effective_height"`
	CumulativeSamples uint64        `json:"cumulative_samples"`
	Window            SettledDemand `json:"window"`
	Previous          SettledDemand `json:"previous_window"`
	Supply            int64         `json:"supply"`
	Target            int64         `json:"target"`
	RawPPM            int64         `json:"raw_ppm"`
	CorridorPPM       int64         `json:"corridor_ppm"`
}

func (s *State) PriceForProfile(id string) ProfilePrice {
	if p := s.Prices[id]; p != nil {
		return *p
	}
	return ProfilePrice{MultiplierPPM: PriceScale, EffectiveHeight: 1}
}
func (s *State) priceRecord(id string) *ProfilePrice {
	if s.Prices == nil {
		s.Prices = map[string]*ProfilePrice{}
	}
	if s.Prices[id] == nil {
		p := s.PriceForProfile(id)
		s.Prices[id] = &p
	}
	return s.Prices[id]
}

// Only final accepted settlement calls this function. Quotes, submissions,
// failed execution and replay work are deliberately absent from these counters.
func (s *State) recordPriceDemand(h *Hold) error {
	if h.State == "settled" || h.ModelID == "" {
		return nil
	}
	p := s.priceRecord(h.ModelID)
	tokens := uint64(h.AuditInputTokens) + uint64(h.AuditOutputTokens)
	if f := h.TokenFee; f != nil {
		c := &pb.ExecutionCommitment{}
		if len(f.Commitment) > 0 && proto.Unmarshal(f.Commitment, c) != nil {
			return errors.New("invalid settled commitment")
		}
		tokens = uint64(c.InputTokens) + uint64(f.OutputTokens)
	}
	if p.Window.Requests == math.MaxUint64 || p.CumulativeSamples == math.MaxUint64 || tokens > math.MaxUint64-p.Window.Tokens {
		return errors.New("settled demand overflow")
	}
	p.Window.Requests++
	p.Window.Tokens += tokens
	p.CumulativeSamples++
	return nil
}
func (s *State) profileSupply(id string, height int64) (int64, int64) {
	model := s.Models[id]
	target := int64(1)
	if model.Pricing != nil {
		target = model.Pricing.TargetExecutors
	}
	raw, _ := modelArtifacts(model)
	g := &pb.ChainGenesis{}
	if len(raw) > 0 && proto.Unmarshal(raw, g) != nil {
		return 0, target
	}
	var count int64
	for node := range s.Nodes {
		p := s.ActiveProfile(node, height)
		if p == nil || p.ChainId != id || (!HasRole(p, "model") && !HasRole(p, "executor")) {
			continue
		}
		// Same profile identity caliber as coverage, without auditor/runtime weighting.
		if len(raw) > 0 && (p.ExecutionProfileVer != g.ExecutionProfileVer || !equal(p.ManifestRoot, g.ModelManifestRoot)) {
			continue
		}
		count++
	}
	return count, target
}
func clampPrice(n, low, high int64) int64 {
	if n < low {
		return low
	}
	if n > high {
		return high
	}
	return n
}

// priceStep uses only settled demand and registered supply. big.Int prevents
// overflowing counters, normalized demand, and signed pressure arithmetic.
func priceStep(old int64, d SettledDemand, supply, target, blocks int64, p PriceParameters) (int64, int64, int64) {
	if d.Requests == 0 {
		return old, old, old
	}
	units := new(big.Int).SetUint64(d.Tokens)
	units.Add(units, big.NewInt(1023)).Quo(units, big.NewInt(1024))
	units.Add(units, new(big.Int).SetUint64(d.Requests))
	pressure := units.Mul(units, big.NewInt(PriceScale)).Quo(units, big.NewInt(blocks))
	if pressure.Cmp(big.NewInt(PriceScale)) > 0 {
		pressure.SetInt64(PriceScale)
	}
	denom := max(int64(1), max(target, supply))
	delta := pressure.Mul(pressure, big.NewInt(target-supply)).Quo(pressure, big.NewInt(denom)).Int64()
	raw := PriceScale + delta
	corridor := clampPrice(raw, p.MinPPM, p.MaxPPM)
	step, _ := core.ScaleFee(old, p.StepPPM)
	return clampPrice(corridor, old-step, old+step), raw, corridor
}

// Called once by FinalizeBlock after that block's transactions: the completed
// window includes the boundary block. Its multiplier applies from height+1.
func (s *State) finishPriceEpoch(height int64) error {
	if s.Version < 8 && s.Parameters.SupplyDemand == (PriceParameters{}) {
		return nil
	}
	policy := s.Parameters.SupplyDemand
	if !policy.valid() {
		return errors.New("invalid consensus price parameters")
	}
	if height-s.PriceEpochStart < policy.EpochBlocks {
		return nil
	}
	if height-s.PriceEpochStart != policy.EpochBlocks {
		return errors.New("skipped price epoch boundary")
	}
	next := policy
	if s.PendingPriceParameters != nil {
		next = *s.PendingPriceParameters
	}
	for id := range s.Models {
		p := s.priceRecord(id)
		supply, target := s.profileSupply(id, height)
		m, raw, corridor := priceStep(p.MultiplierPPM, p.Window, supply, target, policy.EpochBlocks, next)
		if p.CumulativeSamples < next.MinSettledSamples {
			m, raw, corridor = PriceScale, PriceScale, PriceScale
		}
		p.MultiplierPPM, p.EffectiveHeight = m, height+1
		p.Previous, p.Window = p.Window, SettledDemand{}
		p.Supply, p.Target, p.RawPPM, p.CorridorPPM = supply, target, raw, corridor
	}
	s.Parameters.SupplyDemand = next
	s.PendingPriceParameters = nil
	s.PriceEpochStart = height
	return nil
}
