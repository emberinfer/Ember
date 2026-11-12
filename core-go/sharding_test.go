package core

import "testing"

func TestExplicitAndLegacyStageSpans(t *testing.T) {
	for _, v := range []struct {
		a, b       uint32
		e, h       bool
		start, end uint32
	}{{0, 0, true, false, 0, 1}, {0, 14, false, false, 1, 15}, {14, 28, false, false, 15, 29}, {28, 28, false, true, 29, 30}, {0, 14, true, false, 0, 15}, {14, 28, false, true, 15, 30}} {
		a, b, err := StageSpan(v.a, v.b, 28, v.e, v.h)
		if err != nil || a != v.start || b != v.end {
			t.Fatalf("bad span %+v: %d %d %v", v, a, b, err)
		}
	}
	for _, v := range [][4]uint32{{0, 0, 0, 0}, {14, 14, 0, 0}, {0, 1, 0, 1}, {14, 28, 1, 0}} {
		if _, _, err := StageSpan(v[0], v[1], 28, v[2] == 1, v[3] == 1); err == nil {
			t.Fatal("invalid boundary accepted", v)
		}
	}
}
