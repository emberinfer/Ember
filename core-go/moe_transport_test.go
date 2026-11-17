package core

import (
	"bytes"
	"crypto/ed25519"
	"reflect"
	"strings"
	"testing"

	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

func deterministicMoeBytes(t *testing.T, r proto.Message) []byte {
	t.Helper()
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func TestMoeTransportSignedExpansion(t *testing.T) {
	f := newMoeFixture(t)
	v1 := f.buildExecution(t)
	original := deterministicMoeBytes(t, v1)
	v2, err := CompactMoeReceipt(v1)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, deterministicMoeBytes(t, v1)) {
		t.Fatal("compaction mutated v1")
	}
	wire := deterministicMoeBytes(t, v2)
	expanded, err := ExpandMoeReceipt(v2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, deterministicMoeBytes(t, expanded)) {
		t.Fatal("expanded signed bytes/roots differ from v1")
	}
	if !bytes.Equal(wire, deterministicMoeBytes(t, v2)) {
		t.Fatal("expansion mutated v2")
	}
	for _, segment := range expanded.Moe.SegmentReceipts {
		if err := Verify(segment); err != nil {
			t.Fatal("expanded segment signature", err)
		}
	}
	for _, frame := range expanded.Moe.FrameReceipts {
		if err := Verify(frame); err != nil {
			t.Fatal("expanded frame signature", err)
		}
	}
	before, err := VerifyMoeExecution(f.manifest, v1, f.chain, f.root, f.profile, f.bindings)
	if err != nil {
		t.Fatal(err)
	}
	after, err := VerifyMoeExecution(f.manifest, v2, f.chain, f.root, f.profile, f.bindings)
	if err != nil {
		t.Fatal(err)
	}
	// Merged work's Frame member is not semantic (only Instances uses it);
	// the legacy merge retains whichever frame its map visits first.
	for i := range before.Segments {
		before.Segments[i].Frame = 0
	}
	for i := range after.Segments {
		after.Segments[i].Frame = 0
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("compact transport changed verified work")
	}
	// No output aliases the input: even byte slices restored from a shared header
	// belong to independently cloned messages.
	expanded.Moe.SegmentReceipts[0].ManifestRoot[0] ^= 1
	if !bytes.Equal(wire, deterministicMoeBytes(t, v2)) {
		t.Fatal("expanded root aliases compact header")
	}
}
func TestMoeTransportHeaderTamperAndMalformed(t *testing.T) {
	f := newMoeFixture(t)
	v1 := f.buildExecution(t)
	v2, err := CompactMoeReceipt(v1)
	if err != nil {
		t.Fatal(err)
	}
	changes := map[string]func(*pb.PipelineExecutionReceipt){
		"chain":    func(r *pb.PipelineExecutionReceipt) { r.Moe.Header.ChainId += "x" },
		"manifest": func(r *pb.PipelineExecutionReceipt) { r.Moe.Header.ManifestRoot[0] ^= 1 },
		"profile":  func(r *pb.PipelineExecutionReceipt) { r.Moe.Header.ExecutionProfileVer += "x" },
		"request":  func(r *pb.PipelineExecutionReceipt) { r.RequestId += "x"; r.Moe.Header.RequestId = r.RequestId },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			altered := proto.Clone(v2).(*pb.PipelineExecutionReceipt)
			change(altered)
			expanded, err := ExpandMoeReceipt(altered)
			if err != nil {
				t.Fatal(err)
			}
			if err = Verify(expanded.Moe.SegmentReceipts[0]); err == nil {
				t.Fatal("header tamper retained valid segment signature")
			}
			if _, err = VerifyMoeExecution(f.manifest, altered, f.chain, f.root, f.profile, f.bindings); err == nil {
				t.Fatal("header tamper accepted")
			}
		})
	}
	malformed := map[string]func(*pb.PipelineExecutionReceipt){
		"unknown version":      func(r *pb.PipelineExecutionReceipt) { r.Moe.FormatVersion = 3 },
		"no header":            func(r *pb.PipelineExecutionReceipt) { r.Moe.Header = nil },
		"short root":           func(r *pb.PipelineExecutionReceipt) { r.Moe.Header.ManifestRoot = []byte{1} },
		"request mismatch":     func(r *pb.PipelineExecutionReceipt) { r.Moe.Header.RequestId += "x" },
		"segment not stripped": func(r *pb.PipelineExecutionReceipt) { r.Moe.SegmentReceipts[0].ChainId = "x" },
		"frame not stripped":   func(r *pb.PipelineExecutionReceipt) { r.Moe.FrameReceipts[0].RequestId = "x" },
		"v1 with header":       func(r *pb.PipelineExecutionReceipt) { r.Moe.FormatVersion = 0 },
	}
	for name, change := range malformed {
		t.Run(name, func(t *testing.T) {
			r := proto.Clone(v2).(*pb.PipelineExecutionReceipt)
			change(r)
			if _, err := ExpandMoeReceipt(r); err == nil {
				t.Fatal("malformed format accepted")
			}
		})
	}
}

// Design section 1.2 representative decode frame: 130 signed segment receipts
// (2 attention +16*8 expert),16 routing records,4 signed node frame seals.
func representativeMoeFrame(t *testing.T) *pb.PipelineExecutionReceipt {
	t.Helper()
	chain, profile, request := strings.Repeat("c", 24), strings.Repeat("p", 12), strings.Repeat("r", 38)
	root := bytes.Repeat([]byte{7}, 32)
	r := &pb.PipelineExecutionReceipt{RequestId: request, InputTokens: 1, OutputTokens: 2, ElapsedMs: 1, OutputCommitment: root, Moe: &pb.MoeExecutionData{}}
	keys := make([]ed25519.PrivateKey, 4)
	hashes := make([][][]byte, 4)
	for i := range keys {
		keys[i] = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{byte(i + 1)}, 32))
	}
	for i := 0; i < 130; i++ {
		owner := 0
		kind := "attention"
		layer, expert := uint32(i*8), uint32(0)
		if i < 2 {
			owner = i
		} else {
			owner = 2 + (i % 2)
			kind = "expert"
			layer = uint32((i - 2) / 8)
			expert = uint32((i - 2) % 8)
		}
		seg := &pb.SegmentReceipt{NodeId: NodeID(keys[owner].Public().(ed25519.PublicKey)), ChainId: chain, ManifestRoot: root, ExecutionProfileVer: profile, RequestId: request, Kind: kind, Layer: layer, Expert: expert, Frame: 1, StartLayer: layer, EndLayer: layer + 1, InputHash: root, OutputHash: root, RoutingRecordHash: root, ParamsHash: root, TokenRows: 1}
		if err := Sign(seg, keys[owner]); err != nil {
			t.Fatal(err)
		}
		hash, err := MoeSegmentReceiptHash(seg)
		if err != nil {
			t.Fatal(err)
		}
		hashes[owner] = append(hashes[owner], hash)
		r.Moe.SegmentReceipts = append(r.Moe.SegmentReceipts, seg)
	}
	for layer := uint32(0); layer < 16; layer++ {
		r.Moe.RoutingRecords = append(r.Moe.RoutingRecords, &pb.RoutingRecord{Frame: 1, Layer: layer, Positions: 1, Experts: []uint32{0, 1, 2, 3, 4, 5, 6, 7}, InputHash: root})
	}
	for owner, key := range keys {
		fr := &pb.NodeFrameReceipt{NodeId: NodeID(key.Public().(ed25519.PublicKey)), RequestId: request, Frame: 1, SegmentsRoot: MoeFrameRoot(hashes[owner]), SegmentCount: uint32(len(hashes[owner]))}
		if err := Sign(fr, key); err != nil {
			t.Fatal(err)
		}
		r.Moe.FrameReceipts = append(r.Moe.FrameReceipts, fr)
	}
	return r
}
func TestMoeTransportRepresentativeFrameSize(t *testing.T) {
	v1 := representativeMoeFrame(t)
	v2, err := CompactMoeReceipt(v1)
	if err != nil {
		t.Fatal(err)
	}
	for i, seg := range v1.Moe.SegmentReceipts {
		if saved := proto.Size(seg) - proto.Size(v2.Moe.SegmentReceipts[i]); saved != 114 {
			t.Fatalf("segment %d saved %d, want114", i, saved)
		}
	}
	for i, fr := range v1.Moe.FrameReceipts {
		if saved := proto.Size(fr) - proto.Size(v2.Moe.FrameReceipts[i]); saved != 40 {
			t.Fatalf("frame %d saved %d, want40", i, saved)
		}
	}
	before, after := proto.Size(v1), proto.Size(v2)
	t.Logf("representative signed decode frame v1=%dB v2=%dB ratio=%.6f per-segment=114B per-frame-receipt=40B", before, after, float64(after)/float64(before))
	if after*100 >= before*80 {
		t.Fatalf("insufficient transport saving: %d -> %d", before, after)
	}
	expanded, err := ExpandMoeReceipt(v2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(deterministicMoeBytes(t, v1), deterministicMoeBytes(t, expanded)) {
		t.Fatal("representative signed bytes differ")
	}
	for _, seg := range expanded.Moe.SegmentReceipts {
		if err = Verify(seg); err != nil {
			t.Fatal(err)
		}
	}
	for _, fr := range expanded.Moe.FrameReceipts {
		if err = Verify(fr); err != nil {
			t.Fatal(err)
		}
	}
}
