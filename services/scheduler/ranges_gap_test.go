package scheduler

import (
	pb "ember/proto"
	"errors"
	"strings"
	"testing"
)

func TestRangeCoverGaps(t *testing.T) {
	for _, tc := range []struct {
		ranges [][2]uint32
		gaps   [][2]uint32
	}{
		{[][2]uint32{{0, 14}}, [][2]uint32{{14, 28}}},
		{[][2]uint32{{0, 10}, {14, 28}}, [][2]uint32{{10, 14}}},
		{[][2]uint32{{5, 28}}, [][2]uint32{{0, 5}}},
		{[][2]uint32{{3, 10}, {14, 20}}, [][2]uint32{{0, 3}, {10, 14}, {20, 28}}},
	} {
		candidates := []Candidate{}
		for i, span := range tc.ranges {
			candidates = append(candidates, Candidate{Profile: &pb.ResourceProfile{NodeId: string(rune('a' + i)), ChainId: "gap", ExecutionProfileVer: "v", ManifestRoot: []byte{1}, ShardDynamicRange: true, ShardTotalLayers: 28, ShardStartLayer: span[0], ShardEndLayer: span[1], ShardHasEmbedding: span[0] == 0, ShardHasLmHead: span[1] == 28}, Online: true, Stake: 1000})
		}
		_, err := PlanRanges(Request{Chain: "gap", Profile: "v", Manifest: []byte{1}, MinStageLayers: 1}, candidates, 28)
		var failure *RangeCoverError
		if !errors.As(err, &failure) || len(failure.Gaps) != len(tc.gaps) {
			t.Fatalf("%v: %v", tc.ranges, err)
		}
		for i, span := range tc.gaps {
			if failure.Gaps[i].StartLayer != span[0] || failure.Gaps[i].EndLayer != span[1] {
				t.Fatal(failure)
			}
		}
		if !strings.Contains(err.Error(), "uncovered") {
			t.Fatal(err)
		}
	}
}
