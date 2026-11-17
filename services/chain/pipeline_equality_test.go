package chain

import (
	core "ember/core-go"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestPipelineRegisteredPool(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		s, a := liveFixture(t)
		s.Auditors = map[string]bool{}
		s.Parameters.AuditEquality = AuditEqualityParameters{Enabled: enabled, Epsilon: 1, MaxRounds: 3}
		p := s.ActiveProfile(a["node"].id, 0)
		p.ActualDevice = "cuda"
		core.Sign(p, a["node"].key)
		raw, _ := proto.Marshal(p)
		s.Nodes[a["node"].id].Profile = raw
		// All registered participants except one auditor are stage owners.
		auth := []PipelineAuthorization{}
		for id := range s.Nodes {
			if id != a["auditor"].id && id != a["node"].id {
				auth = append(auth, PipelineAuthorization{NodeID: id})
			}
		}
		auth = append([]PipelineAuthorization{{NodeID: a["node"].id, Profile: raw}}, auth...)
		h := &Hold{ID: "pool", RequestID: RequestID("payer", 1, []byte("pool")), FreezeHeight: 1, Node: a["node"].id, Pipeline: &PipelineHold{Authorizations: auth}}
		want := s.EligibleAuditorProfile(a["auditor"].id, a["node"].id, p, s.Height)
		id, _, err := s.selectAuditor(h, "")
		if want != enabled || (err == nil) != want || (want && id != a["auditor"].id) {
			t.Fatalf("enabled=%v want=%v id=%s err=%v", enabled, want, id, err)
		}
		if enabled {
			h.Assignments = nil
			if _, _, err = s.selectPipelineAuditor(h, a["auditor"].id); err == nil {
				t.Fatal("exclude bypass")
			}
			// Finalized qualification must win over a changed live switch.
			h.Assignments = nil
			s.finalized, _ = s.Clone()
			s.Parameters.AuditEquality.Enabled = false
			if _, _, err = s.selectPipelineAuditor(h, ""); err != nil {
				t.Fatal("snapshot", err)
			}
			h.Assignments = nil
			h.Payer = a["auditor"].id
			s.Parameters.Scheduling = &core.SchedulingPolicy{ConsumerCrossCheck: &core.ConsumerCrossCheckPolicy{}}
			if _, _, err = s.selectPipelineAuditor(h, ""); err == nil {
				t.Fatal("payer exclusion bypass")
			}

		}
		t.Logf("equality=%v CPU auditor vs CUDA stage eligible=%v; stage owners excluded", enabled, want)
	}
	s, a := liveFixture(t)
	s.Parameters.RegisteredAuditors = false
	h := &Hold{ID: "legacy", RequestID: RequestID("payer", 1, []byte("legacy")), FreezeHeight: 1, Pipeline: &PipelineHold{Authorizations: []PipelineAuthorization{{NodeID: a["node"].id}}}}
	s.Auditors = map[string]bool{a["node"].id: true, a["auditor"].id: false, a["auditor2"].id: true}
	id, _, err := s.selectPipelineAuditor(h, "")
	if err != nil || id != a["auditor2"].id {
		t.Fatal(id, err)
	}
	s.Auditors = map[string]bool{}
	if _, _, err = s.selectPipelineAuditor(h, ""); err == nil {
		t.Fatal("legacy empty pool")
	}
	t.Log("legacy enabled-only list and empty list: 2/2")
}
