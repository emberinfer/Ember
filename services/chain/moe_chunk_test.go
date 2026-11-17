package chain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	abci "github.com/cometbft/cometbft/abci/types"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

func chunkFixture(t *testing.T) (*moeChainFixture, []byte, Action) {
	t.Helper()
	f := moeChainState(t)
	f.s.Version = 13
	f.s.SetChunkSpoolPath(filepath.Join(t.TempDir(), "state.json"))
	apply(t, f.s, f.a["payer"], 1, Action{Type: "freeze_moe", HoldID: "moe-req", Payer: f.a["payer"].id, Amount: 1_000_000, MoeInputTokens: 3, MoeMaxTokens: 2, MoeAuthorizations: f.authorizations()}, 2)
	raw, err := proto.Marshal(f.buildReceipt(t))
	if err != nil {
		t.Fatal(err)
	}
	return f, raw, manifestAction(raw)
}
func manifestAction(raw []byte) Action {
	root := sha256.Sum256(raw)
	a := Action{Type: "submit_moe_manifest", HoldID: "moe-req", Actual: 600_000, ChunkRoot: root[:], ChunkBytes: len(raw), ChunkTotal: (len(raw) + ChunkPayloadBytes - 1) / ChunkPayloadBytes}
	for start := 0; start < len(raw); start += ChunkPayloadBytes {
		end := start + ChunkPayloadBytes
		if end > len(raw) {
			end = len(raw)
		}
		hash := sha256.Sum256(raw[start:end])
		a.ChunkHashes = append(a.ChunkHashes, hash[:])
	}
	return a
}
func chunkAction(raw []byte, index int) Action {
	start := index * ChunkPayloadBytes
	end := start + ChunkPayloadBytes
	if end > len(raw) {
		end = len(raw)
	}
	return Action{Type: "submit_moe_chunk", HoldID: "moe-req", ChunkIndex: index, ReceiptChunk: raw[start:end]}
}
func stateBytes(t *testing.T, s *State) []byte {
	t.Helper()
	raw, err := s.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func rejectChunkAction(t *testing.T, s *State, who actor, a Action, height int64) {
	t.Helper()
	before := stateBytes(t, s)
	raw, err := Sign(s.Parameters.ChainID, who.key, s.Accounts[who.id].Nonce+1, a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Apply(raw, height); err == nil {
		t.Fatalf("accepted invalid %s", a.Type)
	}
	if !bytes.Equal(before, stateBytes(t, s)) {
		t.Fatal("rejection changed state")
	}
}
func TestMoeChunkV12Gate(t *testing.T) {
	for _, kind := range []string{"submit_moe_manifest", "submit_moe_chunk", "submit_moe_finalize"} {
		t.Run(kind, func(t *testing.T) {
			f, raw, a := chunkFixture(t)
			f.s.Version = 12
			if kind == "submit_moe_chunk" {
				a = chunkAction(raw, 0)
			}
			a.Type = kind
			rejectChunkAction(t, f.s, f.a["coordinator"], a, 3)
		})
	}
}
func TestMoeChunkManifestValidation(t *testing.T) {
	cases := map[string]func(*Action, *State){
		"total zero": func(a *Action, s *State) { a.ChunkTotal = 0 }, "total too large": func(a *Action, s *State) { a.ChunkTotal = 257 },
		"bytes zero": func(a *Action, s *State) { a.ChunkBytes = 0 }, "bytes too large": func(a *Action, s *State) { a.ChunkBytes = MaxChunkBytes + 1 },
		"inconsistent size": func(a *Action, s *State) { a.ChunkTotal = 2; a.ChunkHashes = append(a.ChunkHashes, a.ChunkHashes[0]) },
		"hash count":        func(a *Action, s *State) { a.ChunkHashes = nil }, "hash size": func(a *Action, s *State) { a.ChunkHashes = [][]byte{{1}} },
		"root size": func(a *Action, s *State) { a.ChunkRoot = nil }, "actual zero": func(a *Action, s *State) { a.Actual = 0 }, "actual over": func(a *Action, s *State) { a.Actual = 1_000_001 },
		"unknown hold": func(a *Action, s *State) { a.HoldID = "missing" }, "wrong state": func(a *Action, s *State) { s.Holds[a.HoldID].State = "dispute" },
		"expired": func(a *Action, s *State) { s.Holds[a.HoldID].ExpireHeight = 3 },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			f, _, a := chunkFixture(t)
			mut(&a, f.s)
			rejectChunkAction(t, f.s, f.a["coordinator"], a, 3)
		})
	}
	t.Run("not coordinator", func(t *testing.T) { f, _, a := chunkFixture(t); rejectChunkAction(t, f.s, f.a["payer"], a, 3) })
}
func TestMoeChunkManifestStateSizeAndCompatibility(t *testing.T) {
	f, _, a := chunkFixture(t)
	before := stateBytes(t, f.s)
	if strings.Contains(string(before), "chunk_") {
		t.Fatal("empty chunk metadata changed legacy JSON")
	}
	var old State
	if err := json.Unmarshal(before, &old); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, stateBytes(t, &old)) {
		t.Fatal("v12-compatible state did not roundtrip")
	}
	// Measure only metadata; no nonce/height delta from the transaction envelope.
	if err := f.s.submitMoeManifest(f.a["coordinator"].id, a, 3); err != nil {
		t.Fatal(err)
	}
	after := stateBytes(t, f.s)
	delta := len(after) - len(before)
	t.Logf("chunk hold metadata delta: %d bytes", delta)
	if delta >= 1024 {
		t.Fatal("receipt payload entered state")
	}
	if err := f.s.submitMoeManifest(f.a["coordinator"].id, a, 3); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, stateBytes(t, f.s)) {
		t.Fatal("manifest duplicate changed state")
	}
	path := filepath.Join(f.s.chunkSpoolRoot, "moe-req", "manifest.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := f.s.submitMoeManifest(f.a["coordinator"].id, a, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("manifest did not heal")
	}
	a.Actual++
	rejectChunkAction(t, f.s, f.a["coordinator"], a, 3)
}
func TestMoeChunksOrderingValidationAndHealing(t *testing.T) {
	f, _, _ := chunkFixture(t)
	raw := bytes.Repeat([]byte{73}, ChunkPayloadBytes+29)
	a := manifestAction(raw)
	co := f.a["coordinator"]
	rejectChunkAction(t, f.s, co, chunkAction(raw, 0), 3)
	apply(t, f.s, co, 1, a, 3)
	rejectChunkAction(t, f.s, co, Action{Type: "submit_moe_finalize", HoldID: "moe-req"}, 3)
	bad := chunkAction(raw, 0)
	bad.ReceiptChunk = bytes.Repeat([]byte{74}, ChunkPayloadBytes)
	rejectChunkAction(t, f.s, co, bad, 3)
	if f.s.Holds["moe-req"].Moe.ChunkBitmap[0] != 0 {
		t.Fatal("wrong hash set bit")
	}
	bad = chunkAction(raw, 0)
	bad.ChunkIndex = 2
	rejectChunkAction(t, f.s, co, bad, 3)
	bad.ChunkIndex = -1
	rejectChunkAction(t, f.s, co, bad, 3)
	bad.ChunkIndex = 0
	bad.ReceiptChunk = make([]byte, ChunkPayloadBytes+1)
	rejectChunkAction(t, f.s, co, bad, 3)
	rejectChunkAction(t, f.s, f.a["payer"], chunkAction(raw, 0), 3)
	// Last chunk first, then duplicate; handler's entire state is unchanged.
	if err := f.s.submitMoeChunk(co.id, chunkAction(raw, 1), 3); err != nil {
		t.Fatal(err)
	}
	before := stateBytes(t, f.s)
	if err := f.s.submitMoeChunk(co.id, chunkAction(raw, 1), 3); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, stateBytes(t, f.s)) {
		t.Fatal("duplicate changed state")
	}
	if err := f.s.submitMoeChunk(co.id, chunkAction(raw, 0), 3); err != nil {
		t.Fatal(err)
	}
	got, err := f.s.moeReceipt(f.s.Holds["moe-req"])
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("reassembly: %v", err)
	}
	for i := 0; i < 2; i++ {
		info, err := os.Stat(filepath.Join(f.s.chunkSpoolRoot, "moe-req", string(rune('0'+i))))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("chunk permissions", err)
		}
	}
	// Corrupt/missing manifest fails against the consensus list hash.
	m, err := f.s.chunkSpool().GetManifest("moe-req")
	if err != nil {
		t.Fatal(err)
	}
	m.Hashes[0] = make([]byte, 32)
	if err = f.s.chunkSpool().PutManifest("moe-req", m); err != nil {
		t.Fatal(err)
	}
	rejectChunkAction(t, f.s, co, chunkAction(raw, 0), 3)
	if err = f.s.submitMoeManifest(co.id, a, 3); err != nil {
		t.Fatal(err)
	}
	f.s.Holds["moe-req"].ExpireHeight = 3
	rejectChunkAction(t, f.s, co, chunkAction(raw, 0), 3)
	rejectChunkAction(t, f.s, co, Action{Type: "submit_moe_finalize", HoldID: "moe-req"}, 3)
}
func TestMoeChunkFinalizeEquivalenceAndSpoolFailure(t *testing.T) {
	f, raw, a := chunkFixture(t)
	legacy, err := f.s.Clone()
	if err != nil {
		t.Fatal(err)
	}
	co := f.a["coordinator"]
	apply(t, legacy, co, 1, Action{Type: "submit_moe", HoldID: "moe-req", Actual: a.Actual, Receipt: raw}, 3)
	apply(t, f.s, co, 1, a, 3)
	apply(t, f.s, co, 2, chunkAction(raw, 0), 3)
	path := filepath.Join(f.s.chunkSpoolRoot, "moe-req", "0")
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	rejectChunkAction(t, f.s, co, Action{Type: "submit_moe_finalize", HoldID: "moe-req"}, 3)
	apply(t, f.s, co, 3, chunkAction(raw, 0), 3)
	apply(t, f.s, co, 4, Action{Type: "submit_moe_finalize", HoldID: "moe-req"}, 3)
	chunked := f.s.Holds["moe-req"]
	if len(chunked.Moe.Receipt) != 0 {
		t.Fatal("chunked payload retained in consensus")
	}
	left := copyMutable(reflect.ValueOf(chunked)).Interface().(*Hold)
	left.Moe.ChunkRoot = nil
	left.Moe.ChunkListHash = nil
	left.Moe.ChunkBitmap = nil
	left.Moe.ChunkTotal = 0
	left.Moe.ChunkBytes = 0
	left.Moe.ChunkActual = 0
	left.Moe.Receipt = raw
	if !reflect.DeepEqual(left, legacy.Holds["moe-req"]) {
		t.Fatal("legacy/finalize settlement fields differ")
	}
	got, err := legacy.moeReceipt(legacy.Holds["moe-req"])
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("legacy receipt helper")
	}
	got, err = f.s.moeReceipt(chunked)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("chunk receipt helper", err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	before := stateBytes(t, f.s)
	if _, err = f.s.moeReceipt(chunked); err == nil {
		t.Fatal("missing spool accepted")
	}
	if _, _, err = f.s.moeFaultSegments(chunked, &pb.AuditVerdict{}); err == nil {
		t.Fatal("missing spool verdict accepted")
	}
	if err = f.s.settleMoe(chunked, "accept", 4); err == nil {
		t.Fatal("missing spool settled")
	}
	if !bytes.Equal(before, stateBytes(t, f.s)) {
		t.Fatal("missing spool changed state")
	}
	// Missing data also fails closed on the signed audit transaction path.
	apply(t, f.s, co, 5, Action{Type: "assign_audit", HoldID: "moe-req"}, 4)
	h := f.s.Holds["moe-req"]
	auditor := f.a["auditor"]
	if auditor.id != h.Auditor {
		auditor = f.a["auditor2"]
	}
	rejectChunkAction(t, f.s, auditor, Action{Type: "audit", HoldID: h.ID}, 5)
}
func TestMoeChunkExpireRefundAndArchiveCleanup(t *testing.T) {
	f, _, _ := chunkFixture(t)
	raw := bytes.Repeat([]byte{1}, ChunkPayloadBytes+1)
	a := manifestAction(raw)
	co := f.a["coordinator"]
	payer := f.a["payer"]
	frozenBalance := f.s.Accounts[payer.id].Balance
	apply(t, f.s, co, 1, a, 3)
	apply(t, f.s, co, 2, chunkAction(raw, 1), 3)
	h := f.s.Holds["moe-req"]
	apply(t, f.s, co, 3, Action{Type: "expire_hold", HoldID: h.ID}, h.ExpireHeight)
	if f.s.Accounts[payer.id].Balance != frozenBalance+h.Amount || f.s.Holds[h.ID].Actual != 0 {
		t.Fatal("incomplete chunk hold did not refund in full")
	}
	if _, err := os.Stat(filepath.Join(f.s.chunkSpoolRoot, h.ID)); err != nil {
		t.Fatal("cleaned before archive")
	}
	f.s.pruneBlockHashes()
	f.s.archiveTerminalHolds()
	if _, err := os.Stat(filepath.Join(f.s.chunkSpoolRoot, h.ID)); err != nil {
		t.Fatal("speculative archive cleaned spool")
	}
	statePath := strings.TrimSuffix(f.s.chunkSpoolRoot, ".chunk-spool")
	if err := SaveState(statePath, f.s); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.s.chunkSpoolRoot, h.ID)); !os.IsNotExist(err) {
		t.Fatal("archived spool not removed")
	}
}
func TestMoeChunkSizeMatchesSegaudit(t *testing.T) {
	raw, err := os.ReadFile("../tunnel/audit_transfer.go")
	if err != nil {
		t.Fatal(err)
	}
	if ChunkPayloadBytes != 1048576 || !bytes.Contains(raw, []byte("const auditChunkBytes = 1 << 20")) {
		t.Fatal("chunk transport constants differ")
	}
}

// extendedReceipt retains the fixture's real signed prefill and extends its
// decode frame with new frame indices, freshly bound routing hashes, segment
// signatures and node-frame roots. No padding/unknown fields inflate it.
func extendedReceipt(t *testing.T, f *moeChainFixture, outputs uint32) *pb.PipelineExecutionReceipt {
	t.Helper()
	r := f.buildReceipt(t)
	base := proto.Clone(r).(*pb.PipelineExecutionReceipt)
	r.OutputTokens = outputs
	actors := map[string]actor{f.attn.id: f.attn, f.expB.id: f.expB, f.expC.id: f.expC}
	for frame := uint32(2); frame < outputs; frame++ {
		records := map[uint32]*pb.RoutingRecord{}
		for _, old := range base.Moe.RoutingRecords {
			if old.Frame != 1 {
				continue
			}
			v := proto.Clone(old).(*pb.RoutingRecord)
			v.Frame = frame
			records[v.Layer] = v
			r.Moe.RoutingRecords = append(r.Moe.RoutingRecords, v)
		}
		byNode := map[string][][]byte{}
		for _, old := range base.Moe.SegmentReceipts {
			if old.Frame != 1 {
				continue
			}
			v := proto.Clone(old).(*pb.SegmentReceipt)
			v.Frame = frame
			var err error
			if v.Kind == "attention" {
				v.RoutingRecordHash, err = core.MoeRoutingSetHash([]*pb.RoutingRecord{records[0], records[1]})
			} else {
				v.RoutingRecordHash, err = core.MoeRoutingRecordHash(records[v.Layer])
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = core.Sign(v, actors[v.NodeId].key); err != nil {
				t.Fatal(err)
			}
			hash, err := core.MoeSegmentReceiptHash(v)
			if err != nil {
				t.Fatal(err)
			}
			byNode[v.NodeId] = append(byNode[v.NodeId], hash)
			r.Moe.SegmentReceipts = append(r.Moe.SegmentReceipts, v)
		}
		for id, hashes := range byNode {
			v := &pb.NodeFrameReceipt{NodeId: id, RequestId: r.RequestId, Frame: frame, SegmentsRoot: core.MoeFrameRoot(hashes), SegmentCount: uint32(len(hashes))}
			if err := core.Sign(v, actors[id].key); err != nil {
				t.Fatal(err)
			}
			r.Moe.FrameReceipts = append(r.Moe.FrameReceipts, v)
		}
	}
	return r
}

func runMoeChunkDrill(t *testing.T) (*State, *Hold, []byte, map[int64][][]byte) {
	t.Helper()
	f := moeChainState(t)
	f.s.Version = 13
	statePath := filepath.Join(t.TempDir(), "state.json")
	f.s.SetChunkSpoolPath(statePath)
	for _, node := range []actor{f.attn, f.expB, f.expC, f.expB2, f.expC2} {
		f.s.Accounts[node.id].Stake = 1_000_000_000_000
	}
	const outputs = 2200
	r := extendedReceipt(t, f, outputs)
	raw, err := proto.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= 4_192_848 {
		t.Fatalf("drill receipt too small: %d", len(raw))
	}
	a := manifestAction(raw)
	t.Logf("drill signed receipt: %d bytes, %d chunks, %d segments", len(raw), a.ChunkTotal, len(r.Moe.SegmentReceipts))
	app := &Application{path: statePath, committed: f.s}
	history := map[int64][][]byte{}
	height := int64(2)
	ctx := context.Background()
	block := func(who actor, a Action, valid bool) {
		t.Helper()
		st := app.committed
		tx, err := Sign(st.Parameters.ChainID, who.key, st.Accounts[who.id].Nonce+1, a)
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256([]byte{byte(height)})
		res, err := app.FinalizeBlock(ctx, &abci.FinalizeBlockRequest{Height: height, Hash: hash[:], Txs: [][]byte{tx}})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.TxResults) != 1 || (res.TxResults[0].Code == 0) != valid {
			t.Fatalf("%s valid=%v: %+v", a.Type, valid, res.TxResults)
		}
		history[height] = [][]byte{tx}
		if _, err = app.Commit(ctx, &abci.CommitRequest{}); err != nil {
			t.Fatal(err)
		}
		height++
	}
	block(f.a["payer"], Action{Type: "freeze_moe", HoldID: "moe-req", Payer: f.a["payer"].id, Amount: 1_000_000, MoeInputTokens: 3, MoeMaxTokens: outputs, MoeAuthorizations: f.authorizations()}, true)
	// Compare settlement to the exact same signed receipt via legacy on a twin.
	legacy, err := app.committed.Clone()
	if err != nil {
		t.Fatal(err)
	}
	block(f.a["coordinator"], a, true)
	bad := chunkAction(raw, 0)
	bad.ReceiptChunk = bytes.Repeat([]byte{0xee}, len(bad.ReceiptChunk))
	block(f.a["coordinator"], bad, false)
	for i := a.ChunkTotal - 1; i >= 0; i-- {
		block(f.a["coordinator"], chunkAction(raw, i), true)
		if i == 1 {
			block(f.a["coordinator"], chunkAction(raw, i), true)
		}
	}
	finalizeHeight := height
	block(f.a["coordinator"], Action{Type: "submit_moe_finalize", HoldID: "moe-req"}, true)
	apply(t, legacy, f.a["coordinator"], legacy.Accounts[f.a["coordinator"].id].Nonce+1, Action{Type: "submit_moe", HoldID: "moe-req", Actual: a.Actual, Receipt: raw}, finalizeHeight)
	h := app.committed.Holds["moe-req"]
	if h == nil || h.State != "dispute" || h.ReceiptRef != legacy.Holds[h.ID].ReceiptRef {
		t.Fatal("drill did not enter legacy-equivalent dispute")
	}
	left := copyMutable(reflect.ValueOf(h)).Interface().(*Hold)
	left.Moe.ChunkRoot = nil
	left.Moe.ChunkListHash = nil
	left.Moe.ChunkBitmap = nil
	left.Moe.ChunkTotal = 0
	left.Moe.ChunkBytes = 0
	left.Moe.ChunkActual = 0
	left.Moe.Receipt = raw
	if !reflect.DeepEqual(left, legacy.Holds[h.ID]) {
		t.Fatal("large drill legacy/finalize derivation differs")
	}
	spool, err := app.committed.moeReceipt(h)
	if err != nil || !bytes.Equal(spool, raw) {
		t.Fatal("drill spool mismatch", err)
	}
	// Normal small legacy receipt on this same ABCI chain (distinct request ID).
	small := f.buildReceipt(t)
	small.RequestId = "moe-small"
	keys := map[string]actor{f.attn.id: f.attn, f.expB.id: f.expB, f.expC.id: f.expC}
	hashes := map[string]map[uint32][][]byte{}
	for _, seg := range small.Moe.SegmentReceipts {
		seg.RequestId = small.RequestId
		if err = core.Sign(seg, keys[seg.NodeId].key); err != nil {
			t.Fatal(err)
		}
		hash, _ := core.MoeSegmentReceiptHash(seg)
		if hashes[seg.NodeId] == nil {
			hashes[seg.NodeId] = map[uint32][][]byte{}
		}
		hashes[seg.NodeId][seg.Frame] = append(hashes[seg.NodeId][seg.Frame], hash)
	}
	for _, fr := range small.Moe.FrameReceipts {
		fr.RequestId = small.RequestId
		fr.SegmentsRoot = core.MoeFrameRoot(hashes[fr.NodeId][fr.Frame])
		if err = core.Sign(fr, keys[fr.NodeId].key); err != nil {
			t.Fatal(err)
		}
	}
	smallRaw, _ := proto.Marshal(small)
	block(f.a["payer"], Action{Type: "freeze_moe", HoldID: small.RequestId, Payer: f.a["payer"].id, Amount: 1_000_000, MoeInputTokens: 3, MoeMaxTokens: 2, MoeAuthorizations: f.authorizations()}, true)
	block(f.a["coordinator"], Action{Type: "submit_moe", HoldID: small.RequestId, Actual: 600_000, Receipt: smallRaw}, true)
	hs := app.committed.Holds[small.RequestId]
	ref, _ := core.PipelineReceiptRef(small)
	if hs == nil || hs.State != "dispute" || hs.Moe.ChunkTotal != 0 || !bytes.Equal(hs.Moe.Receipt, smallRaw) || hs.ReceiptRef != fmtHex(ref) {
		t.Fatal("same-chain small legacy regression")
	}
	return app.committed, app.committed.Holds["moe-req"], raw, history
}
func fmtHex(raw []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(raw)*2)
	for i, v := range raw {
		out[i*2] = digits[v>>4]
		out[i*2+1] = digits[v&15]
	}
	return string(out)
}
func TestMoeChunkLargeDrill(t *testing.T) { runMoeChunkDrill(t) }

func TestMoeChunkAuditSettlementAndTamper(t *testing.T) {
	f, raw, a := chunkFixture(t)
	co := f.a["coordinator"]
	apply(t, f.s, co, 1, a, 3)
	apply(t, f.s, co, 2, chunkAction(raw, 0), 3)
	apply(t, f.s, co, 3, Action{Type: "submit_moe_finalize", HoldID: "moe-req"}, 3)
	apply(t, f.s, co, 4, Action{Type: "assign_audit", HoldID: "moe-req"}, 4)
	// Fault attribution must still identify the signed owner via the spool.
	h := f.s.Holds["moe-req"]
	drawn, err := h.moeExpectedDraw()
	if err != nil {
		t.Fatal(err)
	}
	ref := drawn[0]
	keys, nodes, err := f.s.moeFaultSegments(h, &pb.AuditVerdict{SegmentDistances: []*pb.SegmentDistance{{Ref: ref, Distance: 5, BandKey: "moe/" + ref.Kind + "/cpu->cpu"}}})
	if err != nil || len(keys) != 1 || len(nodes) != 1 {
		t.Fatal("chunk fault attribution", err)
	}
	receipt := &pb.PipelineExecutionReceipt{}
	if err = proto.Unmarshal(raw, receipt); err != nil {
		t.Fatal(err)
	}
	owner := ""
	for _, seg := range receipt.Moe.SegmentReceipts {
		if seg.Kind == ref.Kind && seg.Frame == ref.Frame && seg.Layer == ref.Layer && (seg.Kind == "attention" || seg.Expert == ref.Expert) {
			owner = seg.NodeId
		}
	}
	if nodes[0] != owner {
		t.Fatal("chunk fault attributed to wrong owner")
	}
	f.audit(t, f.auditorActor(t), 1, 5, "accept", nil, nil)
	expected, err := ExpectedAuditReceipts(f.s.Holds[h.ID], "accept")
	if err != nil {
		t.Fatal(err)
	}
	apply(t, f.s, co, 5, Action{Type: "settle", HoldID: h.ID, Outcome: "accept", AuditReceipts: expected}, 6)
	if f.s.Holds[h.ID].State != "settled" {
		t.Fatal("chunk audit did not settle")
	}
	// Tampering before manifest cannot launder an invalid signed segment: full
	// legacy validation runs only at finalize and rejects without any mutation.
	f, _, _ = chunkFixture(t)
	r := f.buildReceipt(t)
	r.Moe.SegmentReceipts[0].OutputHash[0] ^= 1
	raw, err = proto.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	a = manifestAction(raw)
	apply(t, f.s, co, 1, a, 3)
	apply(t, f.s, co, 2, chunkAction(raw, 0), 3)
	rejectChunkAction(t, f.s, co, Action{Type: "submit_moe_finalize", HoldID: "moe-req"}, 3)
}
func TestMoeChunkMaximumMetadataBound(t *testing.T) {
	f, _, a := chunkFixture(t)
	before := len(stateBytes(t, f.s))
	a.ChunkBytes = MaxChunkBytes
	a.ChunkTotal = MaxChunkTotal
	a.ChunkHashes = make([][]byte, MaxChunkTotal)
	for i := range a.ChunkHashes {
		a.ChunkHashes[i] = bytes.Repeat([]byte{byte(i)}, 32)
	}
	if err := f.s.submitMoeManifest(f.a["coordinator"].id, a, 3); err != nil {
		t.Fatal(err)
	}
	delta := len(stateBytes(t, f.s)) - before
	t.Logf("maximum 256-chunk hold metadata delta: %d bytes", delta)
	if delta >= 1024 {
		t.Fatal("chunk metadata exceeds bound")
	}
}

func TestMoeChunkCorruptionAndFinalizeAuthorization(t *testing.T) {
	f, raw, a := chunkFixture(t)
	co := f.a["coordinator"]
	apply(t, f.s, co, 1, a, 3)
	apply(t, f.s, co, 2, chunkAction(raw, 0), 3)
	rejectChunkAction(t, f.s, f.a["payer"], Action{Type: "submit_moe_finalize", HoldID: "moe-req"}, 3)
	path := filepath.Join(f.s.chunkSpoolRoot, "moe-req", "0")
	corrupt := append([]byte(nil), raw...)
	corrupt[0] ^= 1
	if err := os.WriteFile(path, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	before := stateBytes(t, f.s)
	if _, err := f.s.moeReceipt(f.s.Holds["moe-req"]); err == nil {
		t.Fatal("corrupt spool receipt accepted")
	}
	rejectChunkAction(t, f.s, co, Action{Type: "submit_moe_finalize", HoldID: "moe-req"}, 3)
	if !bytes.Equal(before, stateBytes(t, f.s)) {
		t.Fatal("corrupt spool changed state")
	}
	apply(t, f.s, co, 3, chunkAction(raw, 0), 3)
	apply(t, f.s, co, 4, Action{Type: "submit_moe_finalize", HoldID: "moe-req"}, 3)
	// Even when all files parse and individual hashes are correct, a mismatched
	// whole-receipt anchor must fail (the independent trust anchor).
	h := f.s.Holds["moe-req"]
	h.Moe.ChunkRoot[0] ^= 1
	m, err := f.s.chunkSpool().GetManifest(h.ID)
	if err != nil {
		t.Fatal(err)
	}
	m.SHA256 = fmtHex(h.Moe.ChunkRoot)
	if err = f.s.chunkSpool().PutManifest(h.ID, m); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.moeReceipt(h); err == nil {
		t.Fatal("wrong whole-receipt root accepted")
	}
}
