package chain

import (
	"bytes"
	core "ember/core-go"
	pb "ember/proto"
	"encoding/hex"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestLatencyUsesIndependentCountersignaturesOnly(t *testing.T) {
	s, a, nodes, auth, r := pipelineState(t)
	s.Parameters.Scheduling = &core.SchedulingPolicy{Scoring: "auditor-roundtrip-us-per-unit-v1", WitnessAuthority: "assigned-auditor", ExplorationPPM: 200000, ExplorationPeriod: 10, PercentilePPM: 900000, WindowSamples: 2, MinSamples: 1, MaxObservationNS: 1000000000}
	s.Auditors = map[string]bool{a["auditor"].id: true}
	for _, stage := range r.StageReceipts {
		ref, _ := core.Digest(stage)
		w := &pb.StageLatencyWitness{StageReceiptRef: ref, StartedMonoNs: 1000000, CompletedMonoNs: 1040000}
		core.Sign(w, a["auditor"].key)
		r.LatencyWitnesses = append(r.LatencyWitnesses, w)
	}
	apply(t, s, a["payer"], 1, Action{Type: "freeze_pipeline", HoldID: "units", Payer: a["payer"].id, Amount: 6, TotalLayers: 4, PipelineAuthorizations: auth}, 2)
	forged := proto.Clone(r).(*pb.PipelineExecutionReceipt)
	core.Sign(forged.LatencyWitnesses[0], nodes[0].key)
	raw, _ := proto.Marshal(forged)
	rejected(t, s, a["coordinator"], Action{Type: "submit_pipeline", HoldID: "units", Actual: 6, Receipt: raw}, 3)
	forged = proto.Clone(r).(*pb.PipelineExecutionReceipt)
	forged.LatencyWitnesses[0].StageReceiptRef = bytes.Repeat([]byte{99}, 32)
	core.Sign(forged.LatencyWitnesses[0], a["auditor"].key)
	raw, _ = proto.Marshal(forged)
	rejected(t, s, a["coordinator"], Action{Type: "submit_pipeline", HoldID: "units", Actual: 6, Receipt: raw}, 3)
	// This untrusted aggregate duration must never affect scheduling statistics.
	r.ElapsedMs = 999999999
	raw, _ = proto.Marshal(r)
	apply(t, s, a["coordinator"], 1, Action{Type: "submit_pipeline", HoldID: "units", Actual: 6, Receipt: raw}, 3)
	if len(s.Latencies) != 0 {
		t.Fatal("unsettled observation affected scheduler")
	}
	apply(t, s, a["coordinator"], 2, Action{Type: "assign_audit", HoldID: "units"}, 4)
	h := s.Holds["units"]
	ref, _ := hex.DecodeString(h.ReceiptRef)
	proof, _ := hex.DecodeString(h.AuditProof)
	c := &pb.AuditChallenge{ReceiptRef: ref, VrfProof: proof}
	core.Sign(c, a["auditor"].key)
	cr, _ := core.Digest(c)
	v := &pb.AuditVerdict{ChallengeRef: cr, AuditorId: a["auditor"].id, Verdict: "accept", EvidenceRef: bytes.Repeat([]byte{7}, 32)}
	core.Sign(v, a["auditor"].key)
	cb, _ := proto.Marshal(c)
	vb, _ := proto.Marshal(v)
	apply(t, s, a["auditor"], 1, Action{Type: "audit", HoldID: "units", Challenge: cb, Verdict: vb}, 5)
	apply(t, s, a["coordinator"], 3, Action{Type: "settle", HoldID: "units", Outcome: "accept"}, 6)
	for i, want := range []uint64{40, 10, 40} {
		stats := s.Latencies[LatencyKey(nodes[i].id, "ember-unit-test")]
		if stats == nil || stats.PercentileMicrosPerUnit != want || stats.VerifiedSamples != 1 {
			t.Fatal("statistics accepted non-witness timing", stats)
		}
	}
}
