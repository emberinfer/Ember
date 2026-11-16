package gateway

import (
	"fmt"
	"log"
	"math"
	"sort"
	"sync"
	"time"
)

type speedSample struct {
	node, cohort string
	tokens       int
	speed        float64
	at           time.Time
}
type weightedValue struct {
	value  float64
	weight int64
}

// weightedMedian returns the lower median at an exact half-weight boundary.
func weightedMedian(values []weightedValue) float64 {
	values = append([]weightedValue(nil), values...)
	sort.Slice(values, func(i, j int) bool { return values[i].value < values[j].value })
	var total, seen int64
	for _, v := range values {
		if v.weight > 0 {
			total += v.weight
		}
	}
	if total == 0 {
		return 0
	}
	for _, v := range values {
		if v.weight > 0 {
			seen += v.weight
		}
		if seen >= (total+1)/2 {
			return v.value
		}
	}
	return 0
}

type SpeedStats struct {
	mu       sync.Mutex
	samples  []speedSample
	degraded map[string]bool
}
type CohortNodeStat struct {
	Cohort, NodeID         string
	N                      int
	Tokens                 int64
	MedianTokPerSec, Score float64
	Neutral, Degraded      bool
}

func CohortKey(profile string, shard bool, startLayer, endLayer uint32) string {
	if shard {
		return fmt.Sprintf("%s|shard:%d-%d", profile, startLayer, endLayer)
	}
	return profile + "|whole"
}
func (s *SpeedStats) prune(now time.Time) {
	out := s.samples[:0]
	for _, v := range s.samples {
		if !v.at.Before(now.Add(-72 * time.Hour)) {
			out = append(out, v)
		}
	}
	s.samples = out
}
func (s *SpeedStats) Record(nodeID, cohort string, outputTokens int, genDuration time.Duration, at time.Time) {
	if s == nil || outputTokens < 4 || genDuration < 500*time.Millisecond {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(at)
	s.samples = append(s.samples, speedSample{nodeID, cohort, outputTokens, float64(outputTokens) / genDuration.Seconds(), at})
	s.stat(nodeID, cohort, at)
}
func (s *SpeedStats) stat(node, cohort string, now time.Time) CohortNodeStat {
	cutoff := now.Add(-24 * time.Hour)
	count := 0
	for _, v := range s.samples {
		if v.cohort == cohort && !v.at.Before(cutoff) && !v.at.After(now) {
			count++
		}
	}
	if count < 10 {
		cutoff = now.Add(-72 * time.Hour)
	}
	values := map[string][]weightedValue{}
	tokens := map[string]int64{}
	counts := map[string]int{}
	for _, v := range s.samples {
		if v.cohort == cohort && !v.at.Before(cutoff) && !v.at.After(now) {
			values[v.node] = append(values[v.node], weightedValue{v.speed, int64(v.tokens)})
			tokens[v.node] += int64(v.tokens)
			counts[v.node]++
		}
	}
	group := []weightedValue{}
	for id, v := range values {
		group = append(group, weightedValue{weightedMedian(v), tokens[id]})
	}
	median := weightedMedian(values[node])
	baseline := weightedMedian(group)
	out := CohortNodeStat{Cohort: cohort, NodeID: node, N: counts[node], Tokens: tokens[node], MedianTokPerSec: median, Score: 1, Neutral: true}
	if baseline > 0 && out.N >= 5 && out.Tokens >= 256 {
		out.Score = math.Max(.5, math.Min(2, median/baseline))
		out.Neutral = false
	}
	out.Degraded = baseline > 0 && out.N >= 10 && median < .35*baseline
	if s.degraded == nil {
		s.degraded = map[string]bool{}
	}
	key := cohort + "\x00" + node
	if out.Degraded && !s.degraded[key] {
		log.Printf("SPEED DEGRADED node=%s cohort=%s score=%g", node, cohort, out.Score)
	}
	s.degraded[key] = out.Degraded
	return out
}
func (s *SpeedStats) Score(nodeID, cohort string, now time.Time) (float64, bool, bool, int) {
	if s == nil {
		return 1, true, false, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(now)
	v := s.stat(nodeID, cohort, now)
	return v.Score, v.Neutral, v.Degraded, v.N
}
func (s *SpeedStats) Snapshot(now time.Time) []CohortNodeStat {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(now)
	keys := map[[2]string]bool{}
	for _, v := range s.samples {
		keys[[2]string{v.cohort, v.node}] = true
	}
	out := []CohortNodeStat{}
	for k := range keys {
		out = append(out, s.stat(k[1], k[0], now))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cohort != out[j].Cohort {
			return out[i].Cohort < out[j].Cohort
		}
		return out[i].NodeID < out[j].NodeID
	})
	return out
}
