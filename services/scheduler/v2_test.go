package scheduler

import (
	"bytes"
	core "ember/core-go"
	pb "ember/proto"
	"testing"
)

func TestOverlappingRangesProduceDifferentLegalPartitions(t *testing.T) {
	root := bytes.Repeat([]byte{1}, 32)
	makeCandidate := func(id string, a, b uint32) Candidate {
		return Candidate{Profile: &pb.ResourceProfile{NodeId: id, ChainId: "ember-ranges", ExecutionProfileVer: "v", ManifestRoot: root, ShardDynamicRange: true, ShardUnitKind: "range", ShardStartLayer: a, ShardEndLayer: b, ShardTotalLayers: 28, ShardHasEmbedding: a == 0, ShardHasLmHead: b == 28}, Online: true, Stake: 1000}
	}
	candidates := []Candidate{makeCandidate("first", 0, 20), makeCandidate("last", 8, 28), makeCandidate("backup", 8, 28)}
	req := Request{Chain: "ember-ranges", Manifest: root, Profile: "v", DynamicRanges: true, MinStageLayers: 1}
	seen := map[uint32]bool{}
	for i := uint64(0); i < 13; i++ {
		req.TieBreak = i
		plan, err := PlanPipeline(req, candidates, 28)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Stages) != 2 || plan.Stages[0].StartLayer != 0 || plan.Stages[1].EndLayer != 28 || plan.Stages[0].EndLayer != plan.Stages[1].StartLayer || len(plan.Stages[1].Nodes) != 2 {
			t.Fatal("bad dynamic cover", plan)
		}
		seen[plan.Stages[0].EndLayer] = true
	}
	if len(seen) != 13 {
		t.Fatal("partition rotation missing", seen)
	}
	req.MinStageLayers = 4
	for i := uint64(0); i < 4; i++ {
		req.TieBreak = i
		p, err := PlanPipeline(req, candidates, 28)
		if err != nil || p.Stages[0].EndLayer%4 != 0 {
			t.Fatal("granularity violated")
		}
	}
	candidates[0].Online = false
	if _, err := PlanPipeline(req, candidates, 28); err == nil {
		t.Fatal("missing prefix coverage accepted")
	}
}

func TestReceiptLatencyPolicyAndExplorationFloor(t *testing.T) {
	root := bytes.Repeat([]byte{2}, 32)
	policy := &core.SchedulingPolicy{Scoring: "auditor-roundtrip-us-per-unit-v1", WitnessAuthority: "assigned-auditor", ExplorationPPM: 200000, ExplorationPeriod: 10, PercentilePPM: 900000, WindowSamples: 100, MinSamples: 2, MaxObservationNS: 60000000000}
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}
	makeCandidate := func(id string, latency uint64) Candidate {
		return Candidate{Profile: &pb.ResourceProfile{NodeId: id, ChainId: "ember-latency", ExecutionProfileVer: "v", ManifestRoot: root}, Online: true, Stake: 1000, LatencyMicrosPerUnit: latency, LatencySamples: 3}
	}
	candidates := []Candidate{makeCandidate("fast", 100), makeCandidate("slow", 10000)}
	candidates[0].RTTMillis = 1000000
	candidates[1].Reputation = 1
	candidates[1].Prefix = "fake-speed"
	req := Request{Chain: "ember-latency", Manifest: root, Profile: "v", Prefix: "fake-speed", LatencyPolicy: policy}
	count := map[string]int{}
	for i := uint64(0); i < 50; i++ {
		req.TieBreak = i
		chosen, err := Choose(req, candidates)
		if err != nil {
			t.Fatal(err)
		}
		count[chosen.NodeID]++
	}
	if count["fast"] != 40 || count["slow"] != 10 {
		t.Fatal("percentile/exploration policy ignored", count)
	}
	candidates[1].LatencySamples = 0
	candidates[1].LatencyMicrosPerUnit = 0
	count = map[string]int{}
	for i := uint64(0); i < 10; i++ {
		req.TieBreak = i
		chosen, _ := Choose(req, candidates)
		count[chosen.NodeID]++
	}
	if count["slow"] != 2 {
		t.Fatal("unproven node lost exploration floor", count)
	}
}
