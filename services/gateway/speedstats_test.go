package gateway

import (
	pb "ember/proto"
	"ember/services/verifier"
	"google.golang.org/protobuf/proto"
	"math"
	"testing"
	"time"
)

func addSpeed(s *SpeedStats, node, cohort string, n, tokens int, speed float64, at time.Time) {
	for i := 0; i < n; i++ {
		s.Record(node, cohort, tokens, time.Duration(float64(tokens)/speed*float64(time.Second)), at)
	}
}
func TestWeightedMedian(t *testing.T) {
	v := []weightedValue{{30, 1}, {10, 2}, {20, 4}}
	if weightedMedian(v) != 20 || v[0].value != 30 || weightedMedian(nil) != 0 || weightedMedian([]weightedValue{{2, 1}, {1, 1}}) != 1 {
		t.Fatal(v)
	}
}
func TestSpeedStatsGate(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		n, tokens int
		neutral   bool
	}{{4, 100, true}, {6, 33, true}, {5, 60, false}} {
		s := &SpeedStats{}
		addSpeed(s, "base", "c", 20, 100, 10, now)
		addSpeed(s, "node", "c", tc.n, tc.tokens, 20, now)
		score, neutral, _, n := s.Score("node", "c", now)
		if neutral != tc.neutral || n != tc.n || (neutral && score != 1) || (!neutral && score != 2) {
			t.Fatal(tc, score, neutral, n)
		}
	}
}
func TestSpeedStatsWindow(t *testing.T) {
	now := time.Now()
	s := &SpeedStats{}
	addSpeed(s, "old", "c", 1, 100, 10, now.Add(-25*time.Hour))
	addSpeed(s, "expired", "c", 1, 100, 10, now.Add(-73*time.Hour))
	_, _, _, n := s.Score("old", "c", now)
	if n != 1 {
		t.Fatal(n)
	}
	_, _, _, n = s.Score("expired", "c", now)
	if n != 0 {
		t.Fatal(n)
	}
	addSpeed(s, "fresh", "c", 10, 100, 10, now)
	_, _, _, n = s.Score("old", "c", now)
	if n != 0 {
		t.Fatal(n)
	}
	for _, v := range s.samples {
		if v.node == "expired" {
			t.Fatal("not pruned")
		}
	}
}
func TestSpeedStatsClampDegraded(t *testing.T) {
	now := time.Now()
	for _, n := range []int{9, 10} {
		s := &SpeedStats{}
		addSpeed(s, "base", "c", 40, 100, 10, now)
		addSpeed(s, "fast", "c", 5, 100, 30, now)
		addSpeed(s, "slow", "c", n, 100, 3, now)
		fast, _, _, _ := s.Score("fast", "c", now)
		slow, neutral, degraded, _ := s.Score("slow", "c", now)
		if fast != 2 || slow != .5 || neutral || degraded != (n == 10) {
			t.Fatal(fast, slow, neutral, degraded)
		}
		snap := s.Snapshot(now)
		if len(snap) != 3 || snap[0].NodeID != "base" || snap[2].Degraded != (n == 10) {
			t.Fatal(snap)
		}
	}
}
func TestSpeedStatsCohortsAndValidity(t *testing.T) {
	now := time.Now()
	s := &SpeedStats{}
	whole := CohortKey("p", false, 0, 0)
	shard := CohortKey("p", true, 0, 10)
	addSpeed(s, "n", whole, 10, 100, 10, now)
	score, neutral, _, n := s.Score("n", shard, now)
	if score != 1 || !neutral || n != 0 || whole != "p|whole" || shard != "p|shard:0-10" {
		t.Fatal(score, neutral, n)
	}
	s.Record("bad", shard, 3, time.Second, now)
	s.Record("bad", shard, 100, 499*time.Millisecond, now)
	if len(s.Snapshot(now)) != 1 {
		t.Fatal(s.Snapshot(now))
	}
	var absent *SpeedStats
	absent.Record("", "", 4, time.Second, now)
	if absent.Snapshot(now) != nil {
		t.Fatal("nil")
	}
}
func TestPipelineSpeedAggregation(t *testing.T) {
	now := time.Now()
	p := &pb.ResourceProfile{NodeId: "node", ExecutionProfileVer: "p", ShardStartLayer: 2, ShardEndLayer: 4}
	wire, _ := proto.Marshal(p)
	e := &PipelineExecution{}
	for i := 0; i < 4; i++ {
		e.Invocations = append(e.Invocations, verifier.PipelineInvocation{Profile: wire, StageMS: 200})
	}
	g := &Gateway{SpeedStats: &SpeedStats{}}
	g.recordPipelineSpeed(e, now)
	snap := g.SpeedStats.Snapshot(now)
	if len(snap) != 1 || snap[0].Tokens != 4 || math.Abs(snap[0].MedianTokPerSec-5) > 1e-9 || snap[0].Cohort != "p|shard:2-4" {
		t.Fatal(snap)
	}
}
