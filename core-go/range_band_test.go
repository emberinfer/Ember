package core

import (
	pb "ember/proto"
	"math"
	"testing"
)

func TestRangeThresholds(t *testing.T) {
	p := &pb.RangeBandParams{Metric: "relative-l2-v1", DepthCoef: .5, HardMultiplier: 3, Base: []*pb.RangeDeviceBase{{ProducerDevice: "cpu", ReferenceDevice: "cuda", Base: .02}}, HeadBase: []*pb.RangeDeviceBase{{ProducerDevice: "cpu", ReferenceDevice: "cuda", Base: .03}}}
	for _, v := range []struct {
		s, e uint32
		head bool
		want float64
	}{{4, 8, false, .048}, {0, 0, false, .02}, {20, 20, true, .03}, {0, 20, true, .03}} {
		a, b, err := RangeThresholds(p, v.s, v.e, 20, "cpu", "cuda", v.head)
		if err != nil || math.Abs(a-v.want) > 1e-15 || math.Abs(b-3*v.want) > 1e-15 {
			t.Fatalf("%+v: %g %g %v", v, a, b, err)
		}
	}
	if _, _, err := RangeThresholds(p, 0, 1, 20, "cpu", "cpu", false); err == nil {
		t.Fatal("missing pair accepted")
	}
	p.Base = append(p.Base, p.Base[0])
	if ValidateRangeBand(p) == nil {
		t.Fatal("duplicate accepted")
	}
}
