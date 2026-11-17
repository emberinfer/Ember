package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "ember/core-go"
	pb "ember/proto"
	"ember/services/ledger"
	"ember/services/scheduler"
	"ember/services/tunnel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

// capBook records every freeze amount and pricing commitment so tests
// can assert the bound follows the per-request max_tokens.
type capBook struct {
	ledger.Backend
	amounts     []int64
	commitments [][]byte
	cancels     int
	submits     int
}

func (b *capBook) TokenAccount(string) (string, error) { return "consumer", nil }
func (b *capBook) Freeze(id, payer, node string, amount int64) error {
	b.amounts = append(b.amounts, amount)
	return nil
}
func (b *capBook) FreezePriced(id, payer, node string, amount int64, commitment []byte) error {
	b.commitments = append(b.commitments, append([]byte(nil), commitment...))
	return b.Freeze(id, payer, node, amount)
}
func (b *capBook) FreezePipelinePriced(string, string, int64, []ledger.StageAuthorization, uint32, []byte) error {
	return errors.New("unused")
}
func (b *capBook) Cancel(string) error { b.cancels++; return nil }
func (b *capBook) Submit(string, *pb.ExecutionReceipt, int64, time.Duration) error {
	b.submits++
	return nil
}

func (b *capBook) lastCommitment(t *testing.T) *pb.ExecutionCommitment {
	t.Helper()
	if len(b.commitments) == 0 {
		t.Fatal("no priced freeze recorded")
	}
	c := &pb.ExecutionCommitment{}
	if err := proto.Unmarshal(b.commitments[len(b.commitments)-1], c); err != nil {
		t.Fatal(err)
	}
	return c
}

// maxTokRig is one fake node behind a real tunnel relay. The node
// tokenizes any prompt to rig.inputTokens and signs a commitment whose
// MaxOutputTokens echoes the job's max_tokens, exactly like the real
// node agent. rig.receiptOut controls the output count its execution
// receipt claims.
type maxTokRig struct {
	gateway     *Gateway
	book        *capBook
	inputTokens uint32
	receiptOut  atomic.Int32
}

func setupMaxTokRig(t *testing.T, contextLimit int) *maxTokRig {
	t.Helper()
	rig := &maxTokRig{book: &capBook{}, inputTokens: 100}
	rig.receiptOut.Store(1)
	relay := tunnel.New()
	relay.Legacy = true
	relay.EvidenceLimit = func() (uint32, error) { return 512, nil }
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	pb.RegisterNodeTunnelServer(server, relay)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	node := core.NodeID(key.Public().(ed25519.PublicKey))
	connection, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })
	stream, err := pb.NewNodeTunnelClient(connection).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&pb.TunnelFrame{NodeId: node, Kind: "hello"}); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			frame, err := stream.Recv()
			if err != nil {
				return
			}
			send := func(kind string, payload []byte) {
				_ = stream.Send(&pb.TunnelFrame{NodeId: node, RequestId: frame.RequestId, AttemptId: frame.AttemptId, Kind: kind, Payload: payload})
			}
			if frame.Kind == "tokenize" {
				var j job
				if err := json.Unmarshal(frame.Payload, &j); err != nil {
					send("error", []byte("bad job"))
					continue
				}
				hash := sha256.Sum256([]byte("user: x\n"))
				sampling := sha256.Sum256([]byte(strconv.Itoa(j.MaxTokens)))
				commitment := &pb.ExecutionCommitment{RequestId: frame.RequestId, ChainId: "ember-mock", ProfileVer: "v1", InputCommitment: hash[:], SamplingParamsHash: sampling[:], InputTokens: rig.inputTokens, MaxOutputTokens: uint32(j.MaxTokens), TokenizerVersion: "fixture-tokenizer-v1"}
				_ = core.Sign(commitment, key)
				raw, _ := proto.Marshal(commitment)
				send("tokenization", raw)
				continue
			}
			send("token", []byte("x"))
			receipt := &pb.ExecutionReceipt{RequestId: frame.RequestId, NodeId: node, InputTokens: rig.inputTokens, OutputTokens: uint32(rig.receiptOut.Load())}
			raw, _ := proto.Marshal(receipt)
			send("receipt", raw)
		}
	}()
	for !relay.Online(node) {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	profile := &pb.ResourceProfile{NodeId: node, ChainId: "ember-mock", ExecutionProfileVer: "v1", ManifestRoot: []byte{1}}
	candidates := []scheduler.Candidate{{Profile: profile, Online: true, Stake: 100000, Reputation: 1}}
	rig.gateway = &Gateway{Ledger: rig.book, Tunnel: relay, Price: 1, PriceInput: 1, Profile: "v1", Manifest: []byte{1}, ContextLimit: contextLimit}
	rig.gateway.Candidates = func() []scheduler.Candidate { return candidates }
	return rig
}

func (rig *maxTokRig) post(t *testing.T, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest("POST", path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer <redacted>")
	response := httptest.NewRecorder()
	rig.gateway.ServeHTTP(response, request)
	return response
}

const maxTokBody = `{"model":"ember-mock","messages":[{"role":"user","content":"x"}],"max_tokens":128}`
const maxTokBodyUnset = `{"model":"ember-mock","messages":[{"role":"user","content":"x"}]}`

// A specified max_tokens drives the freeze: fee(100 in, 128 out) at
// prices 1/1 is 228, and the signed commitment carries the same 128.
func TestPerRequestMaxTokensFreeze(t *testing.T) {
	rig := setupMaxTokRig(t, 0)
	response := rig.post(t, "/v1/chat/completions", maxTokBody)
	if response.Code != 200 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	if len(rig.book.amounts) != 1 || rig.book.amounts[0] != 228 {
		t.Fatalf("freeze amounts=%v, want [228]", rig.book.amounts)
	}
	c := rig.book.lastCommitment(t)
	if c.InputTokens != 100 || c.MaxOutputTokens != 128 {
		t.Fatalf("commitment input=%d max_output=%d, want 100/128", c.InputTokens, c.MaxOutputTokens)
	}
	if rig.book.submits != 1 {
		t.Fatalf("submits=%d", rig.book.submits)
	}
}

// Without max_tokens the request behaves exactly as before: the 1024
// default caps the output and the freeze is input + 1024.
func TestDefaultMaxTokensFreeze(t *testing.T) {
	rig := setupMaxTokRig(t, 0)
	response := rig.post(t, "/v1/chat/completions", maxTokBodyUnset)
	if response.Code != 200 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	if len(rig.book.amounts) != 1 || rig.book.amounts[0] != 1124 {
		t.Fatalf("freeze amounts=%v, want [1124]", rig.book.amounts)
	}
	if c := rig.book.lastCommitment(t); c.MaxOutputTokens != DefaultMaxTokens {
		t.Fatalf("commitment max_output=%d, want default %d", c.MaxOutputTokens, DefaultMaxTokens)
	}
}

// The quote path quotes the same per-request bound.
func TestPerRequestMaxTokensQuote(t *testing.T) {
	rig := setupMaxTokRig(t, 0)
	response := rig.post(t, "/v1/quote", maxTokBody)
	if response.Code != 200 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	if !strings.Contains(response.Body.String(), `"max_output_tokens":128`) || !strings.Contains(response.Body.String(), `"freeze":228`) {
		t.Fatalf("quote body=%s", response.Body)
	}
}

// With a context limit configured, input + max_tokens beyond it is
// refused at admission (chat and quote) with the reason spelled out,
// before any freeze; exactly at the limit is admitted.
func TestContextLimitAdmission(t *testing.T) {
	rig := setupMaxTokRig(t, 150)
	response := rig.post(t, "/v1/chat/completions", maxTokBody) // 100 + 128 > 150
	if response.Code != 400 || !strings.Contains(response.Body.String(), "context limit") {
		t.Fatalf("chat status=%d body=%s", response.Code, response.Body)
	}
	if len(rig.book.amounts) != 0 {
		t.Fatalf("froze %v for an over-limit request", rig.book.amounts)
	}
	response = rig.post(t, "/v1/quote", maxTokBody)
	if response.Code != 400 || !strings.Contains(response.Body.String(), "context limit") {
		t.Fatalf("quote status=%d body=%s", response.Code, response.Body)
	}
	response = rig.post(t, "/v1/chat/completions", `{"model":"ember-mock","messages":[{"role":"user","content":"x"}],"max_tokens":50}`) // 100 + 50 == 150
	if response.Code != 200 {
		t.Fatalf("at-limit status=%d body=%s", response.Code, response.Body)
	}
	if len(rig.book.amounts) != 1 || rig.book.amounts[0] != 150 {
		t.Fatalf("freeze amounts=%v, want [150]", rig.book.amounts)
	}
}

// Validation: negative values are invalid; without a configured
// context limit the legacy 4096 cap applies; with one, max_tokens may
// range up to the model's window; 1 is always fine.
func TestMaxTokensValidation(t *testing.T) {
	rig := setupMaxTokRig(t, 0)
	response := rig.post(t, "/v1/chat/completions", `{"model":"ember-mock","messages":[{"role":"user","content":"x"}],"max_tokens":-3}`)
	if response.Code != 400 {
		t.Fatalf("negative status=%d", response.Code)
	}
	response = rig.post(t, "/v1/chat/completions", `{"model":"ember-mock","messages":[{"role":"user","content":"x"}],"max_tokens":4097}`)
	if response.Code != 400 || !strings.Contains(response.Body.String(), "limit of 4096 tokens") {
		t.Fatalf("legacy cap status=%d body=%s", response.Code, response.Body)
	}
	response = rig.post(t, "/v1/chat/completions", `{"model":"ember-mock","messages":[{"role":"user","content":"x"}],"max_tokens":1}`)
	if response.Code != 200 {
		t.Fatalf("max_tokens=1 status=%d body=%s", response.Code, response.Body)
	}
	rig = setupMaxTokRig(t, 32768)
	response = rig.post(t, "/v1/chat/completions", `{"model":"ember-mock","messages":[{"role":"user","content":"x"}],"max_tokens":5000}`)
	if response.Code != 200 {
		t.Fatalf("context-sized status=%d body=%s", response.Code, response.Body)
	}
	if len(rig.book.amounts) != 1 || rig.book.amounts[0] != 5100 {
		t.Fatalf("freeze amounts=%v, want [5100]", rig.book.amounts)
	}
}

// The cap binds execution too: a receipt claiming more output than
// the requested max_tokens is not submitted for settlement.
func TestReceiptAboveCapRejected(t *testing.T) {
	rig := setupMaxTokRig(t, 0)
	rig.receiptOut.Store(200)
	response := rig.post(t, "/v1/chat/completions", maxTokBody)
	if response.Code == 200 || rig.book.submits != 0 {
		t.Fatalf("status=%d submits=%d, want rejection without submit", response.Code, rig.book.submits)
	}
}

func TestCheckContextWindow(t *testing.T) {
	unconfigured := &Gateway{}
	if err := unconfigured.checkContext(1<<30, 1<<30); err != nil {
		t.Fatalf("unconfigured limit must admit: %v", err)
	}
	g := &Gateway{ContextLimit: 150}
	if err := g.checkContext(100, 50); err != nil {
		t.Fatalf("at limit must admit: %v", err)
	}
	if err := g.checkContext(100, 51); err == nil || !strings.Contains(err.Error(), "context limit") {
		t.Fatalf("over limit must refuse with reason, got %v", err)
	}
}

func TestContextLimitExposure(t *testing.T) {
	rig := setupMaxTokRig(t, 228)
	w := httptest.NewRecorder()
	rig.gateway.ServeHTTP(w, httptest.NewRequest("GET", "/v1/pricing", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"context_limit_tokens":228`) {
		t.Fatalf("pricing status=%d body=%s", w.Code, w.Body)
	}
	w = rig.post(t, "/v1/quote", maxTokBody)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"context_limit_tokens":228`) {
		t.Fatalf("quote status=%d body=%s", w.Code, w.Body)
	}
}
