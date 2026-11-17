package scheduler

import "sort"

func applyExploration(r Request, ranked []Ranked, candidates []Candidate) []Ranked {
	p := r.LatencyPolicy
	if p == nil || len(ranked) < 2 {
		return ranked
	}
	byID := append([]Ranked(nil), ranked...)
	sort.Slice(byID, func(i, j int) bool { return byID[i].NodeID < byID[j].NodeID })
	proven := map[string]bool{}
	for _, c := range candidates {
		if c.Profile != nil && c.LatencySamples >= p.MinSamples {
			proven[c.Profile.NodeId] = true
		}
	}
	anyProven := false
	for _, v := range ranked {
		if proven[v.NodeID] {
			anyProven = true
		}
	}
	period := uint64(p.ExplorationPeriod)
	slots := uint64(p.ExplorationPPM) * period / 1000000
	var chosen string
	if !anyProven || slots*uint64(len(byID)) > period {
		chosen = byID[r.TieBreak%uint64(len(byID))].NodeID
	} else if tick := r.TieBreak % period; tick < slots*uint64(len(byID)) {
		chosen = byID[tick%uint64(len(byID))].NodeID
	}
	if chosen != "" {
		for i := range ranked {
			if ranked[i].NodeID == chosen {
				first := ranked[i]
				first.Reasons = append(first.Reasons, "chain-visible exploration slot")
				copy(ranked[1:i+1], ranked[:i])
				ranked[0] = first
				break
			}
		}
	}
	return ranked
}
