package scheduler

import (
	"log"
	"math"
)

type ShadowEntry struct {
	Stage                      int
	Chain, Profile             string
	Eligible                   int
	ActualNodeID, ShadowNodeID string
	SpeedEnabled               bool
}

var ShadowLogger = func(e ShadowEntry) {
	log.Printf("SPEEDSHADOW stage=%d chain=%s profile=%s eligible=%d actual=%s shadow=%s speed_enabled=%t", e.Stage, e.Chain, e.Profile, e.Eligible, e.ActualNodeID, e.ShadowNodeID, e.SpeedEnabled)
}

type splitmix64 uint64

func (s *splitmix64) next() uint64 {
	*s += 0x9E3779B97F4A7C15
	z := uint64(*s)
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}
func (s *splitmix64) unit() float64 { return float64(s.next()>>11) * (1.0 / (1 << 53)) }
func speedWeight(v float64) float64 {
	if v <= 0 || math.IsNaN(v) {
		return 1
	}
	return math.Max(.5, math.Min(2, v))
}
func weightedPick(nodes []Ranked, seed uint64) Ranked {
	for _, n := range nodes {
		if n.Affinity {
			return n
		}
	}
	if len(nodes) == 1 {
		return nodes[0]
	}
	rng := splitmix64(seed)
	if rng.unit() < .10 {
		return nodes[int(rng.unit()*float64(len(nodes)))]
	}
	total := 0.0
	for _, n := range nodes {
		total += speedWeight(n.SpeedScore)
	}
	draw := rng.unit() * total
	for _, n := range nodes {
		draw -= speedWeight(n.SpeedScore)
		if draw < 0 {
			return n
		}
	}
	return nodes[len(nodes)-1]
}
func selectSpeed(r Request, nodes []Ranked, stage int, pipeline bool) Ranked {
	legacy := nodes[0]
	if r.LatencyPolicy != nil {
		return legacy
	}
	seed := r.TieBreak
	if pipeline {
		seed ^= uint64(stage+1) * 0x9E3779B97F4A7C15
	} else {
		tied := 1
		for tied < len(nodes) && nodes[tied].Score == nodes[0].Score {
			tied++
		}
		legacy = nodes[r.TieBreak%uint64(tied)]
	}
	actual, shadow := legacy, weightedPick(nodes, seed^0x5EED5EED)
	if r.SpeedEnabled {
		actual, shadow = weightedPick(nodes, seed), legacy
	}
	if ShadowLogger != nil {
		ShadowLogger(ShadowEntry{stage, r.Chain, r.Profile, len(nodes), actual.NodeID, shadow.NodeID, r.SpeedEnabled})
	}
	return actual
}

// SelectStage chooses the primary from a planned stage without changing topology.
// Callers retain the remaining ranked nodes for failover.
func SelectStage(r Request, nodes []Ranked, stage int) Ranked {
	return selectSpeed(r, nodes, stage, true)
}
