package scheduler

import (
	"testing"

	pb "ember/proto"
)

func moeCandidate(id string, kind string, start, end uint32, units ...[2]uint32) Candidate {
	p := &pb.ResourceProfile{NodeId: id, ChainId: "moe", ExecutionProfileVer: "v1",
		ManifestRoot: []byte{1}, MoeUnitKind: kind, ShardStartLayer: start, ShardEndLayer: end,
		Signer: []byte(id)}
	for _, u := range units {
		p.MoeUnits = append(p.MoeUnits, &pb.MoeUnitRegistration{Layer: u[0], Expert: u[1]})
	}
	return Candidate{Profile: p, Online: true, Stake: 1000, Reputation: 0.5}
}

func TestPlanMoeCoverageGate(t *testing.T) {
	layout := &pb.MoeLayout{NumLayers: 2, ExpertsPerLayer: 2, TopK: 1}
	r := Request{Chain: "moe", Profile: "v1", Manifest: []byte{1}}
	all := [][2]uint32{{0, 0}, {0, 1}, {1, 0}, {1, 1}}
	cands := []Candidate{
		moeCandidate("attn", "attention", 0, 2),
		moeCandidate("e1", "expert", 0, 0, all...),
		moeCandidate("e2", "expert", 0, 0, all...),
	}
	plan, err := PlanMoe(r, cands, layout)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Stages) != 1 || len(plan.Units) != 4 {
		t.Fatalf("plan shape wrong: %+v", plan)
	}
	for _, unit := range plan.Units {
		if len(unit.Nodes) != 2 || plan.Holders[[2]uint32{unit.Layer, unit.Expert}] != 2 {
			t.Fatal("holder accounting wrong")
		}
	}
	// Drop one holder: every unit falls to one holder -> gate must fire.
	_, err = PlanMoe(r, cands[:2], layout)
	if err == nil {
		t.Fatal("coverage gate did not fire")
	}
	// No attention tiling -> error.
	_, err = PlanMoe(r, cands[1:], layout)
	if err == nil {
		t.Fatal("missing attention tiling accepted")
	}
}
