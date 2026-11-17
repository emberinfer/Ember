package gateway

import (
	pb "ember/proto"
	"ember/services/scheduler"
	"google.golang.org/protobuf/proto"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestExecExcludeDispatchAndIsolation(t *testing.T) {
	r := scheduler.Request{Chain: "model", Profile: "v", SpeedEnabled: true}
	candidates := []scheduler.Candidate{}
	for _, id := range []string{"a", "b", "c"} {
		candidates = append(candidates, scheduler.Candidate{Profile: &pb.ResourceProfile{NodeId: id, ChainId: "model", ExecutionProfileVer: "v", ShardTotalLayers: 2, ShardEndLayer: 2, ShardHasEmbedding: true, ShardHasLmHead: true, ShardDynamicRange: true}, Online: true, Stake: 1000, Reputation: .8, SpeedScore: 1})
	}
	original := append([]scheduler.Candidate(nil), candidates...)
	g := &Gateway{Candidates: func() []scheduler.Candidate { return candidates }}
	baseline := unexpiredCandidates(candidates, time.Now())
	for _, exclusions := range []map[string]bool{nil, {}} {
		g.ExecExclude = exclusions
		if !reflect.DeepEqual(baseline, g.liveCandidates()) {
			t.Fatal("empty exclusions changed snapshot")
		}
		for i := 0; i < 100; i++ {
			r.TieBreak = uint64(i)
			a, e := scheduler.Choose(r, wholeCandidates(baseline))
			b, f := scheduler.Choose(r, wholeCandidates(g.liveCandidates()))
			if e != nil || f != nil || !reflect.DeepEqual(a, b) {
				t.Fatal(a, b, e, f)
			}
		}
		a, e := scheduler.PlanPipeline(r, baseline, 2)
		b, f := scheduler.PlanPipeline(r, g.liveCandidates(), 2)
		if e != nil || f != nil || !reflect.DeepEqual(a, b) {
			t.Fatal(a, b, e, f)
		}
	}
	g.ExecExclude = map[string]bool{"a": true}
	for i := 0; i < 2000; i++ {
		r.TieBreak = uint64(i)
		n, e := scheduler.Choose(r, wholeCandidates(g.liveCandidates()))
		if e != nil || n.NodeID == "a" {
			t.Fatal(n, e)
		}
	}
	for _, dynamic := range []bool{false, true} {
		r.DynamicRanges = dynamic
		r.MinStageLayers = 1
		plan, e := scheduler.PlanPipeline(r, g.liveCandidates(), 2)
		if e != nil {
			t.Fatal(e)
		}
		for _, stage := range plan.Stages {
			for _, n := range stage.Nodes {
				if n.NodeID == "a" {
					t.Fatal(plan)
				}
			}
		}
		if e = g.pipelineAvailable(plan, r, g.liveCandidates()); e != nil {
			t.Fatal(e)
		}
		g.ExecExclude = map[string]bool{"a": true, "b": true, "c": true}
		if _, e = scheduler.PlanPipeline(r, g.liveCandidates(), 2); e == nil {
			t.Fatal("empty pool planned")
		}
		if e = g.pipelineAvailable(plan, r, g.liveCandidates()); e == nil {
			t.Fatal("empty pool available")
		}
		g.ExecExclude = map[string]bool{"a": true}
	}
	saved := g.Candidates
	g.Candidates = func() []scheduler.Candidate { return candidates[:1] }
	if _, err := scheduler.PlanPipeline(r, g.liveCandidates(), 2); err == nil {
		t.Fatal("sole excluded stage candidate planned")
	}
	g.Candidates = saved
	r.Shards = true
	full := scheduler.Rank(r, candidates)
	filtered := scheduler.Rank(r, g.liveCandidates())
	want := []scheduler.Ranked{}
	found := false
	for _, n := range full {
		if n.NodeID == "a" {
			found = true
		} else {
			want = append(want, n)
		}
	}
	if !found || !reflect.DeepEqual(want, filtered) || !reflect.DeepEqual(original, candidates) {
		t.Fatal("trust/rank/source changed")
	}
}
func TestLoadExecExcludeFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "exclude")
	if _, e := LoadExecExcludeFile(p); e == nil {
		t.Fatal("missing file accepted")
	}
	for _, tc := range []struct {
		body string
		bad  bool
	}{{" # comment\n\n aa6b3ccd \n663270c1\naa6b3ccd\n", false}, {"not an id\n", true}, {"!!!\n", true}} {
		if e := os.WriteFile(p, []byte(tc.body), 0600); e != nil {
			t.Fatal(e)
		}
		got, e := LoadExecExcludeFile(p)
		if (e != nil) != tc.bad {
			t.Fatal(got, e)
		}
		if !tc.bad && !reflect.DeepEqual(got, map[string]bool{"aa6b3ccd": true, "663270c1": true}) {
			t.Fatal(got)
		}
	}
}

func wholeCandidates(in []scheduler.Candidate) []scheduler.Candidate {
	out := append([]scheduler.Candidate(nil), in...)
	for i := range out {
		out[i].Profile = proto.Clone(out[i].Profile).(*pb.ResourceProfile)
		out[i].Profile.ShardTotalLayers = 0
	}
	return out
}
