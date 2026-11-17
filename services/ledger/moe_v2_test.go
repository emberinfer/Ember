package ledger

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	core "ember/core-go"
	pb "ember/proto"
	chain "ember/services/chain"
	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	"google.golang.org/protobuf/proto"
)

// A compact finalized receipt must reconcile by its logical v1 ReceiptRef,
// while its manifest and archive are pinned to the compact wire bytes.
func TestMoeV2FinalizedResumeUsesLogicalReceiptRef(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{17}, 32))
	seg := &pb.SegmentReceipt{NodeId: chain.AccountID(key.Public().(ed25519.PublicKey)), ChainId: "chain", ManifestRoot: bytes.Repeat([]byte{19}, 32), ExecutionProfileVer: "profile", RequestId: "hold", Kind: "expert", InputHash: bytes.Repeat([]byte{1}, 32), OutputHash: bytes.Repeat([]byte{2}, 32)}
	if err := core.Sign(seg, key); err != nil {
		t.Fatal(err)
	}
	hash, err := core.MoeSegmentReceiptHash(seg)
	if err != nil {
		t.Fatal(err)
	}
	frame := &pb.NodeFrameReceipt{NodeId: seg.NodeId, RequestId: "hold", SegmentCount: 1, SegmentsRoot: core.MoeFrameRoot([][]byte{hash})}
	if err = core.Sign(frame, key); err != nil {
		t.Fatal(err)
	}
	logical := &pb.PipelineExecutionReceipt{RequestId: "hold", InputTokens: 1, OutputTokens: 1, ElapsedMs: 1, Moe: &pb.MoeExecutionData{SegmentReceipts: []*pb.SegmentReceipt{seg}, FrameReceipts: []*pb.NodeFrameReceipt{frame}}}
	compact, err := core.CompactMoeReceipt(logical)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(compact)
	if err != nil {
		t.Fatal(err)
	}
	root := sha256.Sum256(raw)
	list := sha256.Sum256(root[:])
	ref, err := core.PipelineReceiptRef(logical)
	if err != nil {
		t.Fatal(err)
	}
	if chunkTestRef(raw) == hex.EncodeToString(ref) {
		t.Fatal("fixture does not distinguish wire and logical anchors")
	}
	h := &chain.Hold{ID: "hold", State: "dispute", ReceiptRef: hex.EncodeToString(ref), Moe: &chain.MoeHold{ChunkRoot: root[:], ChunkListHash: list[:], ChunkTotal: 1, ChunkBytes: len(raw), ChunkActual: 9, ChunkBitmap: []byte{1}, Credits: []StageShare{{NodeID: seg.NodeId, Amount: 9}}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Path string `json:"path"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
			return
		}
		if q.Method != "abci_query" || q.Params.Path != "/hold/hold" {
			t.Errorf("finalized compact receipt broadcast or unexpected query: %+v", q)
			http.Error(w, "unexpected", 500)
			return
		}
		value, _ := json.Marshal(h)
		reply, _ := json.Marshal(map[string]any{"response": map[string]any{"value": value, "code": 0}})
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, q.ID, reply)
	}))
	defer server.Close()
	rpc, err := rpchttp.New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	l := &ChainLedger{rpc: rpc}
	shares, err := l.ChunkedSubmitMoe("hold", compact, 9)
	if err != nil || len(shares) != 1 || shares[0].Amount != 9 {
		t.Fatal(shares, err)
	}
}
