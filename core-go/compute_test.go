package core

import (
	pb "ember/proto"
	"testing"
)

func TestRangeComputeLegacyExact(t *testing.T) {
	m := &pb.ModelManifest{TotalLayers: 28}
	for start := uint32(0); start <= 28; start++ {
		for end := start; end <= 28; end++ {
			for _, embedding := range []bool{false, true} {
				for _, head := range []bool{false, true} {
					if got := RangeCompute(m, start, end, embedding, head); got != uint64(StageWeight(start, end)) {
						t.Fatalf("%d:%d = %d", start, end, got)
					}
				}
			}
		}
	}
	if TotalCompute(m) != 28 {
		t.Fatal("legacy denominator changed")
	}
}

func TestComputeAtomsAndWideWeights(t *testing.T) {
	m := &pb.ModelManifest{TotalLayers: 2, LayerFlops: []uint64{1 << 33, 2 << 33}, HeadFlops: 3}
	if RangeCompute(m, 0, 0, true, false) != 1 || RangeCompute(m, 2, 2, false, true) != 3 || TotalCompute(m) != (3<<33)+4 {
		t.Fatal("atom weights")
	}
	shares, err := SplitInvocationCharge(30, []StageWork{{NodeID: "a", Weight: 1 << 33, Tokens: 1}, {NodeID: "b", Weight: 2 << 33, Tokens: 1}})
	if err != nil || shares[0].Amount != 10 || shares[1].Amount != 20 {
		t.Fatal(shares, err)
	}
	if _, err := SplitInvocationCharge(1, []StageWork{{NodeID: "a", Weight: 1 << 63, Tokens: 2}}); err == nil {
		t.Fatal("multiplication overflow accepted")
	}
	if _, err := SplitInvocationCharge(1, []StageWork{{NodeID: "a", Weight: ^uint64(0), Tokens: 1}, {NodeID: "b", Weight: 1, Tokens: 1}}); err == nil {
		t.Fatal("sum overflow accepted")
	}
}
