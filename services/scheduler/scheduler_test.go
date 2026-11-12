package scheduler

import (
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
	"slices"
	"strings"
	"testing"
)

func TestRank(t *testing.T) {
	m := make([]byte, 32)
	p := func(id string) *pb.ResourceProfile {
		return &pb.ResourceProfile{NodeId: id, ChainId: "ember-test", ManifestRoot: m, ExecutionProfileVer: "cpu", MemoryBytes: 100}
	}
	c := []Candidate{{Profile: p("offline"), Stake: 1000, Reputation: 1}, {Profile: p("a"), Online: true, Stake: 1000, Reputation: .9, Prefix: "session"}, {Profile: p("b"), Online: true, Stake: 1000, Reputation: 1}}
	r := Rank(Request{Chain: "ember-test", Manifest: m, Profile: "cpu", Prefix: "session"}, c)
	if len(r) != 2 || r[0].NodeID != "a" || len(r[0].Reasons) != 3 {
		t.Fatalf("unexpected ranking: %+v", r)
	}
}

func TestChooseRotatesOnlyHighestScoringTies(t *testing.T) {
	m := make([]byte, 32)
	profile := func(id string) *pb.ResourceProfile {
		return &pb.ResourceProfile{NodeId: id, ChainId: "ember-test", ManifestRoot: m, ExecutionProfileVer: "cpu", MemoryBytes: 100}
	}
	candidates := []Candidate{
		{Profile: profile("b"), Online: true, Stake: 1000, Reputation: 1},
		{Profile: profile("a"), Online: true, Stake: 1000, Reputation: 1},
		{Profile: profile("c"), Online: true, Stake: 1000, Reputation: 0.8},
	}
	var selected []string
	for i := uint64(0); i < 4; i++ {
		chosen, err := Choose(Request{Chain: "ember-test", Manifest: m, Profile: "cpu", TieBreak: i}, candidates)
		if err != nil {
			t.Fatal(err)
		}
		selected = append(selected, chosen.NodeID)
	}
	if !slices.Equal(selected, []string{"a", "b", "a", "b"}) {
		t.Fatalf("unexpected tie selection: %v", selected)
	}
	chosen, err := Choose(Request{Chain: "ember-test", Manifest: m, Profile: "cpu", Exclude: map[string]bool{"a": true}, TieBreak: 1}, candidates)
	if err != nil || chosen.NodeID != "b" {
		t.Fatalf("fallback selected %+v, %v", chosen, err)
	}
}

func TestChoosePipelineRequiresExactCoverage(t *testing.T) {
	m := make([]byte, 32)
	profile := func(id string, index, start, end uint32, embed, head bool) *pb.ResourceProfile {
		return &pb.ResourceProfile{NodeId: id, ChainId: "ember-sharded", ManifestRoot: m,
			ExecutionProfileVer: "pipeline-v1", MemoryBytes: 100, ShardIndex: index,
			ShardStartLayer: start, ShardEndLayer: end, ShardTotalLayers: 28,
			ShardHasEmbedding: embed, ShardHasLmHead: head}
	}
	candidates := []Candidate{
		{Profile: profile("first", 0, 0, 14, true, false), Online: true, Stake: 1000, Reputation: 1},
		{Profile: profile("last", 1, 14, 28, false, true), Online: true, Stake: 1000, Reputation: 1},
	}
	r := Request{Chain: "ember-sharded", Manifest: m, Profile: "pipeline-v1"}
	pipeline, err := ChoosePipeline(r, candidates, 28)
	if err != nil || len(pipeline.Nodes) != 2 || pipeline.Nodes[0].NodeID != "first" || pipeline.Nodes[1].NodeID != "last" {
		t.Fatalf("pipeline=%+v err=%v", pipeline, err)
	}
	for name, mutate := range map[string]func([]Candidate){
		"gap":               func(c []Candidate) { c[1].Profile.ShardStartLayer = 15 },
		"overlap":           func(c []Candidate) { c[1].Profile.ShardStartLayer = 13 },
		"missing embedding": func(c []Candidate) { c[0].Profile.ShardHasEmbedding = false },
		"early head":        func(c []Candidate) { c[0].Profile.ShardHasLmHead = true },
		"wrong profile":     func(c []Candidate) { c[1].Profile.ExecutionProfileVer = "other" },
	} {
		copyCandidates := make([]Candidate, len(candidates))
		for i, candidate := range candidates {
			copyCandidates[i] = candidate
			copyCandidates[i].Profile = proto.Clone(candidate.Profile).(*pb.ResourceProfile)
		}
		mutate(copyCandidates)
		if _, err := ChoosePipeline(r, copyCandidates, 28); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

// A candidate with a better score but no valid successor must not shadow a
// complete path. Malformed role/range candidates are filtered before ranking.
func TestPipelineSearchesPastDeadEndAndFailsOver(t *testing.T) {
	m := make([]byte, 32)
	stage := func(id string, index, start, end uint32, score float64) Candidate {
		return Candidate{Profile: &pb.ResourceProfile{NodeId: id, ChainId: "ember-p", ManifestRoot: m,
			ExecutionProfileVer: "p", MemoryBytes: 1, ShardIndex: index, ShardStartLayer: start,
			ShardEndLayer: end, ShardTotalLayers: 28, ShardHasEmbedding: start == 0, ShardHasLmHead: end == 28},
			Online: true, Stake: 1000, Reputation: score}
	}
	c := []Candidate{stage("dead-end", 0, 0, 15, 1), stage("primary", 0, 0, 14, .9),
		stage("spare", 0, 0, 14, .8), stage("last", 1, 14, 28, 1)}
	r := Request{Chain: "ember-p", Manifest: m, Profile: "p"}
	p, err := ChoosePipeline(r, c, 28)
	if err != nil || p.Nodes[0].NodeID != "primary" {
		t.Fatalf("%+v %v", p, err)
	}
	c[1].Online = false
	p, err = ChoosePipeline(r, c, 28)
	if err != nil || p.Nodes[0].NodeID != "spare" {
		t.Fatalf("%+v %v", p, err)
	}
	c[3].Online = false
	if _, err = ChoosePipeline(r, c, 28); err == nil {
		t.Fatal("incomplete coverage accepted")
	}
	plan, err := PlanPipeline(r, []Candidate{stage("primary", 0, 0, 14, .9),
		stage("spare", 0, 0, 14, .8), stage("last", 1, 14, 28, 1)}, 28)
	if err != nil || len(plan.Stages) != 2 || len(plan.Stages[0].Nodes) != 2 || plan.Stages[0].Nodes[1].NodeID != "spare" {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	if _, err = PlanPipeline(r, []Candidate{stage("first", 0, 0, 14, 1)}, 28); err == nil ||
		!strings.Contains(err.Error(), "layer 14") {
		t.Fatalf("missing range diagnostic: %v", err)
	}
}
