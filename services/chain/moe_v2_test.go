package chain

import (
	"bytes"
	"crypto/sha256"
	"path/filepath"
	"reflect"
	"testing"

	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

func deterministicMoeBytes(t *testing.T, r *pb.PipelineExecutionReceipt) []byte {
	t.Helper()
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func submitMoeTransport(t *testing.T, s *State, co actor, raw []byte, chunked bool) *Hold {
	t.Helper()
	nonce := s.Accounts[co.id].Nonce
	if !chunked {
		apply(t, s, co, nonce+1, Action{Type: "submit_moe", HoldID: "moe-req", Actual: 600_000, Receipt: raw}, 3)
	} else {
		a := manifestAction(raw)
		apply(t, s, co, nonce+1, a, 3)
		for i := 0; i < a.ChunkTotal; i++ {
			apply(t, s, co, nonce+uint64(i)+2, chunkAction(raw, i), 3)
		}
		apply(t, s, co, nonce+uint64(a.ChunkTotal)+2, Action{Type: "submit_moe_finalize", HoldID: "moe-req"}, 3)
	}
	return s.Holds["moe-req"]
}

func normalizedMoeHold(h *Hold, logical []byte) *Hold {
	out := copyMutable(reflect.ValueOf(h)).Interface().(*Hold)
	out.Moe.ChunkRoot = nil
	out.Moe.ChunkListHash = nil
	out.Moe.ChunkBitmap = nil
	out.Moe.ChunkTotal = 0
	out.Moe.ChunkBytes = 0
	out.Moe.ChunkActual = 0
	out.Moe.Receipt = logical
	return out
}

func TestMoeV2SignedExpansionAndTransportEquivalence(t *testing.T) {
	f, _, _ := chunkFixture(t)
	logical := f.buildReceipt(t)
	v1 := deterministicMoeBytes(t, logical)
	compact, err := core.CompactMoeReceipt(logical)
	if err != nil {
		t.Fatal(err)
	}
	v2 := deterministicMoeBytes(t, compact)
	expanded, err := core.ExpandMoeReceipt(compact)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(deterministicMoeBytes(t, expanded), v1) {
		t.Fatal("expanded signed v2 differs from original v1")
	}
	if !bytes.Equal(deterministicMoeBytes(t, logical), v1) || !bytes.Equal(deterministicMoeBytes(t, compact), v2) {
		t.Fatal("compaction/expansion mutated inputs")
	}
	for _, seg := range expanded.Moe.SegmentReceipts {
		if err = core.Verify(seg); err != nil {
			t.Fatal("expanded segment signature", err)
		}
	}
	for _, frame := range expanded.Moe.FrameReceipts {
		if err = core.Verify(frame); err != nil {
			t.Fatal("expanded frame signature", err)
		}
	}
	if len(v2) >= len(v1) {
		t.Fatal("signed fixture did not compact")
	}
	var want *Hold
	for _, test := range []struct {
		name  string
		wire  []byte
		chunk bool
	}{{"v1/legacy", v1, false}, {"v1/chunked", v1, true}, {"v2/legacy", v2, false}, {"v2/chunked", v2, true}} {
		t.Run(test.name, func(t *testing.T) {
			s, e := f.s.Clone()
			if e != nil {
				t.Fatal(e)
			}
			s.SetChunkSpoolPath(filepath.Join(t.TempDir(), "state.json"))
			h := submitMoeTransport(t, s, f.a["coordinator"], test.wire, test.chunk)
			if h.State != "dispute" {
				t.Fatal("receipt did not enter dispute")
			}
			got := normalizedMoeHold(h, v1)
			if want == nil {
				want = got
			} else if !reflect.DeepEqual(got, want) {
				t.Fatal("format/transport changed settlement fields, receipt ref, or challenge plan")
			}
			if test.chunk {
				hash := sha256.Sum256(test.wire)
				if !bytes.Equal(h.Moe.ChunkRoot, hash[:]) || len(h.Moe.Receipt) != 0 {
					t.Fatal("chunk anchor must hash compact transport bytes without retaining receipt")
				}
			} else if !bytes.Equal(h.Moe.Receipt, test.wire) {
				t.Fatal("legacy inline receipt did not retain transport bytes")
			}
		})
	}
	t.Logf("real signed fixture v1=%d bytes v2=%d bytes", len(v1), len(v2))
}

func TestMoeV2GateAndHeaderTamper(t *testing.T) {
	f, _, _ := chunkFixture(t)
	compact, err := core.CompactMoeReceipt(f.buildReceipt(t))
	if err != nil {
		t.Fatal(err)
	}
	v2 := deterministicMoeBytes(t, compact)
	f.s.Version = 12
	rejectChunkAction(t, f.s, f.a["coordinator"], Action{Type: "submit_moe", HoldID: "moe-req", Actual: 600_000, Receipt: v2}, 3)
	// Legacy signed v1 stays accepted even before v13.
	v1 := deterministicMoeBytes(t, f.buildReceipt(t))
	submitMoeTransport(t, f.s, f.a["coordinator"], v1, false)
	for _, chunked := range []bool{false, true} {
		name := "legacy"
		if chunked {
			name = "chunked"
		}
		t.Run(name, func(t *testing.T) {
			f, _, _ := chunkFixture(t)
			compact, e := core.CompactMoeReceipt(f.buildReceipt(t))
			if e != nil {
				t.Fatal(e)
			}
			compact.Moe.Header.ChainId += "-tampered"
			expanded, e := core.ExpandMoeReceipt(compact)
			if e != nil {
				t.Fatal(e)
			}
			if e = core.Verify(expanded.Moe.SegmentReceipts[0]); e == nil {
				t.Fatal("header tamper preserved segment signature")
			}
			raw := deterministicMoeBytes(t, compact)
			action := Action{Type: "submit_moe", HoldID: "moe-req", Actual: 600_000, Receipt: raw}
			if chunked {
				a := manifestAction(raw)
				apply(t, f.s, f.a["coordinator"], 1, a, 3)
				for i := 0; i < a.ChunkTotal; i++ {
					apply(t, f.s, f.a["coordinator"], uint64(i)+2, chunkAction(raw, i), 3)
				}
				action = Action{Type: "submit_moe_finalize", HoldID: "moe-req"}
			}
			rejectChunkAction(t, f.s, f.a["coordinator"], action, 3)
		})
	}
}

func TestMoeV2SpoolFaultAttributionAndAudit(t *testing.T) {
	f, _, _ := chunkFixture(t)
	logical := f.buildReceipt(t)
	compact, err := core.CompactMoeReceipt(logical)
	if err != nil {
		t.Fatal(err)
	}
	raw := deterministicMoeBytes(t, compact)
	h := submitMoeTransport(t, f.s, f.a["coordinator"], raw, true)
	co := f.a["coordinator"]
	apply(t, f.s, co, f.s.Accounts[co.id].Nonce+1, Action{Type: "assign_audit", HoldID: h.ID}, 4)
	h = f.s.Holds[h.ID]
	drawn, err := h.moeExpectedDraw()
	if err != nil {
		t.Fatal(err)
	}
	ref := drawn[0]
	keys, nodes, err := f.s.moeFaultSegments(h, &pb.AuditVerdict{SegmentDistances: []*pb.SegmentDistance{{Ref: ref, Distance: 5, BandKey: "moe/" + ref.Kind + "/cpu->cpu"}}})
	if err != nil || len(keys) != 1 || len(nodes) != 1 {
		t.Fatal("compact spool fault attribution", err)
	}
	owner := ""
	for _, seg := range logical.Moe.SegmentReceipts {
		if seg.Kind == ref.Kind && seg.Frame == ref.Frame && seg.Layer == ref.Layer && (seg.Kind == "attention" || seg.Expert == ref.Expert) {
			owner = seg.NodeId
		}
	}
	if nodes[0] != owner {
		t.Fatal("compact receipt attributed fault to wrong signed owner")
	}
	f.audit(t, f.auditorActor(t), 1, 5, "accept", nil, nil)
	expected, err := ExpectedAuditReceipts(f.s.Holds[h.ID], "accept")
	if err != nil {
		t.Fatal(err)
	}
	apply(t, f.s, co, f.s.Accounts[co.id].Nonce+1, Action{Type: "settle", HoldID: h.ID, Outcome: "accept", AuditReceipts: expected}, 6)
	if f.s.Holds[h.ID].State != "settled" {
		t.Fatal("compact receipt audit did not settle")
	}
}
