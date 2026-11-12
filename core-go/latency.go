package core

import (
	"errors"
	"sort"
)

// SchedulingPolicy is serialized consensus configuration, never a node claim.
type SchedulingPolicy struct {
	ConsumerCrossCheck *ConsumerCrossCheckPolicy `json:"consumer_cross_check,omitempty"`
	Scoring            string                    `json:"scoring"`
	ExplorationPPM     uint32                    `json:"exploration_ppm_per_node"`
	ExplorationPeriod  uint32                    `json:"exploration_period_requests"`
	PercentilePPM      uint32                    `json:"percentile_ppm"`
	WindowSamples      uint32                    `json:"window_samples"`
	MinSamples         uint32                    `json:"min_samples"`
	MaxObservationNS   uint64                    `json:"max_observation_ns"`
	WitnessAuthority   string                    `json:"witness_authority"`
}

type ConsumerCrossCheckPolicy struct {
	MaxRatioPPM   uint32 `json:"max_ratio_ppm"`
	MinSamples    uint32 `json:"min_samples"`
	DivergencePPM uint32 `json:"divergence_ppm"`
}

func (p *SchedulingPolicy) Validate() error {
	if p == nil {
		return nil
	}
	if c := p.ConsumerCrossCheck; c != nil && (c.MaxRatioPPM <= 1000000 || c.MaxRatioPPM > 100000000 || c.MinSamples == 0 || c.MinSamples > p.WindowSamples || c.DivergencePPM == 0 || c.DivergencePPM > 1000000) {
		return errors.New("invalid consumer latency cross-check policy")
	}
	if p.Scoring != "auditor-roundtrip-us-per-unit-v1" || p.WitnessAuthority != "assigned-auditor" || p.ExplorationPPM == 0 || p.ExplorationPPM > 1000000 || p.ExplorationPeriod == 0 || p.ExplorationPeriod > 1000000 || uint64(p.ExplorationPPM)*uint64(p.ExplorationPeriod)%1000000 != 0 || p.PercentilePPM == 0 || p.PercentilePPM > 1000000 || p.WindowSamples == 0 || p.WindowSamples > 10000 || p.MinSamples == 0 || p.MinSamples > p.WindowSamples || p.MaxObservationNS == 0 || p.MaxObservationNS > 86400000000000 {
		return errors.New("invalid receipt latency scheduling policy")
	}
	return nil
}

func LatencyPercentile(samples []uint64, ppm uint32) uint64 {
	if len(samples) == 0 {
		return 0
	}
	sorted := append([]uint64(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := (uint64(len(sorted))*uint64(ppm)+999999)/1000000 - 1
	return sorted[index]
}
