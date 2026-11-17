package scheduler

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"sort"

	pb "ember/proto"
)

// MoE planning: a request plan is a set of backbone attention stages tiling
// the layer axis plus, for every (layer, expert) unit, a primary holder and
// ranked failovers. The coverage gate is evaluated here as well as at chain
// freeze: a unit with fewer than two eligible live holders makes the whole
// profile unplannable, so cold experts never silently take orders.

type MoeStagePlan struct {
	StartLayer uint32
	EndLayer   uint32
	Nodes      []Ranked // primary first, then failovers
}

type MoeUnitPlan struct {
	Layer  uint32
	Expert uint32
	Nodes  []Ranked // primary first, then failovers
}

type MoePlan struct {
	Stages  []MoeStagePlan
	Units   []MoeUnitPlan
	Holders map[[2]uint32]int // eligible live holders per unit
}

func moeEligible(r Request, c Candidate) bool {
	p := c.Profile
	if p == nil || p.MoeUnitKind == "" || !c.Online || c.Stake <= 0 ||
		c.Exposure >= c.Stake/5 || p.ChainId != r.Chain ||
		p.ExecutionProfileVer != r.Profile || !bytes.Equal(p.ManifestRoot, r.Manifest) ||
		r.Exclude[p.NodeId] {
		return false
	}
	return true
}

func moeScore(r Request, c Candidate) float64 {
	rep := math.Max(0, math.Min(1, c.Reputation))
	score := 100*rep - float64(c.RTTMillis)/10 - float64(c.Queue)*5 -
		float64(c.Exposure)*20/float64(c.Stake)
	if r.LatencyPolicy != nil {
		if c.LatencySamples < r.LatencyPolicy.MinSamples {
			return -math.MaxFloat64
		}
		return -float64(c.LatencyMicrosPerUnit)
	}
	return score
}

func rankMoe(r Request, candidates []Candidate, kind string) []Ranked {
	out := []Ranked{}
	for _, c := range candidates {
		if !moeEligible(r, c) || c.Profile.MoeUnitKind != kind {
			continue
		}
		out = append(out, Ranked{NodeID: c.Profile.NodeId, Score: moeScore(r, c), Reasons: []string{"moe profile/manifest/online/stake matched"}, Signer: c.Profile.Signer, SpeedScore: c.SpeedScore})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score == out[j].Score {
			return out[i].NodeID < out[j].NodeID
		}
		return out[i].Score > out[j].Score
	})
	return out
}

// PlanMoe builds the full request plan or fails with the exact coverage
// gap. Attention spans are the pinned profile spans; tiling is a DFS over
// span starts in ranked order (mirroring PlanPipeline's failover shape).
func PlanMoe(r Request, candidates []Candidate, layout *pb.MoeLayout) (MoePlan, error) {
	if r.Tier == "P1" {
		return MoePlan{}, errors.New("TIER_UNAVAILABLE")
	}
	if r.Tier != "" && r.Tier != "P0" {
		return MoePlan{}, errors.New("invalid tier")
	}
	if layout == nil || layout.NumLayers == 0 {
		return MoePlan{}, errors.New("invalid MoE layout")
	}
	plan := MoePlan{Holders: map[[2]uint32]int{}}
	// Expert units + coverage gate.
	byUnit := map[[2]uint32][]Ranked{}
	for _, c := range candidates {
		if !moeEligible(r, c) || c.Profile.MoeUnitKind != "expert" {
			continue
		}
		for _, unit := range c.Profile.MoeUnits {
			key := [2]uint32{unit.Layer, unit.Expert}
			byUnit[key] = append(byUnit[key], Ranked{NodeID: c.Profile.NodeId, Score: moeScore(r, c), Reasons: []string{"moe expert holder"}, Signer: c.Profile.Signer, SpeedScore: c.SpeedScore})
		}
	}
	for layer := uint32(0); layer < layout.NumLayers; layer++ {
		for expert := uint32(0); expert < layout.ExpertsPerLayer; expert++ {
			key := [2]uint32{layer, expert}
			nodes := byUnit[key]
			sort.Slice(nodes, func(i, j int) bool {
				if nodes[i].Score == nodes[j].Score {
					return nodes[i].NodeID < nodes[j].NodeID
				}
				return nodes[i].Score > nodes[j].Score
			})
			plan.Holders[key] = len(nodes)
			if len(nodes) < 2 {
				return MoePlan{}, fmt.Errorf("expert coverage gate: layer %d expert %d has %d eligible holders", layer, expert, len(nodes))
			}
			plan.Units = append(plan.Units, MoeUnitPlan{Layer: layer, Expert: expert, Nodes: nodes})
		}
	}
	// Attention tiling.
	type span struct {
		start, end uint32
		nodes      []Ranked
	}
	bySpan := map[[2]uint32][]Ranked{}
	for _, c := range candidates {
		if !moeEligible(r, c) || c.Profile.MoeUnitKind != "attention" {
			continue
		}
		key := [2]uint32{c.Profile.ShardStartLayer, c.Profile.ShardEndLayer}
		bySpan[key] = append(bySpan[key], Ranked{NodeID: c.Profile.NodeId, Score: moeScore(r, c), Reasons: []string{"moe attention stage"}, Signer: c.Profile.Signer, SpeedScore: c.SpeedScore})
	}
	spans := []span{}
	for key, nodes := range bySpan {
		sort.Slice(nodes, func(i, j int) bool {
			if nodes[i].Score == nodes[j].Score {
				return nodes[i].NodeID < nodes[j].NodeID
			}
			return nodes[i].Score > nodes[j].Score
		})
		spans = append(spans, span{start: key[0], end: key[1], nodes: nodes})
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		return spans[i].end < spans[j].end
	})
	var chosen []span
	var walk func(cursor uint32) bool
	walk = func(cursor uint32) bool {
		if cursor == layout.NumLayers {
			return true
		}
		for _, sp := range spans {
			if sp.start != cursor || sp.end <= cursor || sp.end > layout.NumLayers {
				continue
			}
			chosen = append(chosen, sp)
			if walk(sp.end) {
				return true
			}
			chosen = chosen[:len(chosen)-1]
		}
		return false
	}
	if !walk(0) {
		return MoePlan{}, errors.New("no attention stage tiling covers the layer axis")
	}
	for _, sp := range chosen {
		plan.Stages = append(plan.Stages, MoeStagePlan{
			StartLayer: sp.start, EndLayer: sp.end, Nodes: sp.nodes})
	}
	return plan, nil
}
