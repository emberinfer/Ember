package chain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"

	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

// MoE chain drill fixture: L=2, E=4, k=2 toy layout; one attention stage
// [0,2); two serving expert nodes (units split B/C) plus two standby
// holders per unit for the coverage gate.
type moeChainFixture struct {
	s        *State
	a        map[string]actor
	attn     actor
	expB     actor
	expC     actor
	expB2    actor
	expC2    actor
	manifest *pb.ModelManifest
	genesis  *pb.ChainGenesis
	root     []byte
	params   [][]byte // expert params hashes, index layer*4+expert
}

func moeProof(t *testing.T, params [][]byte, index int) [][]byte {
	t.Helper()
	level := make([][]byte, len(params))
	for i := range params {
		level[i] = moeLeafForTest(uint32(i)/4, uint32(i)%4, params[i])
	}
	var proof [][]byte
	idx := index
	for len(level) > 1 {
		if idx%2 == 1 {
			proof = append(proof, level[idx-1])
		} else if idx+1 < len(level) {
			proof = append(proof, level[idx+1])
		}
		next := make([][]byte, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			if i+1 == len(level) {
				next = append(next, level[i])
				continue
			}
			h := sha256.New()
			h.Write([]byte("ember/moe-expert-node/v1\x00"))
			h.Write(level[i])
			h.Write(level[i+1])
			next = append(next, h.Sum(nil))
		}
		level = next
		idx /= 2
	}
	return proof
}

func moeLeafForTest(layer, expert uint32, paramsHash []byte) []byte {
	h := sha256.New()
	h.Write([]byte("ember/moe-expert-leaf/v1\x00"))
	var b [4]byte
	for _, v := range []uint32{layer, expert} {
		b[0], b[1], b[2], b[3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
		h.Write(b[:])
	}
	h.Write(paramsHash)
	return h.Sum(nil)
}

func moeChainState(t *testing.T) *moeChainFixture {
	return moeChainStateLayout(t, nil)
}

func moeChainStateLayout(t *testing.T, mutate func(*pb.MoeLayout)) *moeChainFixture {
	t.Helper()
	s, a := testState(t)
	s.Parameters.MoeEnabled = true
	s.Parameters.Misjudge.CapBlockBPS = 10000
	s.Parameters.AuditWork = AuditWorkParameters{EpochBlocks: 10, MaxPerExecutor: 1000}
	f := &moeChainFixture{s: s, a: a,
		attn: newActor(30), expB: newActor(31), expC: newActor(32),
		expB2: newActor(33), expC2: newActor(34)}
	layout := &pb.MoeLayout{
		NumLayers: 2, HiddenSize: 8, ExpertsPerLayer: 4, TopK: 2,
		ExpertParamCount: 50, AttentionParamCount: 100, VocabSize: 16,
		SegmentDtype: "bf16", EngineImplId: "moe-test-v1", RoutingCapBeta: 4,
		ScarcityMax: 8, SegmentSettlement: "granular", MaxFramePositions: 4,
	}
	if mutate != nil {
		mutate(layout)
	}
	f.params = make([][]byte, 8)
	for i := range f.params {
		sum := sha256.Sum256([]byte(fmt.Sprintf("expert-params-%d", i)))
		f.params[i] = sum[:]
	}
	root, err := core.MoeExpertRoot(2, 4, f.params)
	if err != nil {
		t.Fatal(err)
	}
	layout.ExpertWeightsRoot = root
	attnHash := bytes.Repeat([]byte{42}, 32)
	spec := &pb.ShardSpec{Index: 0, UnitKind: "attention", StartLayer: 0, EndLayer: 2,
		HasEmbedding: true, HasLmHead: true, ParametersHash: attnHash}
	f.manifest = &pb.ModelManifest{ChainId: "moe-test-model", TotalLayers: 2,
		Shards: []*pb.ShardSpec{spec}, MoeLayout: layout}
	if err := core.Sign(f.manifest, a["governor"].key); err != nil {
		t.Fatal(err)
	}
	mroot, _ := core.Digest(f.manifest)
	f.root = mroot
	f.genesis = &pb.ChainGenesis{ChainId: "moe-test-model", ModelManifestRoot: mroot,
		ExecutionProfileVer: "moe-test-v1",
		NumericAuditPolicy: &pb.NumericAuditPolicy{Metric: "union-interval-v1", Bands: []*pb.ToleranceBand{
			{ProducerEngine: "moe", ProducerDevice: "cpu", ReferenceEngine: "moe", ReferenceDevice: "cpu",
				Tolerance: 0.01, Mode: "enforce", SegmentKind: "expert"},
			{ProducerEngine: "moe", ProducerDevice: "cpu", ReferenceEngine: "moe", ReferenceDevice: "cpu",
				Tolerance: 0.01, Mode: "enforce", SegmentKind: "attention"},
		}}}
	if err := core.Sign(f.genesis, a["governor"].key); err != nil {
		t.Fatal(err)
	}
	gr, _ := proto.Marshal(f.genesis)
	mr, _ := proto.Marshal(f.manifest)
	gh := sha256.Sum256(gr)
	mh := sha256.Sum256(mr)
	model := Model{ID: "moe-test-model", GenesisHash: hex.EncodeToString(gh[:]),
		ProfileHash: hex.EncodeToString(mh[:]), Genesis: gr, Manifest: mr}
	if err := validateModelArtifacts(model); err != nil {
		t.Fatal(err)
	}
	s.Models["moe-test-model"] = model
	// Audit compensation formula v3 (governance action).
	comp := &AuditCompParameters{FormulaVer: 3, OFixed: 10, RIn: 100, ROut: 100,
		RSegPerKRow: 5000, MarginBPS: 20000, AuditShareBPS: 1500,
		BenchmarkRef: hex.EncodeToString(bytes.Repeat([]byte{0xab}, 32))}
	apply(t, s, a["governor"], 1, Action{Type: "set_audit_comp", ModelID: "moe-test-model", AuditComp: comp}, 1)
	// Balances and stakes.
	s.Accounts[a["payer"].id].Balance = 10_000_000
	register := func(node actor, profile *pb.ResourceProfile) {
		s.Accounts[node.id] = &Account{Stake: 5_000_000}
		profile.NodeId = node.id
		profile.ChainId = "moe-test-model"
		profile.ManifestRoot = mroot
		profile.ExecutionProfileVer = "moe-test-v1"
		profile.ActualDevice = "cpu"
		profile.RequestedDevice = "cpu"
		if err := core.Sign(profile, node.key); err != nil {
			t.Fatal(err)
		}
		raw, _ := proto.Marshal(profile)
		apply(t, s, node, 1, Action{Type: "register_node", Profile: raw, ExpiresHeight: 90}, 1)
	}
	register(f.attn, &pb.ResourceProfile{ShardIndex: 0, ShardStartLayer: 0, ShardEndLayer: 2,
		ShardTotalLayers: 2, ShardHasEmbedding: true, ShardHasLmHead: true,
		ShardParametersHash: attnHash, ShardUnitKind: "attention", MoeUnitKind: "attention",
		EngineCapabilities: &pb.EngineCapabilities{Roles: []string{"moe-attention"}}})
	expertProfile := func(units []uint32) *pb.ResourceProfile {
		p := &pb.ResourceProfile{MoeUnitKind: "expert",
			EngineCapabilities: &pb.EngineCapabilities{Roles: []string{"moe-expert"}}}
		for _, idx := range units {
			p.MoeUnits = append(p.MoeUnits, &pb.MoeUnitRegistration{
				Layer: idx / 4, Expert: idx % 4, ParamsHash: f.params[idx],
				MerkleProof: moeProof(t, f.params, int(idx)), LeafIndex: idx})
		}
		return p
	}
	register(f.expB, expertProfile([]uint32{0, 1, 4, 5}))
	register(f.expC, expertProfile([]uint32{2, 3, 6, 7}))
	register(f.expB2, expertProfile([]uint32{0, 1, 4, 5}))
	register(f.expC2, expertProfile([]uint32{2, 3, 6, 7}))
	return f
}

func (f *moeChainFixture) authorizations() []MoeAuthorization {
	return []MoeAuthorization{
		{NodeID: f.attn.id, Kind: "attention", StartLayer: 0, EndLayer: 2},
		{NodeID: f.expB.id, Kind: "expert", Units: []MoeUnitRef{{0, 0}, {0, 1}, {1, 0}, {1, 1}}},
		{NodeID: f.expC.id, Kind: "expert", Units: []MoeUnitRef{{0, 2}, {0, 3}, {1, 2}, {1, 3}}},
	}
}

// buildReceipt assembles the signed MoE execution for input=3, output=2
// (frames [3,1]) with the same routing as the core fixture.
func (f *moeChainFixture) buildReceipt(t *testing.T) *pb.PipelineExecutionReceipt {
	t.Helper()
	routing := map[[2]uint32][]uint32{
		{0, 0}: {0, 1, 1, 2, 0, 2},
		{0, 1}: {3, 0, 3, 1, 2, 3},
		{1, 0}: {1, 3},
		{1, 1}: {0, 2},
	}
	hash := func(tag string, parts ...uint32) []byte {
		h := sha256.New()
		h.Write([]byte(tag))
		var b [4]byte
		for _, p := range parts {
			b[0], b[1], b[2], b[3] = byte(p>>24), byte(p>>16), byte(p>>8), byte(p)
			h.Write(b[:])
		}
		return h.Sum(nil)
	}
	data := &pb.MoeExecutionData{}
	records := map[[2]uint32]*pb.RoutingRecord{}
	for frame := uint32(0); frame < 2; frame++ {
		size := uint32(3)
		if frame == 1 {
			size = 1
		}
		for layer := uint32(0); layer < 2; layer++ {
			rec := &pb.RoutingRecord{Frame: frame, Layer: layer, Positions: size,
				Experts: routing[[2]uint32{frame, layer}], InputHash: hash("aatt", frame, layer)}
			records[[2]uint32{frame, layer}] = rec
			data.RoutingRecords = append(data.RoutingRecords, rec)
		}
	}
	keyOf := map[string]actor{"attn": f.attn, "expB": f.expB, "expC": f.expC}
	segsByNodeFrame := map[string]map[uint32][]*pb.SegmentReceipt{}
	addSeg := func(role string, seg *pb.SegmentReceipt) {
		node := keyOf[role]
		seg.NodeId = node.id
		seg.ChainId = "moe-test-model"
		seg.ManifestRoot = f.root
		seg.ExecutionProfileVer = "moe-test-v1"
		seg.RequestId = "moe-req"
		if err := core.Sign(seg, node.key); err != nil {
			t.Fatal(err)
		}
		data.SegmentReceipts = append(data.SegmentReceipts, seg)
		if segsByNodeFrame[role] == nil {
			segsByNodeFrame[role] = map[uint32][]*pb.SegmentReceipt{}
		}
		segsByNodeFrame[role][seg.Frame] = append(segsByNodeFrame[role][seg.Frame], seg)
	}
	attnProfile := f.s.Nodes[f.attn.id]
	_ = attnProfile
	for frame := uint32(0); frame < 2; frame++ {
		setHash, err := core.MoeRoutingSetHash([]*pb.RoutingRecord{records[[2]uint32{frame, 0}], records[[2]uint32{frame, 1}]})
		if err != nil {
			t.Fatal(err)
		}
		size := uint32(3)
		if frame == 1 {
			size = 1
		}
		addSeg("attn", &pb.SegmentReceipt{Kind: "attention", Layer: 0, Frame: frame,
			StartLayer: 0, EndLayer: 2, InputHash: hash("in", frame), OutputHash: hash("out", frame),
			RoutingRecordHash: setHash, ParamsHash: bytes.Repeat([]byte{42}, 32), TokenRows: size})
		for layer := uint32(0); layer < 2; layer++ {
			counts := map[uint32]uint32{}
			for _, e := range routing[[2]uint32{frame, layer}] {
				counts[e]++
			}
			rh, err := core.MoeRoutingRecordHash(records[[2]uint32{frame, layer}])
			if err != nil {
				t.Fatal(err)
			}
			for expert, n := range counts {
				role := "expB"
				if expert >= 2 {
					role = "expC"
				}
				addSeg(role, &pb.SegmentReceipt{Kind: "expert", Layer: layer, Expert: expert, Frame: frame,
					StartLayer: layer, EndLayer: layer + 1,
					InputHash: records[[2]uint32{frame, layer}].InputHash,
					OutputHash: hash("eout", frame, layer, expert),
					RoutingRecordHash: rh, ParamsHash: f.params[layer*4+expert], TokenRows: n})
			}
		}
	}
	for role, byFrame := range segsByNodeFrame {
		for frame, segs := range byFrame {
			hashes := make([][]byte, 0, len(segs))
			for _, seg := range segs {
				h, err := core.MoeSegmentReceiptHash(seg)
				if err != nil {
					t.Fatal(err)
				}
				hashes = append(hashes, h)
			}
			fr := &pb.NodeFrameReceipt{NodeId: keyOf[role].id, RequestId: "moe-req", Frame: frame,
				SegmentsRoot: core.MoeFrameRoot(hashes), SegmentCount: uint32(len(segs))}
			if err := core.Sign(fr, keyOf[role].key); err != nil {
				t.Fatal(err)
			}
			data.FrameReceipts = append(data.FrameReceipts, fr)
		}
	}
	return &pb.PipelineExecutionReceipt{RequestId: "moe-req", InputTokens: 3, OutputTokens: 2,
		ElapsedMs: 10, OutputCommitment: bytes.Repeat([]byte{8}, 32), Moe: data}
}

func (f *moeChainFixture) freezeSubmit(t *testing.T) {
	t.Helper()
	apply(t, f.s, f.a["payer"], 1, Action{Type: "freeze_moe", HoldID: "moe-req",
		Payer: f.a["payer"].id, Amount: 1_000_000, MoeInputTokens: 3, MoeMaxTokens: 2,
		MoeAuthorizations: f.authorizations()}, 2)
	raw, _ := proto.Marshal(f.buildReceipt(t))
	apply(t, f.s, f.a["coordinator"], 1, Action{Type: "submit_moe", HoldID: "moe-req",
		Actual: 600_000, Receipt: raw}, 3)
	apply(t, f.s, f.a["coordinator"], 2, Action{Type: "assign_audit", HoldID: "moe-req"}, 4)
}

func (f *moeChainFixture) auditorActor(t *testing.T) actor {
	t.Helper()
	h := f.s.Holds["moe-req"]
	if h.Auditor == f.a["auditor"].id {
		return f.a["auditor"]
	}
	if h.Auditor == f.a["auditor2"].id {
		return f.a["auditor2"]
	}
	t.Fatalf("unexpected auditor %s", h.Auditor)
	return actor{}
}

func (f *moeChainFixture) audit(t *testing.T, auditor actor, nonce uint64, height int64,
	verdict string, faultRef *pb.SegmentRef, grade *pb.OffenseGrade) {
	t.Helper()
	h := f.s.Holds["moe-req"]
	drawn, err := h.moeExpectedDraw()
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := hex.DecodeString(h.ReceiptRef)
	proof, _ := hex.DecodeString(h.AuditProof)
	c := &pb.AuditChallenge{ReceiptRef: ref, VrfProof: proof, SegmentRefs: drawn}
	if err := core.Sign(c, auditor.key); err != nil {
		t.Fatal(err)
	}
	cr, _ := core.Digest(c)
	v := &pb.AuditVerdict{ChallengeRef: cr, AuditorId: auditor.id, Verdict: verdict,
		EvidenceRef: bytes.Repeat([]byte{7}, 32), Offense: grade}
	for _, dref := range drawn {
		d := &pb.SegmentDistance{Ref: dref, Distance: 0.001,
			BandKey: "moe/" + dref.Kind + "/cpu->cpu"}
		if faultRef != nil && dref.Kind == faultRef.Kind && dref.Frame == faultRef.Frame &&
			dref.Layer == faultRef.Layer && dref.Expert == faultRef.Expert {
			d.Distance = 5.0
			v.FaultNodeIds = nil // filled below from derivation expectation
		}
		v.SegmentDistances = append(v.SegmentDistances, d)
	}
	if faultRef != nil {
		// Owner of the fault segment, from the frozen receipt.
		receipt := &pb.PipelineExecutionReceipt{}
		if err := proto.Unmarshal(h.Moe.Receipt, receipt); err != nil {
			t.Fatal(err)
		}
		for _, seg := range receipt.Moe.SegmentReceipts {
			if seg.Kind == faultRef.Kind && seg.Frame == faultRef.Frame &&
				seg.Layer == faultRef.Layer && seg.Expert == faultRef.Expert {
				v.FaultNodeIds = []string{seg.NodeId}
			}
		}
	}
	if err := core.Sign(v, auditor.key); err != nil {
		t.Fatal(err)
	}
	cb, _ := proto.Marshal(c)
	vb, _ := proto.Marshal(v)
	apply(t, f.s, auditor, nonce, Action{Type: "audit", HoldID: "moe-req", Challenge: cb, Verdict: vb}, height)
}

func TestMoeChainAcceptSettlement(t *testing.T) {
	f := moeChainState(t)
	f.freezeSubmit(t)
	h := f.s.Holds["moe-req"]
	if h.State != "dispute" || len(h.Moe.Credits) != 3 {
		t.Fatalf("unexpected post-submit state %s credits %v", h.State, h.Moe.Credits)
	}
	credits := map[string]int64{}
	for _, c := range h.Moe.Credits {
		credits[c.NodeID] = c.Amount
	}
	if credits[f.attn.id] != 412_500 || credits[f.expB.id] != 93_750 || credits[f.expC.id] != 93_750 {
		t.Fatalf("FLOPs credits wrong: %v", credits)
	}
	auditor := f.auditorActor(t)
	f.audit(t, auditor, 1, 5, "accept", nil, nil)
	if f.s.Holds["moe-req"].AuditVerdict != "accept" {
		t.Fatal("verdict not accepted")
	}
	expected, err := ExpectedAuditReceipts(f.s.Holds["moe-req"], "accept")
	if err != nil {
		t.Fatal(err)
	}
	if len(expected) != 1 || expected[0].AuditComp.FormulaVer != 3 || expected[0].AuditComp.SegCount == 0 {
		t.Fatalf("unexpected audit receipts %+v", expected)
	}
	comp := expected[0].AuditComp.CompSubunits
	apply(t, f.s, f.a["coordinator"], 3, Action{Type: "settle", HoldID: "moe-req",
		Outcome: "accept", AuditReceipts: expected}, 6)
	h = f.s.Holds["moe-req"]
	if h.State != "settled" {
		t.Fatalf("state = %s", h.State)
	}
	if got := f.s.Accounts[f.attn.id].Balance; got != 330_000 {
		t.Fatalf("attn balance = %d, want 330000", got)
	}
	if got := f.s.Accounts[f.expB.id].Balance; got != 75_000 {
		t.Fatalf("expB balance = %d, want 75000", got)
	}
	if got := f.s.Accounts[f.expC.id].Balance; got != 75_000 {
		t.Fatalf("expC balance = %d, want 75000", got)
	}
	if got := f.s.Accounts[f.a["payer"].id].Balance; got != 9_430_000 {
		t.Fatalf("payer balance = %d, want 9430000", got)
	}
	if got := f.s.Accounts[auditor.id].Balance; got != 1000+comp {
		t.Fatalf("auditor balance = %d, want %d", got, 1000+comp)
	}
	if f.s.exposure(f.attn.id) != 0 || f.s.exposure(f.expB.id) != 0 {
		t.Fatal("exposure not released")
	}
}

func TestMoeChainPlanBReject(t *testing.T) {
	f := moeChainState(t)
	f.freezeSubmit(t)
	h := f.s.Holds["moe-req"]
	drawn, err := h.moeExpectedDraw()
	if err != nil {
		t.Fatal(err)
	}
	// Pick the drawn segment with the smallest due as the cheater, so the
	// 20x slash stays below the stake cap and the fund math is exact.
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
	grade := &pb.OffenseGrade{Class: "objective", Fault: "hash"}
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
	// Expected Plan B accounting, recomputed from the frozen dues.
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
	h = f.s.Holds["moe-req"]
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
		t.Fatalf("fault node stake = %d", got)
	}
	for node, want := range honestByNode {
		if got := f.s.Accounts[node].Balance; got != want {
			t.Fatalf("honest node %s balance = %d, want %d", node, got, want)
		}
	}
	payerWant := int64(10_000_000-1_000_000) + (1_000_000-600_000) + 90_000 + plan.LeftoverToPayer
	if got := f.s.Accounts[f.a["payer"].id].Balance; got != payerWant {
		t.Fatalf("payer balance = %d, want %d", got, payerWant)
	}
}

func TestMoeCoverageGateAndEnableGate(t *testing.T) {
	// Enable gate: a state without MoeEnabled rejects freeze_moe.
	s, a := testState(t)
	rejected(t, s, a["payer"], Action{Type: "freeze_moe", HoldID: "x", Payer: a["payer"].id,
		Amount: 10, MoeInputTokens: 1, MoeMaxTokens: 1,
		MoeAuthorizations: []MoeAuthorization{{NodeID: a["node"].id, Kind: "attention"}}}, 2)
	// Coverage gate: with only one registered holder per unit, freeze fails.
	f := moeChainState(t)
	// Remove the standby holders by expiring their registrations.
	f.s.Nodes[f.expB2.id].ExpiresHeight = 1
	f.s.Nodes[f.expC2.id].ExpiresHeight = 1
	rejected(t, f.s, f.a["payer"], Action{Type: "freeze_moe", HoldID: "moe-req",
		Payer: f.a["payer"].id, Amount: 1_000_000, MoeInputTokens: 3, MoeMaxTokens: 2,
		MoeAuthorizations: f.authorizations()}, 2)
}

func TestMoeDynamicPricingRequiresSignedCommitment(t *testing.T) {
	f := moeChainState(t)
	f.s.Version = 11
	model := f.s.Models["moe-test-model"]
	model.TokenPrices = &core.TokenPrices{PriceInputPer1K: 195, PriceOutputPer1K: 217}
	f.s.Models[model.ID] = model
	fee, err := core.EffectiveTokenFee(f.genesis, model.TokenPrices, 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	amount, err := core.ScaleFee(fee.Total, f.s.PriceForProfile(model.ID).MultiplierPPM)
	if err != nil {
		t.Fatal(err)
	}
	action := Action{Type: "freeze_moe", HoldID: "moe-req", Payer: f.a["payer"].id, Amount: amount, MoeInputTokens: 3, MoeMaxTokens: 2, MoeAuthorizations: f.authorizations()}
	rejectedWith(t, f.s, f.a["payer"], action, 2, "dynamic price requires tokenization commitment")
	c := &pb.ExecutionCommitment{RequestId: action.HoldID, ChainId: model.ID, ProfileVer: f.genesis.ExecutionProfileVer, InputCommitment: bytes.Repeat([]byte{5}, 32), InputTokens: 3, MaxOutputTokens: 2, TokenizerVersion: "test"}
	if err := core.Sign(c, f.a["coordinator"].key); err != nil {
		t.Fatal(err)
	}
	action.PricingCommitment, err = proto.MarshalOptions{Deterministic: true}.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	apply(t, f.s, f.a["payer"], 1, action, 2)
	if h := f.s.Holds[action.HoldID]; h.TokenFee == nil || !bytes.Equal(h.TokenFee.Commitment, action.PricingCommitment) {
		t.Fatal("missing frozen commitment")
	}
}
