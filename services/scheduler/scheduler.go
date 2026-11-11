package scheduler

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"sort"

	core "ember/core-go"
	pb "ember/proto"
)

type Candidate struct {
	LatencyMicrosPerUnit uint64
	LatencySamples       uint32
	Profile              *pb.ResourceProfile
	Online               bool
	Stake                int64
	Exposure             int64
	RTTMillis            int
	Queue                int
	Reputation           float64
	Prefix               string
}

type Request struct {
	LatencyPolicy  *core.SchedulingPolicy
	DynamicRanges  bool
	MinStageLayers uint32
	AllowedCuts    []uint32
	Chain          string
	Manifest       []byte
	Profile        string
	Memory         uint64
	Prefix         string
	Exclude        map[string]bool
	TieBreak       uint64
	Shards         bool
}

type Ranked struct {
	NodeID  string
	Score   float64
	Reasons []string
	Signer  []byte
}

type Pipeline struct {
	Nodes []Ranked
}

type PipelineStage struct {
	Index      uint32
	StartLayer uint32
	EndLayer   uint32
	Nodes      []Ranked
}

type PipelinePlan struct {
	Stages []PipelineStage
}

func Rank(r Request, candidates []Candidate) []Ranked {
	out := []Ranked{}
	for _, c := range candidates {
		p := c.Profile
		isShard := p != nil && p.ShardTotalLayers > 0 && (p.ShardEndLayer > p.ShardStartLayer || p.ShardHasEmbedding || p.ShardHasLmHead)
		if p == nil || isShard != r.Shards || !c.Online || c.Stake <= 0 || c.Exposure >= c.Stake/5 || p.ChainId != r.Chain || p.ExecutionProfileVer != r.Profile || !bytes.Equal(p.ManifestRoot, r.Manifest) || p.MemoryBytes < r.Memory || r.Exclude[p.NodeId] {
			continue
		}
		rep := math.Max(0, math.Min(1, c.Reputation))
		score := 100*rep - float64(c.RTTMillis)/10 - float64(c.Queue)*5 - float64(c.Exposure)*20/float64(c.Stake)
		reasons := []string{"profile/manifest/online/stake matched", "reputation, RTT, queue, exposure weighted"}
		if r.LatencyPolicy != nil {
			score = -math.MaxFloat64
			if c.LatencySamples >= r.LatencyPolicy.MinSamples {
				score = -float64(c.LatencyMicrosPerUnit)
			}
			reasons = []string{"finalized countersigned receipt percentile; self-reported timing ignored"}
		}
		if r.LatencyPolicy == nil && r.Prefix != "" && r.Prefix == c.Prefix {
			score += 30
			reasons = append(reasons, "KV prefix affinity +30")
		}
		out = append(out, Ranked{p.NodeId, score, reasons, p.Signer})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score == out[j].Score {
			return out[i].NodeID < out[j].NodeID
		}
		return out[i].Score > out[j].Score
	})
	return applyExploration(r, out, candidates)
}

func Choose(r Request, candidates []Candidate) (Ranked, error) {
	ranked := Rank(r, candidates)
	if len(ranked) == 0 {
		return Ranked{}, errors.New("no eligible node")
	}
	if r.LatencyPolicy != nil {
		return ranked[0], nil
	}
	tied := 1
	for tied < len(ranked) && ranked[tied].Score == ranked[0].Score {
		tied++
	}
	return ranked[r.TieBreak%uint64(tied)], nil
}

// ChoosePipeline selects one eligible node per shard and requires exact,
// contiguous coverage. Shard ranges are half-open: [start_layer,end_layer).
func ChoosePipeline(r Request, candidates []Candidate, totalLayers uint32) (Pipeline, error) {
	plan, err := PlanPipeline(r, candidates, totalLayers)
	if err != nil {
		return Pipeline{}, err
	}
	nodes := make([]Ranked, len(plan.Stages))
	for i, stage := range plan.Stages {
		nodes[i] = stage.Nodes[0]
	}
	return Pipeline{Nodes: nodes}, nil
}

// PlanPipeline returns the best complete topology plus ranked same-range
// alternatives for stage-level failover.
func PlanPipeline(r Request, candidates []Candidate, totalLayers uint32) (PipelinePlan, error) {
	if r.DynamicRanges {
		return PlanRanges(r, candidates, totalLayers)
	}
	if totalLayers == 0 {
		return PipelinePlan{}, errors.New("invalid pipeline layer count")
	}
	r.Shards = true
	// Search all eligible edges instead of choosing a stage before checking
	// its boundaries. A high-ranked dead end must not hide a valid pipeline.
	type path struct {
		nodes []Ranked
		score float64
	}
	type key struct{ index, cursor uint32 }
	memo := map[key]*path{}
	var search func(uint32, uint32) *path
	search = func(index, cursor uint32) *path {
		if cursor == totalLayers+2 {
			return &path{}
		}
		k := key{index, cursor}
		if cached, ok := memo[k]; ok {
			return cached
		}
		var choices []Candidate
		for _, c := range candidates {
			p := c.Profile
			if p == nil || p.ShardTotalLayers != totalLayers || p.ShardIndex != index {
				continue
			}
			start, end, err := core.StageSpan(p.ShardStartLayer, p.ShardEndLayer, totalLayers, p.ShardHasEmbedding, p.ShardHasLmHead)
			if err != nil || start != cursor || end <= cursor {
				continue
			}
			choices = append(choices, c)
		}
		var best *path
		for _, ranked := range Rank(r, choices) {
			var end uint32
			for _, c := range choices {
				if c.Profile.NodeId == ranked.NodeID {
					_, end, _ = core.StageSpan(c.Profile.ShardStartLayer, c.Profile.ShardEndLayer, totalLayers, c.Profile.ShardHasEmbedding, c.Profile.ShardHasLmHead)
					break
				}
			}
			rest := search(index+1, end)
			if rest == nil {
				continue
			}
			next := &path{nodes: append([]Ranked{ranked}, rest.nodes...), score: ranked.Score + rest.score}
			if best == nil || next.score > best.score {
				best = next
			}
		}
		memo[k] = best
		return best
	}
	best := search(0, 0)
	if best == nil {
		reachable := uint32(0)
		for cursor := uint32(0); cursor < totalLayers; {
			end := cursor
			for _, c := range candidates {
				p := c.Profile
				if p != nil && p.ShardStartLayer == cursor && len(Rank(r, []Candidate{c})) == 1 && p.ShardEndLayer > end {
					end = p.ShardEndLayer
				}
			}
			if end == cursor {
				break
			}
			reachable, cursor = end, end
		}
		return PipelinePlan{}, fmt.Errorf("no eligible contiguous pipeline covering layers [0,%d); missing coverage starts at layer %d", totalLayers, reachable)
	}
	stages := make([]PipelineStage, len(best.nodes))
	for i, chosen := range best.nodes {
		var profile *pb.ResourceProfile
		for _, c := range candidates {
			if c.Profile != nil && c.Profile.NodeId == chosen.NodeID {
				profile = c.Profile
				break
			}
		}
		if profile == nil {
			return PipelinePlan{}, errors.New("selected shard profile disappeared")
		}
		var same []Candidate
		for _, c := range candidates {
			p := c.Profile
			if p != nil && p.ShardIndex == profile.ShardIndex && p.ShardStartLayer == profile.ShardStartLayer &&
				p.ShardEndLayer == profile.ShardEndLayer && p.ShardTotalLayers == totalLayers &&
				p.ShardHasEmbedding == profile.ShardHasEmbedding && p.ShardHasLmHead == profile.ShardHasLmHead &&
				bytes.Equal(p.ShardParametersHash, profile.ShardParametersHash) {
				same = append(same, c)
			}
		}
		stages[i] = PipelineStage{Index: profile.ShardIndex, StartLayer: profile.ShardStartLayer,
			EndLayer: profile.ShardEndLayer, Nodes: Rank(r, same)}
		if len(stages[i].Nodes) == 0 {
			return PipelinePlan{}, fmt.Errorf("shard %d has no eligible candidates", profile.ShardIndex)
		}
	}
	return PipelinePlan{Stages: stages}, nil
}
