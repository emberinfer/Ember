package scheduler

import (
	core "ember/core-go"
	pb "ember/proto"
	"math"
	"reflect"
	"testing"
)

func speedFixture(scores ...float64) (Request, []Candidate) {
	r := Request{Chain: "model", Profile: "v1", SpeedEnabled: true}
	c := []Candidate{}
	for i, v := range scores {
		c = append(c, Candidate{Profile: &pb.ResourceProfile{NodeId: string(rune('a' + i)), ChainId: r.Chain, ExecutionProfileVer: r.Profile}, Online: true, Stake: 1000, Reputation: .8, SpeedScore: v})
	}
	return r, c
}
func silenceShadow(t *testing.T) {
	old := ShadowLogger
	ShadowLogger = nil
	t.Cleanup(func() { ShadowLogger = old })
}
func replay(t *testing.T, scores ...float64) []int {
	t.Helper()
	silenceShadow(t)
	r, c := speedFixture(scores...)
	counts := make([]int, len(c))
	for i := 0; i < 20000; i++ {
		r.TieBreak = uint64(i)
		v, err := Choose(r, c)
		if err != nil {
			t.Fatal(err)
		}
		counts[int(v.NodeID[0]-'a')]++
	}
	t.Logf("draws=20000 counts=%v shares=%v", counts, func() []float64 {
		v := []float64{}
		for _, n := range counts {
			v = append(v, float64(n)/20000)
		}
		return v
	}())
	return counts
}
func TestSpeedReplayThinFleet(t *testing.T) {
	n := replay(t, 1, 17.4/16.4)
	b := float64(n[1]) / 20000
	if b < .48 || b > .54 || float64(n[0])/20000 < .44 {
		t.Fatal(n)
	}
}
func TestSpeedMaxTilt(t *testing.T) {
	n := replay(t, 2, .5)
	f := float64(n[0]) / 20000
	if f < .72 || f > .82 || float64(n[1])/20000 < .15 || float64(n[0])/float64(n[1]) > 4 {
		t.Fatal(n)
	}
}
func TestSpeedDegradedReplayShares(t *testing.T) {
	n := replay(t, 1, 1, .5)
	v := float64(n[2]) / 20000
	if v <= .05 || v >= .25 {
		t.Fatal(n)
	}
}
func TestSpeedAffinityFirst(t *testing.T) {
	silenceShadow(t)
	r, c := speedFixture(.5, 2)
	r.Prefix = "x"
	c[0].Prefix = "x"
	for i := 0; i < 2000; i++ {
		r.TieBreak = uint64(i)
		v, _ := Choose(r, c)
		if v.NodeID != "a" {
			t.Fatal(v)
		}
	}
}
func TestSpeedDisabledMatchesLegacy(t *testing.T) {
	silenceShadow(t)
	r, c := speedFixture(2, .5, 1)
	r.SpeedEnabled = false
	c[2].Reputation = .1
	for i := 0; i < 100; i++ {
		r.TieBreak = uint64(i)
		v, _ := Choose(r, c)
		if v.NodeID != string(rune('a'+i%2)) {
			t.Fatal(v)
		}
	}
}
func TestSpeedShadowLogged(t *testing.T) {
	old := ShadowLogger
	defer func() { ShadowLogger = old }()
	for _, enabled := range []bool{false, true} {
		entries := []ShadowEntry{}
		ShadowLogger = func(e ShadowEntry) { entries = append(entries, e) }
		r, c := speedFixture(1, 2)
		r.SpeedEnabled = enabled
		Choose(r, c)
		if len(entries) != 1 || entries[0].ActualNodeID == "" || entries[0].ShadowNodeID == "" || entries[0].Stage != -1 {
			t.Fatal(entries)
		}
	}
}
func TestSpeedTrustAndRank(t *testing.T) {
	silenceShadow(t)
	r, c := speedFixture(0, 2)
	c[1].Stake = 0
	for i := 0; i < 2000; i++ {
		r.TieBreak = uint64(i)
		v, _ := Choose(r, c)
		if v.NodeID != "a" {
			t.Fatal(v)
		}
	}
	c[1].Stake = 1000
	a := Rank(r, c)
	c[0].SpeedScore = 2
	c[1].SpeedScore = .5
	b := Rank(r, c)
	for i := range a {
		if math.Float64bits(a[i].Score) != math.Float64bits(b[i].Score) {
			t.Fatal(a, b)
		}
		a[i].SpeedScore = b[i].SpeedScore
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatal(a, b)
	}
}
func TestSpeedStageAndLatency(t *testing.T) {
	old := ShadowLogger
	defer func() { ShadowLogger = old }()
	entries := []ShadowEntry{}
	ShadowLogger = func(e ShadowEntry) { entries = append(entries, e) }
	r, c := speedFixture(.5, 2)
	nodes := Rank(r, c)
	r.SpeedEnabled = false
	if SelectStage(r, nodes, 2).NodeID != nodes[0].NodeID || len(entries) != 1 || entries[0].Stage != 2 {
		t.Fatal(entries)
	}
	entries = nil
	r.LatencyPolicy = &core.SchedulingPolicy{ExplorationPeriod: 100, MinSamples: 1}
	r.SpeedEnabled = true
	v, _ := Choose(r, c)
	if v.NodeID != Rank(r, c)[0].NodeID || len(entries) != 0 {
		t.Fatal(v, entries)
	}
}

func TestSpeedPipelineSampling(t *testing.T) {
	old := ShadowLogger
	defer func() { ShadowLogger = old }()
	r, c := speedFixture(.5, 2)
	for _, v := range c {
		v.Profile.ShardTotalLayers = 2
		v.Profile.ShardEndLayer = 2
		v.Profile.ShardHasEmbedding = true
		v.Profile.ShardHasLmHead = true
	}
	counts := map[string]int{}
	for i := 0; i < 2000; i++ {
		r.TieBreak = uint64(i)
		var entries []ShadowEntry
		ShadowLogger = func(e ShadowEntry) { entries = append(entries, e) }
		p, err := ChoosePipeline(r, c, 2)
		if err != nil || len(p.Nodes) != 1 {
			t.Fatal(p, err)
		}
		counts[p.Nodes[0].NodeID]++
		if len(entries) != 1 || entries[0].Stage != 0 || entries[0].ActualNodeID != p.Nodes[0].NodeID || entries[0].ShadowNodeID != "a" {
			t.Fatal(entries)
		}
		r.Shards = true
		expected := weightedPick(Rank(r, c), r.TieBreak^uint64(0x9E3779B97F4A7C15))
		if p.Nodes[0].NodeID != expected.NodeID {
			t.Fatal(p, expected)
		}
	}
	if counts["a"] < 300 || counts["b"] < 1400 {
		t.Fatal(counts)
	}
	r.SpeedEnabled = false
	p, err := ChoosePipeline(r, c, 2)
	if err != nil || p.Nodes[0].NodeID != "a" {
		t.Fatal(p, err)
	}
}
func TestSpeedDefensiveWeightsAndShadowIsolation(t *testing.T) {
	for _, tc := range []struct{ in, want float64 }{{0, 1}, {-1, 1}, {.1, .5}, {3, 2}, {math.NaN(), 1}} {
		if speedWeight(tc.in) != tc.want {
			t.Fatal(tc)
		}
	}
	old := ShadowLogger
	defer func() { ShadowLogger = old }()
	r, c := speedFixture(1, 2)
	for i := 0; i < 100; i++ {
		r.TieBreak = uint64(i)
		ShadowLogger = nil
		a, _ := Choose(r, c)
		var entry ShadowEntry
		ShadowLogger = func(e ShadowEntry) { entry = e }
		b, _ := Choose(r, c)
		if a.NodeID != b.NodeID || entry.ShadowNodeID != legacyID(r, Rank(r, c)) {
			t.Fatal(a, b, entry)
		}
		r.SpeedEnabled = false
		Choose(r, c)
		if entry.ShadowNodeID != weightedPick(Rank(r, c), r.TieBreak^0x5EED5EED).NodeID {
			t.Fatal(entry)
		}
		r.SpeedEnabled = true
	}
}
func legacyID(r Request, nodes []Ranked) string {
	n := 1
	for n < len(nodes) && nodes[n].Score == nodes[0].Score {
		n++
	}
	return nodes[r.TieBreak%uint64(n)].NodeID
}
