package chain

import (
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
	"math"
	"testing"
)

func TestPipelineGradeValidation(t *testing.T) {
	s, _, _, _, _ := pipelineState(t)
	h := &Hold{Pipeline: &PipelineHold{}}
	cases := []struct {
		g  *pb.OffenseGrade
		ok bool
	}{
		{nil, false}, {&pb.OffenseGrade{Class: "numerical", Band: "pipeline-replay-v1", Score: .45}, true},
		{&pb.OffenseGrade{Class: "numerical", Band: "wrong"}, false},
		{&pb.OffenseGrade{Class: "numerical", Band: "pipeline-replay-v1", Fault: "hash"}, false},
		{&pb.OffenseGrade{Class: "numerical", Band: "pipeline-replay-v1", Score: -1}, false},
		{&pb.OffenseGrade{Class: "numerical", Band: "pipeline-replay-v1", Score: math.Inf(1)}, false},
		{&pb.OffenseGrade{Class: "numerical", Band: "pipeline-replay-v1", Score: math.NaN()}, false},
	}
	for i, c := range cases {
		raw, _ := proto.Marshal(&pb.AuditVerdict{Offense: c.g})
		_, err := s.validateGrade(h, raw)
		if (err == nil) != c.ok {
			t.Fatalf("case %d: %v", i, err)
		}
	}
	t.Log("pipeline grading validation 7/7 including missing-grade fail-closed")
}
