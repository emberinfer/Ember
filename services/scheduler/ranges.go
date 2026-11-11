package scheduler

import (
	core "ember/core-go"
	pb "ember/proto"
	"errors"
	"sort"
)

// PlanRanges finds minimum-stage covers, rotates among legal partitions, and
// retains every eligible covering capability for same-executed-range failover.
func PlanRanges(r Request, candidates []Candidate, total uint32) (PipelinePlan, error) {
	if total == 0 || r.MinStageLayers == 0 {
		return PipelinePlan{}, errors.New("missing signed dynamic range rules")
	}
	r.Shards = true
	m := &pb.ModelManifest{DynamicRanges: true, TotalLayers: total, MinStageLayers: r.MinStageLayers, AllowedLayerCuts: r.AllowedCuts}
	cuts := append([]uint32(nil), r.AllowedCuts...)
	if len(cuts) == 0 {
		if uint64(total)/uint64(r.MinStageLayers)+2 > 4096 {
			return PipelinePlan{}, errors.New("dynamic cut catalog exceeds planner bound")
		}
		for k := uint64(0); k < uint64(total); k += uint64(r.MinStageLayers) {
			cuts = append(cuts, uint32(k))
		}
		cuts = append(cuts, total)
	}
	if len(cuts) > 4096 {
		return PipelinePlan{}, errors.New("dynamic cut catalog exceeds planner bound")
	}
	sort.Slice(cuts, func(i, j int) bool { return cuts[i] < cuts[j] })
	eligible := []Candidate{}
	for _, c := range candidates {
		if c.Profile != nil && c.Profile.ShardDynamicRange && c.Profile.ShardTotalLayers == total && len(Rank(r, []Candidate{c})) > 0 {
			eligible = append(eligible, c)
		}
	}
	covers := func(c Candidate, a, b uint32) bool {
		p := c.Profile
		return a >= p.ShardStartLayer && b <= p.ShardEndLayer && (a != 0 || p.ShardHasEmbedding) && (b != total || p.ShardHasLmHead) && core.AllowedRange(m, a, b)
	}
	paths := [][]uint32{}
	best := int(total) + 1
	var search func(uint32, []uint32, map[string]bool)
	search = func(cursor uint32, ends []uint32, used map[string]bool) {
		if cursor == total {
			if len(ends) < best {
				best = len(ends)
				paths = nil
			}
			if len(ends) == best {
				paths = append(paths, append([]uint32(nil), ends...))
			}
			return
		}
		if len(ends) >= best {
			return
		}
		for _, c := range eligible {
			id := c.Profile.NodeId
			if used[id] {
				continue
			}
			used[id] = true
			// Long edges first establish the minimum stage count before exploring cuts.
			for k := len(cuts) - 1; k >= 0; k-- {
				end := cuts[k]
				if end > cursor && covers(c, cursor, end) {
					search(end, append(ends, end), used)
				}
			}
			delete(used, id)
		}
	}
	search(0, nil, map[string]bool{})
	if len(paths) == 0 {
		return PipelinePlan{}, errors.New("no complete cover of registered dynamic ranges")
	}
	sort.Slice(paths, func(i, j int) bool {
		for k := range paths[i] {
			if paths[i][k] != paths[j][k] {
				return paths[i][k] < paths[j][k]
			}
		}
		return false
	})
	unique := [][]uint32{}
	for _, p := range paths {
		same := len(unique) > 0
		if same {
			for i := range p {
				if unique[len(unique)-1][i] != p[i] {
					same = false
					break
				}
			}
		}
		if !same {
			unique = append(unique, p)
		}
	}
	chosen := unique[r.TieBreak%uint64(len(unique))]
	stages := []PipelineStage{}
	cursor := uint32(0)
	for i, end := range chosen {
		choices := []Candidate{}
		for _, c := range eligible {
			if covers(c, cursor, end) {
				choices = append(choices, c)
			}
		}
		stages = append(stages, PipelineStage{Index: uint32(i), StartLayer: cursor, EndLayer: end, Nodes: Rank(r, choices)})
		cursor = end
	}
	return PipelinePlan{Stages: stages}, nil
}
