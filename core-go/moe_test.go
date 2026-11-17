package core

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"math/rand"
	"testing"

	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

func moeHash(tag string, parts ...uint32) []byte {
	h := sha256.New()
	h.Write([]byte(tag))
	var b [4]byte
	for _, p := range parts {
		binary.BigEndian.PutUint32(b[:], p)
		h.Write(b[:])
	}
	return h.Sum(nil)
}

type moeFixture struct {
	manifest     *pb.ModelManifest
	layout       *pb.MoeLayout
	root         []byte
	chain        string
	profile      string
	keys         map[string]ed25519.PrivateKey
	nodes        map[string]string // role -> node id
	bindings     map[string]MoeProfileBinding
	expertParams [][]byte // index layer*E+expert
}

func newMoeFixture(t *testing.T) *moeFixture {
	t.Helper()
	layout := &pb.MoeLayout{
		NumLayers: 2, HiddenSize: 8, ExpertsPerLayer: 4, TopK: 2,
		ExpertParamCount: 50, AttentionParamCount: 100,
		VocabSize: 16, SegmentDtype: "bf16", EngineImplId: "test-moe-v1",
		RoutingCapBeta: 4, ScarcityMax: 8, SegmentSettlement: "granular",
		MaxFramePositions: 4,
	}
	expertParams := make([][]byte, 8)
	for i := range expertParams {
		expertParams[i] = moeHash("expert-params", uint32(i))
	}
	root, err := MoeExpertRoot(2, 4, expertParams)
	if err != nil {
		t.Fatal(err)
	}
	layout.ExpertWeightsRoot = root
	manifest := &pb.ModelManifest{ChainId: "moe-test", TotalLayers: 2, MoeLayout: layout}
	f := &moeFixture{manifest: manifest, layout: layout, root: moeHash("manifest"),
		chain: "moe-test", profile: "test-moe-v1",
		keys: map[string]ed25519.PrivateKey{}, nodes: map[string]string{},
		bindings: map[string]MoeProfileBinding{}, expertParams: expertParams}
	mk := func(role string) string {
		_, key, err := ed25519.GenerateKey(rand.New(rand.NewSource(int64(len(f.keys) + 7))))
		if err != nil {
			t.Fatal(err)
		}
		f.keys[role] = key
		id := NodeID([]byte(key.Public().(ed25519.PublicKey)))
		f.nodes[role] = id
		return id
	}
	attn := mk("attn")
	f.bindings[attn] = MoeProfileBinding{UnitKind: "attention", ParamsHash: moeHash("range", 0, 2),
		StartLayer: 0, EndLayer: 2, HasHead: true}
	expB := mk("expB")
	expC := mk("expC")
	unitsB := map[[2]uint32][]byte{}
	unitsC := map[[2]uint32][]byte{}
	for layer := uint32(0); layer < 2; layer++ {
		unitsB[[2]uint32{layer, 0}] = expertParams[layer*4+0]
		unitsB[[2]uint32{layer, 1}] = expertParams[layer*4+1]
		unitsC[[2]uint32{layer, 2}] = expertParams[layer*4+2]
		unitsC[[2]uint32{layer, 3}] = expertParams[layer*4+3]
	}
	f.bindings[expB] = MoeProfileBinding{UnitKind: "expert", ExpertUnits: unitsB}
	f.bindings[expC] = MoeProfileBinding{UnitKind: "expert", ExpertUnits: unitsC}
	return f
}

// buildExecution assembles a fully signed, coverage-complete execution for
// input=3, output=2 (frames [3,1]).
func (f *moeFixture) buildExecution(t *testing.T) *pb.PipelineExecutionReceipt {
	t.Helper()
	routing := map[[2]uint32][]uint32{
		{0, 0}: {0, 1, 1, 2, 0, 2},
		{0, 1}: {3, 0, 3, 1, 2, 3},
		{1, 0}: {1, 3},
		{1, 1}: {0, 2},
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
				Experts: routing[[2]uint32{frame, layer}], InputHash: moeHash("aatt", frame, layer)}
			records[[2]uint32{frame, layer}] = rec
			data.RoutingRecords = append(data.RoutingRecords, rec)
		}
	}
	segsByNodeFrame := map[string]map[uint32][]*pb.SegmentReceipt{}
	addSeg := func(role string, seg *pb.SegmentReceipt) {
		seg.NodeId = f.nodes[role]
		seg.ChainId = f.chain
		seg.ManifestRoot = f.root
		seg.ExecutionProfileVer = f.profile
		seg.RequestId = "req-1"
		if err := Sign(seg, f.keys[role]); err != nil {
			t.Fatal(err)
		}
		data.SegmentReceipts = append(data.SegmentReceipts, seg)
		if segsByNodeFrame[role] == nil {
			segsByNodeFrame[role] = map[uint32][]*pb.SegmentReceipt{}
		}
		segsByNodeFrame[role][seg.Frame] = append(segsByNodeFrame[role][seg.Frame], seg)
	}
	for frame := uint32(0); frame < 2; frame++ {
		setHash, err := MoeRoutingSetHash([]*pb.RoutingRecord{records[[2]uint32{frame, 0}], records[[2]uint32{frame, 1}]})
		if err != nil {
			t.Fatal(err)
		}
		size := uint32(3)
		if frame == 1 {
			size = 1
		}
		addSeg("attn", &pb.SegmentReceipt{Kind: "attention", Layer: 0, Frame: frame,
			StartLayer: 0, EndLayer: 2, InputHash: moeHash("in", frame), OutputHash: moeHash("out", frame),
			RoutingRecordHash: setHash, ParamsHash: f.bindings[f.nodes["attn"]].ParamsHash, TokenRows: size})
		for layer := uint32(0); layer < 2; layer++ {
			counts := map[uint32]uint32{}
			for _, e := range routing[[2]uint32{frame, layer}] {
				counts[e]++
			}
			rh, err := MoeRoutingRecordHash(records[[2]uint32{frame, layer}])
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
					InputHash: records[[2]uint32{frame, layer}].InputHash, OutputHash: moeHash("eout", frame, layer, expert),
					RoutingRecordHash: rh, ParamsHash: f.expertParams[layer*4+expert], TokenRows: n})
			}
		}
	}
	for role, byFrame := range segsByNodeFrame {
		for frame, segs := range byFrame {
			hashes := make([][]byte, 0, len(segs))
			for _, seg := range segs {
				h, err := MoeSegmentReceiptHash(seg)
				if err != nil {
					t.Fatal(err)
				}
				hashes = append(hashes, h)
			}
			fr := &pb.NodeFrameReceipt{NodeId: f.nodes[role], RequestId: "req-1", Frame: frame,
				SegmentsRoot: MoeFrameRoot(hashes), SegmentCount: uint32(len(segs))}
			if err := Sign(fr, f.keys[role]); err != nil {
				t.Fatal(err)
			}
			data.FrameReceipts = append(data.FrameReceipts, fr)
		}
	}
	return &pb.PipelineExecutionReceipt{RequestId: "req-1", InputTokens: 3, OutputTokens: 2,
		ElapsedMs: 10, Moe: data}
}

func TestMoeCoverageAndFlops(t *testing.T) {
	f := newMoeFixture(t)
	receipt := f.buildExecution(t)
	work, err := VerifyMoeExecution(f.manifest, receipt, f.chain, f.root, f.profile, f.bindings)
	if err != nil {
		t.Fatal(err)
	}
	// Hand-computed FLOPs.
	// Attention per layer per position: 2*100 + 2*8*4 + 2*2*8 = 200+64+32 = 296.
	// Frame 0 (size 3, offset 0): context sum = 0 + 3*4/2 = 6; per layer:
	// 3*296 + 4*8*6 = 888 + 192 = 1080. Two layers: 2160.
	// Frame 1 (size 1, offset 3): context sum = 3 + 1 = 4; per layer:
	// 296 + 128 = 424. Two layers: 848.
	// Head: frames producing tokens: frame 0 (last prefill) + frame 1 = 2
	// tokens * 2*8*16 = 512. Attention total = 2160+848+512 = 3520.
	// Experts: rows total = frame0: 6+6, frame1: 2+2 = 16 rows * 2*50 = 1600.
	if got := work.PerNode[f.nodes["attn"]]; got != 3520 {
		t.Fatalf("attention flops = %d, want 3520", got)
	}
	expertTotal := work.PerNode[f.nodes["expB"]] + work.PerNode[f.nodes["expC"]]
	if expertTotal != 1600 {
		t.Fatalf("expert flops = %d, want 1600", expertTotal)
	}
	if work.TotalFlops != 5120 {
		t.Fatalf("total flops = %d, want 5120", work.TotalFlops)
	}
	if work.ExpertRows[[2]uint32{0, 0}] != 2 || work.ExpertRows[[2]uint32{1, 3}] != 3 {
		t.Fatalf("expert rows wrong: %v", work.ExpertRows)
	}
}

func TestMoeCoverageRejectsTampering(t *testing.T) {
	f := newMoeFixture(t)
	// Missing expert segment.
	receipt := f.buildExecution(t)
	data := receipt.Moe
	data.SegmentReceipts = data.SegmentReceipts[:len(data.SegmentReceipts)-1]
	if _, err := VerifyMoeExecution(f.manifest, receipt, f.chain, f.root, f.profile, f.bindings); err == nil {
		t.Fatal("missing segment accepted")
	}
	// Routing record changed after signing (set hash mismatch).
	receipt = f.buildExecution(t)
	receipt.Moe.RoutingRecords[0].Experts[0] = 3
	if _, err := VerifyMoeExecution(f.manifest, receipt, f.chain, f.root, f.profile, f.bindings); err == nil {
		t.Fatal("tampered routing record accepted")
	}
	// Wrong row count on an expert receipt (resigned by its holder).
	receipt = f.buildExecution(t)
	for _, seg := range receipt.Moe.SegmentReceipts {
		if seg.Kind == "expert" {
			seg.TokenRows++
			role := "expB"
			if seg.Expert >= 2 {
				role = "expC"
			}
			if err := Sign(seg, f.keys[role]); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if _, err := VerifyMoeExecution(f.manifest, receipt, f.chain, f.root, f.profile, f.bindings); err == nil {
		t.Fatal("row-count mismatch accepted")
	}
}

func TestMoeRoutingCap(t *testing.T) {
	f := newMoeFixture(t)
	// beta=4, k=2, T=4, E=4 -> cap = ceil(4*2*4/4) = 8: generous.
	if got := RoutingCap(f.layout, 4); got != 8 {
		t.Fatalf("cap = %d, want 8", got)
	}
	// Same execution under beta=1: cap = 2, but (layer 1, expert 3) has 3
	// routed rows -> the cap check itself must reject it.
	receipt := f.buildExecution(t)
	manifestCopy := proto.Clone(f.manifest).(*pb.ModelManifest)
	manifestCopy.MoeLayout.RoutingCapBeta = 1
	if _, err := VerifyMoeExecution(manifestCopy, receipt, f.chain, f.root, f.profile, f.bindings); err == nil {
		t.Fatal("cap breach accepted")
	}
}

func TestMoeExpertProof(t *testing.T) {
	f := newMoeFixture(t)
	// Build proofs by recomputing the tree levels directly.
	layers, experts := uint32(2), uint32(4)
	leaves := make([][]byte, 8)
	for i := range leaves {
		leaves[i] = moeExpertLeaf(uint32(i)/experts, uint32(i)%experts, f.expertParams[i])
	}
	level := leaves
	var proof [][]byte
	idx := uint32(5)
	for len(level) > 1 {
		if idx%2 == 1 {
			proof = append(proof, level[idx-1])
		} else if idx+1 < uint32(len(level)) {
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
	root, err := MoeExpertRoot(layers, experts, f.expertParams)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyMoeExpertProof(root, layers, experts, 1, 1, f.expertParams[5], proof, 5) {
		t.Fatal("valid proof rejected")
	}
	if VerifyMoeExpertProof(root, layers, experts, 1, 1, f.expertParams[4], proof, 5) {
		t.Fatal("wrong params accepted")
	}
	if VerifyMoeExpertProof(root, layers, experts, 1, 2, f.expertParams[5], proof, 5) {
		t.Fatal("wrong leaf index accepted")
	}
}

func TestMoeFramesAndDraw(t *testing.T) {
	frames := MoeFrames(10, 3, 4)
	want := []uint32{4, 4, 2, 1, 1}
	if len(frames) != len(want) {
		t.Fatalf("frames = %v", frames)
	}
	for i := range want {
		if frames[i] != want[i] {
			t.Fatalf("frames = %v, want %v", frames, want)
		}
	}
	cands := []MoeSegmentCandidate{
		{Ref: &pb.SegmentRef{Kind: "expert", Layer: 0, Expert: 0, Frame: 0}, Work: 100},
		{Ref: &pb.SegmentRef{Kind: "expert", Layer: 0, Expert: 1, Frame: 0}, Work: 300},
		{Ref: &pb.SegmentRef{Kind: "attention", Layer: 0, Frame: 0, EndLayer: 2}, Work: 600},
	}
	a := DrawMoeSegments([]byte("seed"), cands, 2)
	b := DrawMoeSegments([]byte("seed"), cands, 2)
	if len(a) != 2 || a[0] != b[0] || a[1] != b[1] {
		t.Fatal("draw not deterministic")
	}
	if a[0] == a[1] {
		t.Fatal("draw repeated a segment")
	}
	if got := ScarcityMultiplierPPM(2, 1, 8); got != 2_000_000 {
		t.Fatalf("scarcity ppm = %d", got)
	}
	if got := ScarcityMultiplierPPM(2, 4, 8); got != 1_000_000 {
		t.Fatalf("scarcity floor = %d", got)
	}
	if got := ScarcityMultiplierPPM(64, 1, 8); got != 8_000_000 {
		t.Fatalf("scarcity cap = %d", got)
	}
}

func TestSplitWorkChargeMatchesInvocationSplit(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for trial := 0; trial < 200; trial++ {
		amount := int64(rng.Intn(100000))
		var invocations []StageWork
		var shares []WorkShare
		nodes := []string{"a", "b", "c", "d"}
		for _, node := range nodes {
			layers := uint32(1 + rng.Intn(5))
			tokens := uint32(1 + rng.Intn(50))
			invocations = append(invocations, StageWork{NodeID: node, StartLayer: 0, EndLayer: layers, Tokens: tokens})
			shares = append(shares, WorkShare{NodeID: node, Work: uint64(layers) * uint64(tokens)})
		}
		want, err := SplitInvocationCharge(amount, invocations)
		if err != nil {
			t.Fatal(err)
		}
		got, err := SplitWorkCharge(amount, shares)
		if err != nil {
			t.Fatal(err)
		}
		if len(want) != len(got) {
			t.Fatalf("trial %d: length mismatch", trial)
		}
		var sum int64
		for i := range want {
			if want[i] != got[i] {
				t.Fatalf("trial %d: %+v != %+v", trial, want[i], got[i])
			}
			sum += got[i].Amount
		}
		if sum != amount {
			t.Fatalf("trial %d: sum %d != amount %d", trial, sum, amount)
		}
	}
}
