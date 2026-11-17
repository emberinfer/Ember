package chain

import (
	"bytes"
	"testing"

	core "ember/core-go"
	pb "ember/proto"
)

// Profile-level misjudge governance for MoE holds (2026-11-15 decision):
// granular MoE profiles slash numerical segment offenses BY DEFAULT —
// Plan B's honest-segment compensation fund is built from slash
// proceeds, and under the whole-model defaults (gray_gate, or the
// genesis refund_only mode) numerical faults used to be refund-only,
// leaving the fund short (measured 2,882,644 subunits in the MOE-8
// drill, which had to force the global mode by hand). The global mode
// and non-MoE behavior are untouched; the global "off" kill switch
// still wins over the profile default.

// moePlanBRejectRun drives the two-round reject flow with a
// caller-chosen offense grade and returns the settled hold plus the
// expected accounting pieces, recomputed from the frozen dues exactly
// as in TestMoeChainPlanBReject.
func moePlanBRejectRun(t *testing.T, f *moeChainFixture, grade *pb.OffenseGrade) (*Hold, int64, string, map[string]int64) {
	t.Helper()
	f.freezeSubmit(t)
	h := f.s.Holds["moe-req"]
	drawn, err := h.moeExpectedDraw()
	if err != nil {
		t.Fatal(err)
	}
	// Pick the drawn segment with the smallest due as the cheater, so
	// the 20x slash stays below the stake cap and the fund math is exact.
	var fault *pb.SegmentRef
	var faultDueRaw int64 = -1
	for _, ref := range drawn {
		for _, due := range h.Moe.SegmentDues {
			if due.Kind == ref.Kind && due.Frame == ref.Frame && due.Layer == ref.Layer &&
				(ref.Kind == "attention" || due.Expert == ref.Expert) {
				if faultDueRaw < 0 || due.Amount < faultDueRaw {
					faultDueRaw = due.Amount
					fault = ref
				}
			}
		}
	}
	if fault == nil {
		t.Fatal("no drawn segment has a due")
	}
	auditor1 := f.auditorActor(t)
	f.audit(t, auditor1, 1, 5, "reject", fault, grade)
	h = f.s.Holds["moe-req"]
	if h.AuditVerdict != "" || h.AuditRound != 2 {
		t.Fatalf("round 1 reject did not escalate: verdict %q round %d", h.AuditVerdict, h.AuditRound)
	}
	auditor2 := f.auditorActor(t)
	if auditor2.id == auditor1.id {
		t.Fatal("round 2 must use a different auditor")
	}
	f.audit(t, auditor2, 1, 6, "reject", fault, grade)
	h = f.s.Holds["moe-req"]
	if h.AuditVerdict != "reject" {
		t.Fatalf("verdict = %q, want reject", h.AuditVerdict)
	}
	expected, err := ExpectedAuditReceipts(h, "reject")
	if err != nil {
		t.Fatal(err)
	}
	pool := int64(480_000)
	dueShares := make([]core.WorkShare, 0, len(h.Moe.SegmentDues))
	for _, due := range h.Moe.SegmentDues {
		dueShares = append(dueShares, core.WorkShare{NodeID: due.Key, Work: uint64(due.Amount)})
	}
	poolDues, err := core.SplitWorkCharge(pool, dueShares)
	if err != nil {
		t.Fatal(err)
	}
	poolByKey := map[string]int64{}
	for _, share := range poolDues {
		poolByKey[share.NodeID] = share.Amount
	}
	var faultDue int64
	var faultNode string
	honestByNode := map[string]int64{}
	for _, due := range h.Moe.SegmentDues {
		if due.Kind == fault.Kind && due.Frame == fault.Frame && due.Layer == fault.Layer &&
			(fault.Kind == "attention" || due.Expert == fault.Expert) {
			faultDue += poolByKey[due.Key]
			faultNode = due.NodeID
		} else {
			honestByNode[due.NodeID] += poolByKey[due.Key]
		}
	}
	apply(t, f.s, f.a["coordinator"], 3, Action{Type: "settle", HoldID: "moe-req",
		Outcome: "reject", AuditReceipts: expected}, 7)
	return f.s.Holds["moe-req"], faultDue, faultNode, honestByNode
}

func moeNumericalGrade() *pb.OffenseGrade {
	return &pb.OffenseGrade{Class: "numerical", Score: 5.0, Band: "moe-segment-v1"}
}

func assertPlanBSlashed(t *testing.T, f *moeChainFixture, h *Hold, faultDue int64, faultNode string, honestByNode map[string]int64) {
	t.Helper()
	if h.State != "slashed" || h.Moe.PlanB == nil {
		t.Fatalf("state = %s planB = %+v", h.State, h.Moe.PlanB)
	}
	plan := h.Moe.PlanB
	if plan.Confiscated != faultDue || plan.HonestPaid != plan.HonestDue || plan.Shortfall != 0 {
		t.Fatalf("plan B accounting wrong: %+v (faultDue %d)", plan, faultDue)
	}
	if plan.SlashFund != 20*faultDue {
		t.Fatalf("slash fund = %d, want %d", plan.SlashFund, 20*faultDue)
	}
	if got := f.s.Accounts[faultNode].Stake; got != 5_000_000-20*faultDue {
		t.Fatalf("fault node stake = %d, want %d", got, 5_000_000-20*faultDue)
	}
	for node, want := range honestByNode {
		if got := f.s.Accounts[node].Balance; got != want {
			t.Fatalf("honest node %s balance = %d, want %d", node, got, want)
		}
	}
}

// The headline pin: a default granular MoE profile (layout field unset,
// no manual configuration anywhere) slashes a numerical segment cheater
// under the untouched global default mode, and Plan B pays the honest
// segments in full.
func TestMoeGranularNumericalSlashByDefault(t *testing.T) {
	f := moeChainState(t)
	if got := f.s.Parameters.Misjudge.SlashMode; got != "gray_gate" {
		t.Fatalf("precondition: global mode = %q, want gray_gate default", got)
	}
	h, faultDue, faultNode, honestByNode := moePlanBRejectRun(t, f, moeNumericalGrade())
	assertPlanBSlashed(t, f, h, faultDue, faultNode, honestByNode)
}

// The profile default also holds under the genesis refund_only mode:
// that mode governs whole-model numerical disputes, not MoE segment
// offenses on granular profiles.
func TestMoeGranularNumericalSlashUnderRefundOnlyGlobal(t *testing.T) {
	f := moeChainState(t)
	f.s.Parameters.Misjudge.SlashMode = "refund_only"
	h, faultDue, faultNode, honestByNode := moePlanBRejectRun(t, f, moeNumericalGrade())
	assertPlanBSlashed(t, f, h, faultDue, faultNode, honestByNode)
}

// An explicit profile opt-out restores refund-only for that profile:
// no slash proceeds, and Plan B's fund falls back to confiscated dues
// plus the unissued rebate — which leaves a shortfall. This is the
// documented cost of opting out.
func TestMoeMisjudgeSlashOffOptOut(t *testing.T) {
	f := moeChainStateLayout(t, func(l *pb.MoeLayout) { l.MisjudgeSlashMode = "off" })
	h, _, faultNode, _ := moePlanBRejectRun(t, f, moeNumericalGrade())
	if h.State != "cancelled" || h.Moe.PlanB == nil {
		t.Fatalf("state = %s planB = %+v", h.State, h.Moe.PlanB)
	}
	plan := h.Moe.PlanB
	if plan.SlashFund != 0 {
		t.Fatalf("slash fund = %d, want 0 under profile opt-out", plan.SlashFund)
	}
	if plan.Shortfall <= 0 || plan.HonestPaid >= plan.HonestDue {
		t.Fatalf("expected a Plan B shortfall under opt-out: %+v", plan)
	}
	if got := f.s.Accounts[faultNode].Stake; got != 5_000_000 {
		t.Fatalf("fault node stake = %d, want untouched 5000000", got)
	}
	found := false
	for _, r := range f.s.OffenseRecords {
		if r.NodeID == faultNode && r.Status == "refund-only" {
			found = true
		}
	}
	if !found {
		t.Fatal("no refund-only offense record for the fault node")
	}
}

// The global "off" kill switch remains supreme over the profile
// default: with misjudge slashing disabled network-wide, even a
// granular MoE profile does not slash.
func TestMoeMisjudgeGlobalOffWins(t *testing.T) {
	f := moeChainState(t)
	f.s.Parameters.Misjudge.SlashMode = "off"
	h, _, _, _ := moePlanBRejectRun(t, f, moeNumericalGrade())
	if h.State != "cancelled" || h.Moe.PlanB == nil || h.Moe.PlanB.SlashFund != 0 {
		t.Fatalf("state = %s planB = %+v", h.State, h.Moe.PlanB)
	}
}

// Atomic settlement profiles keep the pre-existing behavior: with the
// layout field unset they inherit the global mode (refund-only under
// gray_gate), the payer is refunded in full and no stake moves.
func TestMoeAtomicNumericalInheritsGlobal(t *testing.T) {
	f := moeChainStateLayout(t, func(l *pb.MoeLayout) { l.SegmentSettlement = "atomic" })
	h, _, faultNode, _ := moePlanBRejectRun(t, f, moeNumericalGrade())
	if h.State != "cancelled" {
		t.Fatalf("state = %s, want cancelled", h.State)
	}
	if h.Moe.PlanB != nil {
		t.Fatalf("atomic settlement recorded a Plan B: %+v", h.Moe.PlanB)
	}
	if got := f.s.Accounts[f.a["payer"].id].Balance; got != 10_000_000 {
		t.Fatalf("payer balance = %d, want full refund 10000000", got)
	}
	if got := f.s.Accounts[faultNode].Stake; got != 5_000_000 {
		t.Fatalf("fault node stake = %d, want untouched 5000000", got)
	}
}

func TestMoeLayoutMisjudgeSlashModeValidation(t *testing.T) {
	manifest := &pb.ModelManifest{TotalLayers: 2, MoeLayout: &pb.MoeLayout{
		NumLayers: 2, ExpertsPerLayer: 4, TopK: 2, HiddenSize: 8,
		ExpertParamCount: 50, AttentionParamCount: 100, VocabSize: 16,
		SegmentDtype: "bf16", RoutingCapBeta: 4, ScarcityMax: 8,
		MaxFramePositions: 4, ExpertWeightsRoot: bytes.Repeat([]byte{1}, 32)}}
	for _, mode := range []string{"", "on", "off"} {
		manifest.MoeLayout.MisjudgeSlashMode = mode
		if err := core.ValidateMoeLayout(manifest); err != nil {
			t.Fatalf("mode %q rejected: %v", mode, err)
		}
	}
	manifest.MoeLayout.MisjudgeSlashMode = "bogus"
	if err := core.ValidateMoeLayout(manifest); err == nil {
		t.Fatal("bogus misjudge slash mode accepted")
	}
}
