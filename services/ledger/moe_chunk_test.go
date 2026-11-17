package ledger

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	pb "ember/proto"
	ch "ember/services/chain"
	abci "github.com/cometbft/cometbft/abci/types"
	cmtjson "github.com/cometbft/cometbft/libs/json"
	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/cometbft/cometbft/types"
	"google.golang.org/protobuf/proto"
)

func chunkTestRef(raw []byte) string {
	h := sha256.New()
	h.Write([]byte("ember/pipeline-receipt/v1\x00"))
	h.Write(raw)
	return hex.EncodeToString(h.Sum(nil))
}

func TestMoeChunksResumeAndFinalizeConfirmation(t *testing.T) {
	raw := bytes.Repeat([]byte{37}, 2*ch.ChunkPayloadBytes+17)
	h := &ch.Hold{State: "frozen", Moe: &ch.MoeHold{}}
	var indices []int
	manifests, queries := 0, 0
	fail := true
	send := func(a ch.Action) error {
		switch a.Type {
		case "submit_moe_manifest":
			manifests++
			if h.Moe.ChunkTotal == 0 {
				list := sha256.New()
				for _, v := range a.ChunkHashes {
					list.Write(v)
				}
				h.Moe.ChunkRoot = a.ChunkRoot
				h.Moe.ChunkListHash = list.Sum(nil)
				h.Moe.ChunkBytes = a.ChunkBytes
				h.Moe.ChunkTotal = a.ChunkTotal
				h.Moe.ChunkActual = a.Actual
				h.Moe.ChunkBitmap = make([]byte, (a.ChunkTotal+7)/8)
			}
		case "submit_moe_chunk":
			indices = append(indices, a.ChunkIndex)
			if a.ChunkIndex == 1 && fail {
				fail = false
				return errors.New("interrupted before inclusion")
			}
			end := min((a.ChunkIndex+1)*ch.ChunkPayloadBytes, len(raw))
			if !bytes.Equal(a.ReceiptChunk, raw[a.ChunkIndex*ch.ChunkPayloadBytes:end]) {
				t.Fatal("wrong chunk payload")
			}
			h.Moe.ChunkBitmap[a.ChunkIndex/8] |= 1 << uint(a.ChunkIndex%8)
		case "submit_moe_finalize":
			h.State = "dispute"
			h.ReceiptRef = chunkTestRef(raw)
			h.Moe.Credits = []StageShare{{NodeID: "node", Amount: 9}}
			return errors.New("lost final response") // Reconcile by hold, not this error.
		default:
			t.Fatal(a.Type)
		}
		return nil
	}
	hold := func(string) (*ch.Hold, error) { queries++; return h, nil }
	if _, err := sendMoeChunks("hold", raw, 9, send, hold); err == nil {
		t.Fatal("interruption ignored")
	}
	if !reflect.DeepEqual(indices, []int{0, 1}) {
		t.Fatal(indices)
	}
	indices = nil
	shares, err := sendMoeChunks("hold", raw, 9, send, hold)
	if err != nil || len(shares) != 1 || shares[0].Amount != 9 {
		t.Fatal(shares, err)
	}
	if !reflect.DeepEqual(indices, []int{1, 2}) || manifests != 2 || queries != 8 {
		t.Fatal(indices, manifests, queries)
	}
	indices = nil
	if _, err = sendMoeChunks("hold", raw, 9, func(ch.Action) error { t.Fatal("finalized replay broadcast"); return nil }, hold); err != nil {
		t.Fatal(err)
	}
	changed := append([]byte(nil), raw...)
	changed[0]++
	if _, err = sendMoeChunks("hold", changed, 9, func(ch.Action) error { t.Fatal("conflicting replay broadcast"); return nil }, hold); err == nil {
		t.Fatal("accepted different receipt")
	}
}

func TestMoeChunksRequireHoldConfirmation(t *testing.T) {
	raw := []byte("receipt")
	root := sha256.Sum256(raw)
	list := sha256.Sum256(root[:])
	h := &ch.Hold{State: "frozen", Moe: &ch.MoeHold{ChunkRoot: root[:], ChunkListHash: list[:], ChunkBytes: len(raw), ChunkTotal: 1, ChunkActual: 1, ChunkBitmap: []byte{1}}}
	sent := []string{}
	_, err := sendMoeChunks("hold", raw, 1, func(a ch.Action) error { sent = append(sent, a.Type); return nil }, func(string) (*ch.Hold, error) { return h, nil })
	if err == nil || !strings.Contains(err.Error(), "did not enter dispute") || !reflect.DeepEqual(sent, []string{"submit_moe_manifest", "submit_moe_finalize"}) {
		t.Fatal(sent, err)
	}
	h.Moe.ChunkActual = 2
	if _, err = sendMoeChunks("hold", raw, 1, func(ch.Action) error { t.Fatal("conflict broadcast"); return nil }, func(string) (*ch.Hold, error) { return h, nil }); err == nil {
		t.Fatal("manifest conflict accepted")
	}
	if _, err = sendMoeChunks("hold", nil, 1, func(ch.Action) error { t.Fatal("empty broadcast"); return nil }, func(string) (*ch.Hold, error) { t.Fatal("empty queried"); return nil, nil }); err == nil {
		t.Fatal("empty accepted")
	}
}

type chunkRPCStub struct {
	broadcasts, polls int
	check, deliver    uint32
	transport         error
	delayed           int
	deadline          time.Duration
	hash              []byte
}

func (s *chunkRPCStub) BroadcastTxCommit(ctx context.Context, _ types.Tx) (*coretypes.ResultBroadcastTxCommit, error) {
	s.broadcasts++
	deadline, _ := ctx.Deadline()
	s.deadline = time.Until(deadline)
	if s.transport != nil {
		return nil, s.transport
	}
	return &coretypes.ResultBroadcastTxCommit{CheckTx: abci.CheckTxResponse{Code: s.check, Log: "check failed"}, TxResult: abci.ExecTxResult{Code: s.deliver, Log: "deliver failed"}}, nil
}
func (s *chunkRPCStub) Tx(_ context.Context, hash []byte, _ bool) (*coretypes.ResultTx, error) {
	s.polls++
	s.hash = append([]byte(nil), hash...)
	if s.polls <= s.delayed {
		return nil, errors.New("not found")
	}
	return &coretypes.ResultTx{Height: 1, Hash: hash, TxResult: abci.ExecTxResult{Code: s.deliver, Log: "deliver failed"}}, nil
}
func TestMoeChunkBroadcastCommitAndPoll(t *testing.T) {
	for _, tc := range []struct {
		name    string
		s       chunkRPCStub
		wantErr bool
		polls   int
	}{
		{"confirmed", chunkRPCStub{}, false, 1},
		{"transport after inclusion", chunkRPCStub{transport: errors.New("EOF"), delayed: 2}, false, 3},
		{"check rejected", chunkRPCStub{check: 1}, true, 0},
		{"deliver rejected", chunkRPCStub{deliver: 1}, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte("signed tx")
			hash := sha256.Sum256(raw)
			_, err := broadcastMoeChunk(context.Background(), &tc.s, raw, 60*time.Second, time.Second, time.Millisecond)
			if (err != nil) != tc.wantErr || tc.s.polls != tc.polls || tc.s.broadcasts != 1 {
				t.Fatal(err, tc.s)
			}
			if tc.s.deadline < 59*time.Second || tc.s.deadline > 60*time.Second {
				t.Fatal(tc.s.deadline)
			}
			if tc.polls > 0 && !bytes.Equal(tc.s.hash, hash[:]) {
				t.Fatal("polled another hash")
			}
		})
	}
	s := &chunkRPCStub{delayed: 10000}
	raw := []byte("unconfirmed")
	_, err := broadcastMoeChunk(context.Background(), s, raw, time.Second, 5*time.Millisecond, time.Millisecond)
	var unknown *UnconfirmedError
	if !errors.As(err, &unknown) || unknown.Hash != sha256.Sum256(raw) || s.broadcasts != 1 {
		t.Fatal(err, s)
	}
}

func TestSubmitMoeSmallLegacyAction(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{23}, 32))
	receipt := &pb.PipelineExecutionReceipt{RequestId: "hold", Moe: &pb.MoeExecutionData{}}
	raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(receipt)
	var signed []byte
	broadcasts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Path string `json:"path"`
				Tx   []byte `json:"tx"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		var result any
		switch req.Method {
		case "abci_query":
			var value []byte
			if strings.HasPrefix(req.Params.Path, "/account/") {
				value, _ = json.Marshal(&ch.Account{Nonce: 3})
			} else if req.Params.Path == "/hold/hold" {
				value, _ = json.Marshal(&ch.Hold{Moe: &ch.MoeHold{Credits: []StageShare{{NodeID: "node", Amount: 4}}}})
			} else {
				t.Error(req.Params.Path)
			}
			result = &coretypes.ResultABCIQuery{Response: abci.QueryResponse{Value: value}}
		case "broadcast_tx_sync":
			broadcasts++
			signed = req.Params.Tx
			env, _, err := ch.DecodeAndVerifyVersion(signed, 13)
			if err != nil || env.Nonce != 4 || env.Action.Type != "submit_moe" || env.Action.HoldID != "hold" || env.Action.Actual != 4 || !bytes.Equal(env.Action.Receipt, raw) || env.Action.ChunkTotal != 0 {
				t.Error(env, err)
			}
			hash := sha256.Sum256(signed)
			result = &coretypes.ResultBroadcastTx{Hash: hash[:]}
		case "tx":
			hash := sha256.Sum256(signed)
			result = &coretypes.ResultTx{Hash: hash[:], Height: 1, Tx: signed}
		default:
			t.Error("unexpected method", req.Method)
			http.Error(w, "unexpected", 500)
			return
		}
		encoded, err := cmtjson.Marshal(result)
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, encoded)
	}))
	defer server.Close()
	rpc, err := rpchttp.New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	l := &ChainLedger{rpc: rpc, chainID: "chain", coordinator: key}
	credits, err := l.SubmitMoe("hold", receipt, 4)
	if err != nil || len(credits) != 1 || credits[0].Amount != 4 || broadcasts != 1 {
		t.Fatal(credits, err, broadcasts)
	}
}

// Exercise the public entry at the exact transport boundary, including the
// chunk-specific signing/broadcast path rather than only its orchestration.
func TestSubmitMoeTransportThreshold(t *testing.T) {
	for _, size := range []int{ch.ChunkPayloadBytes, ch.ChunkPayloadBytes + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			r := &pb.PipelineExecutionReceipt{RequestId: "hold", Moe: &pb.MoeExecutionData{}, OutputCommitment: make([]byte, size-20)}
			var raw []byte
			for n := 0; n < 4; n++ {
				raw, _ = proto.MarshalOptions{Deterministic: true}.Marshal(r)
				if len(raw) == size {
					break
				}
				r.OutputCommitment = make([]byte, len(r.OutputCommitment)+size-len(raw))
			}
			if len(raw) != size {
				t.Fatal("fixture size", len(raw))
			}
			key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{24}, 32))
			h := &ch.Hold{State: "frozen", Moe: &ch.MoeHold{}}
			var nonce uint64
			var signed []byte
			var actions []string
			methods := []string{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, reqHTTP *http.Request) {
				var req struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
					Params struct {
						Path string `json:"path"`
						Tx   []byte `json:"tx"`
					} `json:"params"`
				}
				if err := json.NewDecoder(reqHTTP.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				var result any
				switch req.Method {
				case "abci_query":
					var value []byte
					if strings.HasPrefix(req.Params.Path, "/account/") {
						value, _ = json.Marshal(&ch.Account{Nonce: nonce})
					} else if req.Params.Path == "/hold/hold" {
						value, _ = json.Marshal(h)
					} else {
						t.Error(req.Params.Path)
					}
					result = &coretypes.ResultABCIQuery{Response: abci.QueryResponse{Value: value}}
				case "broadcast_tx_sync", "broadcast_tx_commit":
					methods = append(methods, req.Method)
					signed = req.Params.Tx
					env, _, err := ch.DecodeAndVerifyVersion(signed, 13)
					if err != nil || env.Nonce != nonce+1 {
						t.Error("bad signed action", env.Nonce, nonce, err)
					}
					nonce = env.Nonce
					a := env.Action
					actions = append(actions, a.Type)
					switch a.Type {
					case "submit_moe":
						if !bytes.Equal(raw, a.Receipt) {
							t.Error("legacy bytes changed")
						}
						h.State = "dispute"
						h.ReceiptRef = chunkTestRef(raw)
					case "submit_moe_manifest":
						hash := sha256.New()
						for _, v := range a.ChunkHashes {
							hash.Write(v)
						}
						h.Moe.ChunkRoot = a.ChunkRoot
						h.Moe.ChunkListHash = hash.Sum(nil)
						h.Moe.ChunkBytes = a.ChunkBytes
						h.Moe.ChunkTotal = a.ChunkTotal
						h.Moe.ChunkActual = a.Actual
						h.Moe.ChunkBitmap = make([]byte, (a.ChunkTotal+7)/8)
					case "submit_moe_chunk":
						start := a.ChunkIndex * ch.ChunkPayloadBytes
						end := min(start+ch.ChunkPayloadBytes, len(raw))
						if !bytes.Equal(raw[start:end], a.ReceiptChunk) {
							t.Error("chunk bytes changed")
						}
						h.Moe.ChunkBitmap[a.ChunkIndex/8] |= 1 << uint(a.ChunkIndex%8)
					case "submit_moe_finalize":
						h.State = "dispute"
						h.ReceiptRef = chunkTestRef(raw)
					default:
						t.Error(a.Type)
					}
					hash := sha256.Sum256(signed)
					if req.Method == "broadcast_tx_sync" {
						result = &coretypes.ResultBroadcastTx{Hash: hash[:]}
					} else {
						result = &coretypes.ResultBroadcastTxCommit{Hash: hash[:], Height: 1}
					}
				case "tx":
					hash := sha256.Sum256(signed)
					result = &coretypes.ResultTx{Hash: hash[:], Height: 1, Tx: signed}
				default:
					t.Error(req.Method)
					http.Error(w, "unexpected", 500)
					return
				}
				encoded, err := cmtjson.Marshal(result)
				if err != nil {
					t.Error(err)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, encoded)
			}))
			defer server.Close()
			rpc, err := rpchttp.New(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			l := &ChainLedger{rpc: rpc, chainID: "chain", coordinator: key}
			if _, err = l.SubmitMoe("hold", r, 4); err != nil {
				t.Fatal(err)
			}
			if size == ch.ChunkPayloadBytes {
				if !reflect.DeepEqual(actions, []string{"submit_moe"}) || !reflect.DeepEqual(methods, []string{"broadcast_tx_sync"}) {
					t.Fatal(actions, methods)
				}
			} else {
				if !reflect.DeepEqual(actions, []string{"submit_moe_manifest", "submit_moe_chunk", "submit_moe_chunk", "submit_moe_finalize"}) {
					t.Fatal(actions)
				}
				for _, method := range methods {
					if method != "broadcast_tx_commit" {
						t.Fatal(method)
					}
				}
			}
		})
	}
}
