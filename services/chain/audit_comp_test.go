package chain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func auditCompFixture(t *testing.T) *AuditCompParameters {
	t.Helper()
	raw, err := os.ReadFile("testdata/audit_replay_gb10.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Policy AuditCompParameters `json:"audit_comp"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	f.Policy.BenchmarkRef = hex.EncodeToString(sum[:])
	return &f.Policy
}
func TestAuditCompBoundaries(t *testing.T) {
	p := auditCompFixture(t)
	zero, err := AuditCompAmount(p, 123, 0)
	if err != nil {
		t.Fatal(err)
	}
	expected := (p.OFixed*1000 + p.RIn*123) * int64(p.MarginBPS) / 10000000
	if zero != expected {
		t.Fatal(zero, expected)
	}
	h := &Hold{RequestID: "request", AuditCompPolicy: p, Actual: 1, AuditOutputTokens: 32, AuditWorks: []AuditWork{{Auditor: "auditor", Input: 123}}}
	v := &pb.AuditVerdict{AuditorId: "auditor", NOutBilled: 8}
	raw, _ := proto.Marshal(v)
	c, _ := proto.Marshal(&pb.AuditChallenge{Positions: []uint32{0, 7}})
	if err := h.citeAudit(raw, c); err != nil {
		t.Fatal(err)
	}
	receipts, err := ExpectedAuditReceipts(h, "accept")
	if err != nil {
		t.Fatal(err)
	}
	if receipts[0].AuditComp.NOutBilled != 8 || receipts[0].AuditComp.CompSubunits != 0 || receipts[0].AuditShortfallSubunits == 0 {
		t.Fatal(receipts)
	}
	// Extra local replay is not a billing input; only signed citation counts.
	before, _ := proto.Marshal(receipts[0])
	h.AuditOutputTokens = 100
	after, _ := ExpectedAuditReceipts(h, "accept")
	afterRaw, _ := proto.Marshal(after[0])
	if !equal(before, afterRaw) {
		t.Fatal("uncited replay billed")
	}
	t.Logf("prefix=0 amount=%d; cited=8 output available=100 paid=0 shortfall=%d", zero, receipts[0].AuditShortfallSubunits)
}
func TestAuditCompConsensusRejectAndFrozenGovernance(t *testing.T) {
	s, a := pricingState(t)
	s.Accounts[a["node"].id].Stake = 5000
	p := auditCompFixture(t)
	rejected(t, s, a["payer"], Action{Type: "set_audit_comp", ModelID: "ember-test-model", AuditComp: p}, 10)
	apply(t, s, a["governor"], 1, Action{Type: "set_audit_comp", ModelID: "ember-test-model", AuditComp: p}, 10)
	// A real first-data freeze pins the policy, even after a governor update.
	apply(t, s, a["payer"], 1, Action{Type: "freeze", HoldID: "comp", Payer: a["payer"].id, Node: a["node"].id, Amount: 500}, 11)
	h := s.Holds["comp"]
	old := h.AuditCompPolicy.OFixed
	next := *p
	next.OFixed++
	apply(t, s, a["governor"], 2, Action{Type: "set_audit_comp", ModelID: "ember-test-model", AuditComp: &next}, 12)
	if s.Holds["comp"].AuditCompPolicy.OFixed != old {
		t.Fatal("freeze drift")
	}
	h = s.Holds["comp"]
	h.State = "dispute"
	h.Actual = 500
	h.AuditVerdict = "accept"
	h.SettleHeight = 12
	h.Auditor = a["auditor"].id
	h.AuditWorks = []AuditWork{{Auditor: h.Auditor, Input: 8, Prefix: 1, Replayed: true}}
	receipts, err := ExpectedAuditReceipts(h, "accept")
	if err != nil {
		t.Fatal(err)
	}
	bad := proto.Clone(receipts[0]).(*pb.SettlementReceipt)
	bad.AuditComp.CompSubunits++
	before, _ := s.Hash()
	rejected(t, s, a["coordinator"], Action{Type: "settle", HoldID: "comp", Outcome: "accept", AuditReceipts: []*pb.SettlementReceipt{bad}}, 13)
	after, _ := s.Hash()
	if !equal(before, after) {
		t.Fatal("rejected receipt mutated state")
	}
	apply(t, s, a["coordinator"], 1, Action{Type: "settle", HoldID: "comp", Outcome: "accept", AuditReceipts: receipts}, 13)
	h = s.Holds["comp"]
	if h.State != "settled" || h.AuditReceipts[0].AuditComp.CompSubunits != 75 || h.AuditEscrow != 0 {
		t.Fatal(h)
	}
	rejected(t, s, a["coordinator"], Action{Type: "settle", HoldID: "comp", Outcome: "accept", AuditReceipts: receipts}, 14)
	t.Logf("wrong comp rejected with unchanged state; paid=%d shortfall=%d one settlement", receipts[0].AuditComp.CompSubunits, receipts[0].AuditShortfallSubunits)
}
func TestAuditWorkCapEpochAndAffordability(t *testing.T) {
	s, a := testState(t)
	p := auditCompFixture(t)
	h := &Hold{Node: a["node"].id, AuditCompPolicy: p, AuditWorkPolicy: AuditWorkParameters{EpochBlocks: 10, MaxPerExecutor: 2}, Actual: 100000, AuditOutputTokens: 1}
	for i := 0; i < 2; i++ {
		if err := s.reserveAuditWork(h, []string{h.Node}, a["auditor"].id, 9); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.reserveAuditWork(h, []string{h.Node}, a["auditor"].id, 9); err == nil {
		t.Fatal("epoch cap bypass")
	}
	if err := s.reserveAuditWork(h, []string{h.Node}, a["auditor"].id, 10); err != nil {
		t.Fatal(err)
	}
	h.Actual = 1
	if err := s.reserveAuditWork(h, []string{h.Node}, a["auditor"].id, 10); err == nil {
		t.Fatal("underfunded work admitted")
	}
	if len(h.AuditWorks) != 3 {
		t.Fatal("failed admission charged work")
	}
	t.Log("epoch 0 admits 2/3; epoch 1 resets; fee=1 underfunded request rejected")
}
func TestAuditCompSignedCitation(t *testing.T) {
	_, a := testState(t)
	p := auditCompFixture(t)
	h := &Hold{AuditCompPolicy: p, AuditOutputTokens: 8, AuditWorks: []AuditWork{{Auditor: a["auditor"].id}}}
	v := &pb.AuditVerdict{AuditorId: a["auditor"].id, NOutBilled: 9}
	core.Sign(v, a["auditor"].key)
	raw, _ := proto.Marshal(v)
	c, _ := proto.Marshal(&pb.AuditChallenge{Positions: []uint32{7}})
	if h.citeAudit(raw, c) == nil {
		t.Fatal("overbilling accepted")
	}
	v.NOutBilled = 7
	raw, _ = proto.Marshal(v)
	if h.citeAudit(raw, c) == nil {
		t.Fatal("citation mismatch accepted")
	}
}

func TestAuditCompEndToEndAssignmentCitationAndEscrow(t *testing.T) {
	s, a := pricingState(t)
	s.Accounts[a["payer"].id].Balance = 100000
	s.Accounts[a["node"].id].Stake = 1000000
	p := auditCompFixture(t)
	apply(t, s, a["governor"], 1, Action{Type: "set_audit_comp", ModelID: "ember-test-model", AuditComp: p}, 10)
	apply(t, s, a["payer"], 1, Action{Type: "freeze", HoldID: "full-comp", Payer: a["payer"].id, Node: a["node"].id, Amount: 10000}, 11)
	raw := signedReceipt(t, "full-comp", a["node"])
	apply(t, s, a["coordinator"], 1, Action{Type: "submit", HoldID: "full-comp", Actual: 10000, Receipt: raw}, 12)
	apply(t, s, a["coordinator"], 2, Action{Type: "assign_audit", HoldID: "full-comp"}, 14)
	auditor, proof := assignedActor(t, s, a, "full-comp")
	challenge, verdict, _ := signedAudit(t, raw, auditor, "accept", proof)
	c := &pb.AuditChallenge{}
	proto.Unmarshal(challenge, c)
	c.Positions = []uint32{0}
	core.Sign(c, auditor.key)
	challenge, _ = proto.Marshal(c)
	v := &pb.AuditVerdict{}
	proto.Unmarshal(verdict, v)
	v.ChallengeRef, _ = core.Digest(c)
	v.NOutBilled = 1
	core.Sign(v, auditor.key)
	verdict, _ = proto.Marshal(v)
	apply(t, s, auditor, s.Accounts[auditor.id].Nonce+1, Action{Type: "audit", HoldID: "full-comp", Challenge: challenge, Verdict: verdict}, 15)
	h := s.Holds["full-comp"]
	receipts, err := ExpectedAuditReceipts(h, "accept")
	if err != nil {
		t.Fatal(err)
	}
	apply(t, s, a["coordinator"], s.Accounts[a["coordinator"].id].Nonce+1, Action{Type: "settle", HoldID: h.ID, Outcome: "accept", AuditReceipts: receipts}, 16)
	h = s.Holds[h.ID]
	if len(h.AuditWorks) != 1 || !h.AuditWorks[0].Replayed || h.AuditEscrow+receipts[0].AuditComp.CompSubunits != 1500 || h.Shares.Executor != 8000 || h.Shares.Consumer != 500 {
		t.Fatal(h)
	}
	t.Logf("assigned=1 replayed=1 fee=10000 executor=8000 consumer=500 audit paid=%d retained audit escrow=%d", receipts[0].AuditComp.CompSubunits, h.AuditEscrow)
}

// This optional staging gate reports measured economics; accounting correctness
// and economic acceptance are separate. Failed acceptance is never hidden by
// changing the measured fixture, conversion or workload. Fee scenarios are
// supplied explicitly by the caller; this test never tunes them.
func TestAuditCompMeasuredTwoEpochLoad(t *testing.T) {
	path := os.Getenv("TASK21_AUDIT_LOAD")
	if path == "" {
		t.Skip("set TASK21_AUDIT_LOAD to measured observations")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var load struct {
		Rows []struct {
			Workload string `json:"workload"`
			Epoch    int64  `json:"epoch"`
			Auditor  string `json:"auditor"`
			ID       string `json:"request_id"`
			Input    uint32 `json:"n_in"`
			Prefix   uint32 `json:"n_out_billed"`
			NS       int64  `json:"elapsed_ns"`
		}
		FeeProfile struct {
			Genesis json.RawMessage `json:"genesis"`
		} `json:"fee_profile"`
	}
	if err := json.Unmarshal(raw, &load); err != nil {
		t.Fatal(err)
	}
	g := &pb.ChainGenesis{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(load.FeeProfile.Genesis, g); err != nil {
		t.Fatal(err)
	}
	p := auditCompFixture(t)
	// Fixed accounting conversion from the measured benchmark fixture.
	// Optional local derivation scenario; production defaults stay unchanged.
	if path := os.Getenv("TASK21_AUDIT_COMP_DERIVATION"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var derivation struct {
			Policy AuditCompParameters `json:"audit_comp"`
		}
		if err := json.Unmarshal(raw, &derivation); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		derivation.Policy.BenchmarkRef = hex.EncodeToString(sum[:])
		p = &derivation.Policy
		if !p.valid() {
			t.Fatal("invalid derived scenario policy")
		}
	}
	var benchmark struct {
		Rate int64 `json:"subunits_per_second"`
	}
	b, _ := os.ReadFile("testdata/audit_replay_gb10.json")
	json.Unmarshal(b, &benchmark)
	state := map[string]any{"report_context": "Counterfactual share-capped settlement of all independently measured replay, including denied work. These are not chain earnings.", "holds": map[string]*Hold{}, "height": 1999, "parameters": map[string]any{"audit_work": AuditWorkParameters{EpochBlocks: 1000, MaxPerExecutor: 64}}}
	holds := state["holds"].(map[string]*Hold)
	admittedHolds := map[string]*Hold{}
	admissionStates := map[string]*State{}
	costs := []map[string]any{}
	type result struct {
		Admitted     int
		Replayed     int
		Comp         int64
		Cost         int64
		Shortfall    int64
		Auditors     map[string]int64
		AdmittedComp int64
		AdmittedCost int64
	}
	results := map[string]*result{}
	for _, r := range load.Rows {
		if r.NS < 0 {
			t.Fatal("negative measured cost")
		}
		fee, err := core.TokenFee(g, r.Input, r.Prefix)
		if err != nil {
			t.Fatal(err)
		}
		h := &Hold{ID: r.ID, RequestID: r.ID, Node: "executor", Actual: fee.Total, AuditCompPolicy: p, AuditWorkPolicy: AuditWorkParameters{EpochBlocks: 1000, MaxPerExecutor: 64}, AuditInputTokens: r.Input, AuditOutputTokens: r.Prefix}
		s := admissionStates[r.Workload]
		if s == nil {
			s = &State{}
			admissionStates[r.Workload] = s
		}
		admitted := s.reserveAuditWork(h, []string{h.Node}, r.Auditor, r.Epoch*1000) == nil
		// The independent measurement already replayed even denied work. Report its
		// counterfactual share-capped return separately from actual admission.
		h.AuditWorks = []AuditWork{{Auditor: r.Auditor, Epoch: r.Epoch, Input: r.Input, Prefix: r.Prefix, Replayed: true}}
		receipts, err := ExpectedAuditReceipts(h, "accept")
		if err != nil {
			t.Fatal(err)
		}
		if err := verifyAuditReceipts(h, "accept", receipts); err != nil {
			t.Fatal(err)
		}
		holds[r.ID] = h
		cost := (r.NS*benchmark.Rate + 999999999) / 1000000000
		costs = append(costs, map[string]any{"hold_id": r.ID, "auditor": r.Auditor, "cost_subunits": cost})
		rr := results[r.Workload]
		if rr == nil {
			rr = &result{Auditors: map[string]int64{}}
			results[r.Workload] = rr
		}
		if admitted {
			rr.Admitted++
			rr.AdmittedComp += receipts[0].AuditComp.CompSubunits
			rr.AdmittedCost += cost
			admittedHolds[r.ID] = h
		}
		rr.Replayed++
		rr.Comp += receipts[0].AuditComp.CompSubunits
		rr.Cost += cost
		rr.Shortfall += receipts[0].AuditShortfallSubunits
		rr.Auditors[r.Auditor] += receipts[0].AuditComp.CompSubunits - cost
	}
	for name, r := range results {
		nonnegative := 0
		for _, pnl := range r.Auditors {
			if pnl >= 0 {
				nonnegative++
			}
		}
		t.Logf("%s admitted-only reconstructed comp=%d cost=%d pnl=%d (not live chain earnings)", name, r.AdmittedComp, r.AdmittedCost, r.AdmittedComp-r.AdmittedCost)
		t.Logf("%s epochs=2 measured_replays=%d admitted=%d auditors_nonnegative=%d/%d share_capped_comp=%d measured_cost=%d pnl=%d shortfalls=%d", name, r.Replayed, r.Admitted, nonnegative, len(r.Auditors), r.Comp, r.Cost, r.Comp-r.Cost, r.Shortfall)
	}
	if out := os.Getenv("TASK21_AUDIT_LOAD_OUTPUT"); out != "" {
		admittedState := map[string]any{"report_context": "Admission-only accounting reconstruction with saved measured costs; shared per-workload epoch counters. Not live chain earnings.", "holds": admittedHolds, "height": 1999, "parameters": state["parameters"]}
		for suffix, data := range map[string]any{"-state.json": state, "-costs.json": costs, "-admitted-state.json": admittedState} {
			raw, _ := json.MarshalIndent(data, "", "  ")
			if err := os.WriteFile(out+suffix, raw, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestAuditCompPipelineFundingAndExecutorCaps(t *testing.T) {
	s, a, nodes, auth, _ := pipelineState(t)
	p := auditCompFixture(t)
	h := &Hold{ID: "pipeline-comp", RequestID: "pipeline-comp", ModelID: "ember-unit-test", Payer: a["payer"].id, Amount: 10000, Actual: 10000, AuditCompPolicy: p, AuditWorkPolicy: AuditWorkParameters{EpochBlocks: 1000, MaxPerExecutor: 1}, Pipeline: &PipelineHold{Authorizations: auth, Credits: []core.StageShare{{NodeID: nodes[0].id, Amount: 1667}, {NodeID: nodes[1].id, Amount: 6667}, {NodeID: nodes[2].id, Amount: 1666}}}}
	executors := []string{nodes[0].id, nodes[1].id, nodes[2].id}
	if err := s.reserveAuditWork(h, executors, a["auditor"].id, 3); err != nil {
		t.Fatal(err)
	}
	if err := s.reserveAuditWork(h, []string{nodes[1].id}, a["auditor2"].id, 3); err == nil {
		t.Fatal("pipeline executor cap bypass")
	}
	h.AuditWorks[0].Replayed = true
	receipts, err := ExpectedAuditReceipts(h, "accept")
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyAuditReceipts(h, "accept", receipts); err != nil {
		t.Fatal(err)
	}
	before := s.Accounts[a["payer"].id].Balance
	if err := s.settleAt(h, "accept", 4); err != nil {
		t.Fatal(err)
	}
	executorTotal := int64(0)
	for _, n := range nodes {
		executorTotal += s.Accounts[n.id].Balance
	}
	if executorTotal != 8000 || s.Accounts[a["payer"].id].Balance-before != 500 || h.AuditEscrow+receipts[0].AuditComp.CompSubunits != 1500 {
		t.Fatal(executorTotal, h)
	}
	// Full refund leaves no collected request fee, hence no audit funding.
	rejectedReceipts, err := ExpectedAuditReceipts(h, "reject")
	if err != nil {
		t.Fatal(err)
	}
	if rejectedReceipts[0].AuditComp.CompSubunits != 0 || rejectedReceipts[0].AuditShortfallSubunits == 0 {
		t.Fatal(rejectedReceipts)
	}
	t.Logf("pipeline executor credits sum=8000 consumer=500 audit payment=%d escrow=%d all 3 executor counts=1; refunded request audit payment=0", receipts[0].AuditComp.CompSubunits, h.AuditEscrow)
}
