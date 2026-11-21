package chain

import (
	"bytes"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
	"reflect"
	"testing"
)

func TestBoundedSpotDrawV15(t *testing.T) {
	if SPOT_ESCALATION_BLOCKS != 600 {
		t.Fatal("budget")
	}
	first := &pb.AuditVerdict{PerStep: []*pb.SpotStepResult{{Step: 3, Outcome: "mismatch"}, {Step: 7, Outcome: "match"}}}
	for _, n := range []uint64{9, 94} {
		a, err := DeriveBoundedSpotSteps(bytes.Repeat([]byte{2}, 32), n, []uint64{3, 7}, first)
		b, _ := DeriveBoundedSpotSteps(bytes.Repeat([]byte{2}, 32), n, []uint64{3, 7}, first)
		if err != nil || !reflect.DeepEqual(a, b) || len(a) > 16 {
			t.Fatal(a, err)
		}
		found := false
		for _, v := range a {
			if v == 7 {
				t.Fatal("initial match redrawn")
			}
			found = found || v == 3
		}
		want := 16
		if n == 9 {
			want = 8
		}
		if !found || len(a) != want {
			t.Fatal(a)
		}
	}
}

func TestBoundedSpotVerdictsV15(t *testing.T) {
	for _, corroborated := range []bool{false, true} {
		for _, additional := range []bool{false, true} {
			s, _, _, h := spotFixture(t)
			s.Version = 15
			h.VerdictBytes = spotVerdict(h, true)
			if err := s.validateSpotVerdict(h, h.VerdictBytes, 5); err != nil {
				t.Fatal(err)
			}
			first := h.Auditor
			if err := s.spotOutcome(h, first, "reject", "first", 5); err != nil {
				t.Fatal(err)
			}
			c := h.Pipeline.Spot
			if c.Deadline != 605 || len(c.StepSet) > 16 {
				t.Fatal(c)
			}
			fv := &pb.AuditVerdict{}
			proto.Unmarshal(c.FirstVerdict, fv)
			disputed := fv.PerStep[0].Step
			initial := map[uint64]bool{}
			for _, p := range fv.PerStep {
				initial[p.Step] = true
			}
			v := &pb.AuditVerdict{Verdict: "accept", VerdictKind: "escalation_bounded_accept"}
			used := false
			for _, step := range c.StepSet {
				p := &pb.SpotStepResult{Step: step, Outcome: "match"}
				if (step == disputed && corroborated) || (!initial[step] && additional && !used) {
					p.Outcome = "mismatch"
					p.FirstDivergenceDistance = 1
					v.VerdictDistance = 1
					if !initial[step] {
						used = true
					}
				}
				v.PerStep = append(v.PerStep, p)
			}
			convicted := corroborated && additional
			if convicted {
				v.Verdict = "reject"
				v.VerdictKind = "escalation_bounded_reject"
			}
			raw, _ := proto.Marshal(v)
			if err := s.validateSpotVerdict(h, raw, 6); err != nil {
				t.Fatal(err)
			}
			oldKind := v.VerdictKind
			v.VerdictKind = "escalation_full_accept"
			wrong, _ := proto.Marshal(v)
			if s.validateSpotVerdict(h, wrong, 6) == nil {
				t.Fatal("v15 accepted historical kind")
			}
			v.VerdictKind = oldKind
			h.VerdictBytes = raw
			if err := s.spotOutcome(h, h.Auditor, v.Verdict, "bounded", 6); err != nil {
				t.Fatal(err)
			}
			if confirmedSpotConviction(h) != convicted {
				t.Fatal("conviction")
			}
			if !convicted && (h.AuditVerdict != "accept" || c.GoodFaithAuditor != first || s.Availability[first].Major != 0) {
				t.Fatal("good faith")
			}
			// v14 still expects full coverage and the original kind strings.
			s.Version = 14
			c.StepSet = []uint64{1}
			c.Received = true
			legacy := &pb.AuditVerdict{Verdict: "accept", VerdictKind: "escalation_full_accept"}
			for step := uint64(1); step <= h.Pipeline.SpotStepCount; step++ {
				legacy.PerStep = append(legacy.PerStep, &pb.SpotStepResult{Step: step, Outcome: "match"})
			}
			raw, _ = proto.Marshal(legacy)
			if err := s.validateSpotVerdict(h, raw, 6); err != nil {
				t.Fatal(err)
			}
			legacy.VerdictKind = "escalation_bounded_accept"
			raw, _ = proto.Marshal(legacy)
			if s.validateSpotVerdict(h, raw, 6) == nil {
				t.Fatal("v14 accepted bounded kind")
			}
		}
	}
}

func TestBoundedSpotRetainsAllDisputesV15(t *testing.T) {
	first := &pb.AuditVerdict{}
	initial := []uint64{}
	for step := uint64(1); step <= 15; step++ {
		initial = append(initial, step)
		first.PerStep = append(first.PerStep, &pb.SpotStepResult{Step: step, Outcome: "mismatch"})
	}
	steps, err := DeriveBoundedSpotSteps(bytes.Repeat([]byte{1}, 32), 94, initial, first)
	if err != nil || len(steps) != 30 {
		t.Fatal(steps, err)
	}
	for i := 0; i < 15; i++ {
		if steps[i] != uint64(i+1) {
			t.Fatal("lost dispute")
		}
	}
	other, _ := DeriveBoundedSpotSteps(bytes.Repeat([]byte{3}, 32), 94, initial, first)
	if reflect.DeepEqual(steps, other) {
		t.Fatal("entropy ignored")
	}
}
