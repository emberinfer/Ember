package gateway

import (
	pb "ember/proto"
	"ember/services/scheduler"
	"strings"
	"testing"
)

func TestResolvedPipelineRangePlanning(t *testing.T) {
	const chain = "ember-qwen2.5-1.5b-unified-v1"
	g := &Gateway{TotalLayers: 28, Profile: "transformers-qwen2.5-pipeline-range-v2", Manifest: make([]byte, 32), Pipeline: &PipelineConfig{DynamicRanges: true, MinStageLayers: 1}}
	req := scheduler.Request{Chain: chain, Manifest: g.Manifest, Profile: g.Profile, DynamicRanges: g.Pipeline.DynamicRanges, MinStageLayers: g.Pipeline.MinStageLayers, AllowedCuts: g.Pipeline.AllowedCuts}
	candidates := []scheduler.Candidate{
		{Online: true, Stake: 1000, Profile: &pb.ResourceProfile{NodeId: "stage-a", ChainId: chain, ExecutionProfileVer: g.Profile, ManifestRoot: g.Manifest, ShardTotalLayers: 28, ShardDynamicRange: true, ShardUnitKind: "range", ShardStartLayer: 0, ShardEndLayer: 14, ShardHasEmbedding: true}},
		{Online: true, Stake: 1000, Profile: &pb.ResourceProfile{NodeId: "stage-b", ChainId: chain, ExecutionProfileVer: g.Profile, ManifestRoot: g.Manifest, ShardTotalLayers: 28, ShardDynamicRange: true, ShardUnitKind: "range", ShardStartLayer: 14, ShardEndLayer: 28, ShardHasLmHead: true}},
	}
	plan, err := scheduler.PlanPipeline(req, candidates, g.TotalLayers)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Stages) != 2 {
		t.Fatalf("got %d stages", len(plan.Stages))
	}
	for i, stage := range plan.Stages {
		if stage.StartLayer != uint32(i*14) || stage.EndLayer != uint32((i+1)*14) {
			t.Fatalf("unexpected stage: %+v", stage)
		}
	}
	if _, err := scheduler.PlanPipeline(req, candidates[:1], g.TotalLayers); err == nil || !strings.Contains(err.Error(), "no complete cover of registered dynamic ranges") {
		t.Fatalf("expected incomplete cover error, got %v", err)
	}
}
