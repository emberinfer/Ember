package gateway

import (
	pb "ember/proto"
	"ember/services/scheduler"
	"math"
	"testing"
	"time"
)

func TestTTFTShortSamplesAndReplay(t *testing.T) {
	now := time.Now()
	stats := &SpeedStats{}
	legacy := &SpeedStats{}
	for _, id := range []string{"a", "b", "c"} {
		addSpeed(stats, id, "c", 6, 9, 10, now)
		addSpeed(legacy, id, "c", 6, 9, 10, now)
		delay := 800 * time.Millisecond
		if id == "c" {
			delay = 30 * time.Second
		}
		for i := 0; i < 6; i++ {
			stats.RecordTTFT(id, "c", 9, delay, now)
		}
	}
	r := scheduler.Request{Chain: "model", Profile: "v", SpeedEnabled: true}
	c := []scheduler.Candidate{}
	for _, id := range []string{"a", "b", "c"} {
		score, neutral, degraded, n := stats.Score(id, "c", now)
		want := 1.0
		if id == "c" {
			want = .5
		}
		old, oldNeutral, _, _ := legacy.Score(id, "c", now)
		if score != want || neutral || degraded || n != 6 || old != 1 || !oldNeutral {
			t.Fatal(id, score, neutral, degraded, n, old)
		}
		c = append(c, scheduler.Candidate{Profile: &pb.ResourceProfile{NodeId: id, ChainId: "model", ExecutionProfileVer: "v"}, Online: true, Stake: 1000, Reputation: .8, SpeedScore: score})
	}
	oldLogger := scheduler.ShadowLogger
	scheduler.ShadowLogger = nil
	defer func() { scheduler.ShadowLogger = oldLogger }()
	counts := [2][3]int{}
	ranks := scheduler.Rank(r, c)
	for mode := 0; mode < 2; mode++ {
		for i := 0; i < 20000; i++ {
			r.TieBreak = uint64(i)
			n, e := scheduler.Choose(r, c)
			if e != nil {
				t.Fatal(e)
			}
			counts[mode][int(n.NodeID[0]-'a')]++
		}
		for i := range c {
			c[i].SpeedScore = 1
		}
	}
	neutralRanks := scheduler.Rank(r, c)
	for i := range ranks {
		if math.Float64bits(ranks[i].Score) != math.Float64bits(neutralRanks[i].Score) {
			t.Fatal("TTFT changed trust rank")
		}
	}
	share := float64(counts[0][2]) / 20000
	relative := float64(counts[0][0]) / float64(counts[0][0]+counts[0][1])
	oldRelative := float64(counts[1][0]) / float64(counts[1][0]+counts[1][1])
	if share <= .05 || share >= .35 || math.Abs(relative-oldRelative) > .02 {
		t.Fatal(counts)
	}
	t.Logf("TTFT replay counts=%v shares=%g,%g,%g; neutral counts=%v shares=%g,%g,%g; normal relative delta=%g", counts[0], float64(counts[0][0])/20000, float64(counts[0][1])/20000, share, counts[1], float64(counts[1][0])/20000, float64(counts[1][1])/20000, float64(counts[1][2])/20000, relative-oldRelative)
	snap := stats.Snapshot(now)
	if len(snap) != 3 || snap[2].TTFTN != 6 || snap[2].MedianTTFTSeconds != 30 || snap[2].TTFTScore != .5 {
		t.Fatal(snap)
	}
}
func TestTTFTValidityWindowAndComposition(t *testing.T) {
	now := time.Now()
	s := &SpeedStats{}
	var absent *SpeedStats
	absent.RecordTTFT("n", "c", 4, time.Second, now)
	s.RecordTTFT("bad", "c", 3, time.Second, now)
	s.RecordTTFT("bad", "c", 4, 0, now)
	if len(s.Snapshot(now)) != 0 {
		t.Fatal("invalid accepted")
	}
	for i := 0; i < 5; i++ {
		s.RecordTTFT("old", "c", 4, time.Millisecond, now.Add(-25*time.Hour))
	}
	if s.Snapshot(now)[0].TTFTN != 5 {
		t.Fatal("fallback")
	}
	for i := 0; i < 10; i++ {
		s.RecordTTFT("base", "c", 4, time.Second, now)
	}
	for _, v := range s.Snapshot(now) {
		if v.NodeID == "old" && v.TTFTN != 0 {
			t.Fatal("24h window")
		}
	}
	s.RecordTTFT("expired", "c", 4, time.Second, now.Add(-73*time.Hour))
	s.Score("base", "c", now)
	for _, v := range s.ttftSamples {
		if v.node == "expired" {
			t.Fatal("pruning")
		}
	}
	addSpeed(s, "base", "c", 10, 100, 10, now)
	addSpeed(s, "slow", "c", 5, 100, 20, now)
	for i := 0; i < 5; i++ {
		s.RecordTTFT("slow", "c", 4, 30*time.Second, now)
	}
	score, neutral, _, _ := s.Score("slow", "c", now)
	if score != 1 || neutral {
		t.Fatal(score, neutral)
	}
}
