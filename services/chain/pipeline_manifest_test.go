package chain

import (
	"bytes"
	pb "ember/proto"
	"testing"
)

func pipelineSchemeFixture() *pb.ModelManifest {
	s := &pb.LayoutScheme{TotalLayers: 28, MinStageLayers: 1, LayerFlops: make([]uint64, 28), HeadFlops: 466750464}
	for i := range s.LayerFlops {
		s.LayerFlops[i] = 1
	}
	for i := uint32(0); i < 30; i++ {
		a := &pb.ShardSpec{Index: i, ParametersHash: bytes.Repeat([]byte{byte(i + 1)}, 32)}
		switch {
		case i == 0:
			a.UnitKind = "embedding"
			a.HasEmbedding = true
		case i == 29:
			a.UnitKind = "lm_head"
			a.StartLayer = 28
			a.EndLayer = 28
			a.HasLmHead = true
		default:
			a.UnitKind = "layers"
			a.StartLayer = i - 1
			a.EndLayer = i
		}
		s.Atoms = append(s.Atoms, a)
	}
	return &pb.ModelManifest{ChainId: "ember-qwen2.5-1.5b-unified-v1", Schemes: []*pb.LayoutScheme{s}}
}

func TestResolvePipelineManifest(t *testing.T) {
	m := pipelineSchemeFixture()
	resolved, err := ResolvePipelineManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.DynamicRanges || resolved.TotalLayers != 28 || resolved.MinStageLayers != 1 || len(resolved.Shards) != 0 || len(resolved.Schemes) != 0 {
		t.Fatalf("unexpected resolved layout: %v", resolved)
	}
	if !(len(resolved.Shards) >= 2 || resolved.DynamicRanges) {
		t.Fatal("pipeline gate rejects resolved layout")
	}
	if m.TotalLayers != 0 || len(m.Schemes) != 1 {
		t.Fatal("raw manifest changed")
	}
	legacy := &pb.ModelManifest{TotalLayers: 12}
	if got, err := ResolvePipelineManifest(legacy); err != nil || got != legacy {
		t.Fatalf("legacy manifest changed: %v", err)
	}
	m.Schemes = append(m.Schemes, m.Schemes[0])
	if _, err := ResolvePipelineManifest(m); err == nil {
		t.Fatal("accepted ambiguous schemes")
	}
}

func TestSelectPipelineScheme(t *testing.T) {
	m := pipelineSchemeFixture()
	got, err := SelectScheme(m, schemeHash(m.Schemes[0]))
	if err != nil {
		t.Fatal(err)
	}
	if got.TotalLayers != 28 || got.MinStageLayers != 1 || !got.DynamicRanges {
		t.Fatalf("unexpected layout: %v", got)
	}
	if _, err := SelectScheme(m, make([]byte, 32)); err == nil {
		t.Fatal("accepted unknown scheme")
	}
}

func TestResolvePipelineManifestInvalid(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*pb.LayoutScheme)
	}{
		{"atoms", func(s *pb.LayoutScheme) { s.Atoms[1].EndLayer = 2 }},
		{"zero layers", func(s *pb.LayoutScheme) {
			s.TotalLayers = 0
			s.LayerFlops = nil
			s.Atoms = []*pb.ShardSpec{s.Atoms[0], {UnitKind: "lm_head", HasLmHead: true, ParametersHash: make([]byte, 32)}}
		}},
		{"zero minimum", func(s *pb.LayoutScheme) { s.MinStageLayers = 0 }},
		{"large minimum", func(s *pb.LayoutScheme) { s.MinStageLayers = 29 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := pipelineSchemeFixture()
			tc.mutate(m.Schemes[0])
			if _, err := ResolvePipelineManifest(m); err == nil {
				t.Fatal("accepted invalid layout")
			}
		})
	}
}
