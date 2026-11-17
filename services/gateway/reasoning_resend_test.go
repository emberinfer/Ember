package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	core "ember/core-go"
	pb "ember/proto"
	"ember/services/evidence"
	"ember/services/ledger"
	"ember/services/scheduler"
	"ember/services/tunnel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

// Scripted node for the thinking-truncation fallback tests: each "run"
// job is answered by the next scripted receipt shape.
type scriptedRun struct {
	tokens   []string
	category string
	reason   uint32
	content  uint32
}

// pricedTestLedger makes the plain ledger satisfy the gateway's
// priced path (ConsumerPricing + QuotedFreezer) at a flat 1x rate, so
// thinking requests keep their signed commitment through freeze.
type pricedTestLedger struct {
	ledger.Backend
}

func (b *pricedTestLedger) QuoteFee(_ string, input, output uint32) (core.FeeComponents, int64, int64, error) {
	c, err := core.TokenFee(&pb.ChainGenesis{PriceInput: 1, PriceOutput: 1}, input, output)
	return c, 1000000, 0, err
}

func (b *pricedTestLedger) HoldFee(_ string, input, output uint32) (core.FeeComponents, error) {
	return core.TokenFee(&pb.ChainGenesis{PriceInput: 1, PriceOutput: 1}, input, output)
}

func (b *pricedTestLedger) FreezeQuoted(id, payer, node string, amount int64, _ []byte, _, _ int64) error {
	return b.Backend.Freeze(id, payer, node, amount)
}

func (b *pricedTestLedger) FreezePipelineQuoted(id, payer string, amount int64, _ []ledger.StageAuthorization, _ uint32, _ []byte, _, _ int64) error {
	return b.Backend.Freeze(id, payer, "", amount)
}

type resendFixture struct {
	gateway  *Gateway
	book     *ledger.Ledger
	jobs     []job
	receipts []*pb.ExecutionReceipt
	mu       sync.Mutex
	done     chan error
}

func startResendFixture(t *testing.T, script []scriptedRun) *resendFixture {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	ident := core.NodeID(key.Public().(ed25519.PublicKey))
	book, err := ledger.Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	token := strings.Repeat("t", 32)
	if err = book.IssueToken("consumer", 100000, token); err != nil {
		t.Fatal(err)
	}
	if err = book.Create(ident, 100000); err != nil {
		t.Fatal(err)
	}
	if err = book.Stake(ident, 50000); err != nil {
		t.Fatal(err)
	}
	relay := tunnel.New()
	relay.Legacy = true
	relay.Evidence = &evidence.Store{Dir: t.TempDir()}
	relay.EvidenceLimit = func() (uint32, error) { return 512, nil }
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	pb.RegisterNodeTunnelServer(server, relay)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	connection, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	stream, err := pb.NewNodeTunnelClient(connection).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = stream.Send(&pb.TunnelFrame{NodeId: ident, Kind: "hello"}); err != nil {
		t.Fatal(err)
	}
	fx := &resendFixture{book: book, done: make(chan error, 1)}
	go func() {
		runs := 0
		for {
			frame, err := stream.Recv()
			if err != nil {
				fx.done <- err
				return
			}
			send := func(kind string, payload []byte) error {
				return stream.Send(&pb.TunnelFrame{NodeId: ident, RequestId: frame.RequestId, AttemptId: frame.AttemptId, Kind: kind, Payload: payload})
			}
			switch frame.Kind {
			case "tokenize":
				var j job
				if err := json.Unmarshal(frame.Payload, &j); err != nil {
					fx.done <- err
					return
				}
				q := request{ThinkingEnabled: j.ThinkingEnabled, ThinkingBudget: j.ThinkingBudget, AnswerBudget: j.AnswerBudget, MaxTokens: j.MaxTokens, Seed: j.Seed, Model: j.Chain}
				hash := sha256.Sum256([]byte(j.Prompt))
				sampling := requestSamplingHash(q)
				c := &pb.ExecutionCommitment{RequestId: frame.RequestId, ChainId: j.Chain, ProfileVer: j.Profile, InputCommitment: hash[:], SamplingParamsHash: sampling[:], Seed: j.Seed, InputTokens: 1, MaxOutputTokens: uint32(j.MaxTokens), TokenizerVersion: "fixture-tokenizer-v1"}
				setReasoningCommitment(c, q)
				core.Sign(c, key)
				wire, _ := proto.Marshal(c)
				if err := send("tokenization", wire); err != nil {
					fx.done <- err
					return
				}
			case "run":
				var j job
				if err := json.Unmarshal(frame.Payload, &j); err != nil {
					fx.done <- err
					return
				}
				fx.mu.Lock()
				fx.jobs = append(fx.jobs, j)
				fx.mu.Unlock()
				if runs >= len(script) {
					fx.done <- errUnexpectedRun
					return
				}
				shape := script[runs]
				runs++
				for _, tok := range shape.tokens {
					if err := send("token", []byte(tok)); err != nil {
						fx.done <- err
						return
					}
				}
				openings, _ := json.Marshal(make([]int, len(shape.tokens)))
				if err := send("evidence", openings); err != nil {
					fx.done <- err
					return
				}
				r := &pb.ExecutionReceipt{RequestId: frame.RequestId, NodeId: ident, InputTokens: 1,
					OutputTokens: uint32(len(shape.tokens)), ReasoningTokens: shape.reason, ContentTokens: shape.content,
					OutputCategory: shape.category}
				rootHash := sha256.Sum256([]byte("fixture-root"))
				var previous []byte
				for i := range shape.tokens {
					root := &pb.ChunkRoot{RequestId: frame.RequestId, Seq: uint32(i), UptoStep: uint32(i + 1), MerkleRoot: rootHash[:], PrevRoot: previous}
					core.Sign(root, key)
					r.ChunkRoots = append(r.ChunkRoots, root)
					previous, _ = core.Digest(root)
				}
				core.Sign(r, key)
				wire, _ := proto.Marshal(r)
				if err := send("receipt", wire); err != nil {
					fx.done <- err
					return
				}
			default:
				// hello/ready/challenge frames need no answer.
			}
		}
	}()
	for !relay.Online(ident) {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	profile := &pb.ResourceProfile{NodeId: ident, ChainId: "ember-mock", ExecutionProfileVer: "v1", ManifestRoot: []byte{1}, MemoryBytes: 1}
	core.Sign(profile, key)
	fx.gateway = &Gateway{Ledger: &pricedTestLedger{Backend: book}, Tunnel: relay, Price: 1, Profile: "v1", Manifest: []byte{1},
		Candidates: func() []scheduler.Candidate {
			return []scheduler.Candidate{{Profile: profile, Online: true, Stake: 50000}}
		},
		OnReceipt: func(r *pb.ExecutionReceipt, _ []byte, _ string, _ int64, _ int64) {
			fx.mu.Lock()
			fx.receipts = append(fx.receipts, r)
			fx.mu.Unlock()
		},
	}
	return fx
}

var errUnexpectedRun = errorString("unexpected extra run dispatch")

type errorString string

func (e errorString) Error() string { return string(e) }

func (fx *resendFixture) chat(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	token := strings.Repeat("t", 32)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	fx.gateway.ServeHTTP(response, request)
	var decoded map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("response not JSON: %d %s", response.Code, response.Body)
	}
	return response.Code, decoded
}

// A thinking request whose budget burns entirely inside the think
// segment (receipt marked thinking-truncated) must trigger exactly one
// independent fallback: thinking off, full total budget as the answer
// budget, its own hold/commitment/settlement. Both receipts stay
// visible; the answer comes from the fallback.
func TestGatewayThinkingTruncatedFallbackResend(t *testing.T) {
	fx := startResendFixture(t, []scriptedRun{
		{tokens: []string{"t", "t", "t", "t"}, category: "thinking-truncated", reason: 4, content: 0},
		{tokens: []string{"o", "k", "!"}, category: "", reason: 0, content: 3},
	})
	code, resp := fx.chat(t, `{"model":"ember-mock","messages":[{"role":"user","content":"hard question"}],"max_tokens":64,"thinking_enabled":true,"thinking_budget":48,"answer_budget":16}`)
	if code != 200 {
		t.Fatalf("chat failed: %d %v", code, resp)
	}
	if len(fx.jobs) != 2 {
		t.Fatalf("expected exactly 2 dispatched jobs (original + one fallback), got %d", len(fx.jobs))
	}
	first, second := fx.jobs[0], fx.jobs[1]
	if first.ThinkingEnabled == nil || !*first.ThinkingEnabled || first.ThinkingBudget != 48 || first.AnswerBudget != 16 {
		t.Fatalf("first job lost its thinking commitment: %+v", first)
	}
	if second.ThinkingEnabled == nil || *second.ThinkingEnabled || second.ThinkingBudget != 0 || second.AnswerBudget != 64 {
		t.Fatalf("fallback job is not thinking-off with the full total budget: %+v", second)
	}
	if len(fx.receipts) != 2 {
		t.Fatalf("expected 2 settled receipts, got %d", len(fx.receipts))
	}
	if fx.receipts[0].OutputCategory != "thinking-truncated" || fx.receipts[1].OutputCategory != "" {
		t.Fatalf("receipt markers wrong: %q / %q", fx.receipts[0].OutputCategory, fx.receipts[1].OutputCategory)
	}
	if fx.receipts[0].RequestId == fx.receipts[1].RequestId {
		t.Fatal("fallback reused the first request id (requests must be independent)")
	}
	for _, r := range fx.receipts {
		if state, err := fx.book.State(r.RequestId); err != nil || state != "dispute" {
			t.Fatalf("receipt %s not settled independently: state=%q err=%v", r.RequestId, state, err)
		}
	}
	choices := resp["choices"].([]any)
	message := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "ok!" {
		t.Fatalf("response content is not the fallback output: %v", message["content"])
	}
	firstID, _ := resp["ember_first_request_id"].(string)
	if resp["ember_fallback_resend"] != true || firstID == "" || firstID == resp["id"] ||
		!strings.HasPrefix(fx.receipts[0].RequestId, firstID+"-") {
		t.Fatalf("response does not mark the fallback resend: %v", resp)
	}
	usage := resp["usage"].(map[string]any)
	details := usage["completion_tokens_details"].(map[string]any)
	if details["reasoning_tokens"] != float64(0) || usage["reasoning_tokens"] != float64(0) || usage["content_tokens"] != float64(3) {
		t.Fatalf("usage three-way split wrong: %v", usage)
	}
}

// If the fallback also truncates, it is presented honestly
// (finish_reason length) and NO second resend happens.
func TestGatewayFallbackTruncatedAgainIsNotResent(t *testing.T) {
	fx := startResendFixture(t, []scriptedRun{
		{tokens: []string{"t", "t"}, category: "thinking-truncated", reason: 2, content: 0},
		{tokens: []string{"u", "u"}, category: "thinking-truncated", reason: 2, content: 0},
	})
	code, resp := fx.chat(t, `{"model":"ember-mock","messages":[{"role":"user","content":"hard question"}],"max_tokens":64,"thinking_enabled":true,"thinking_budget":48,"answer_budget":16}`)
	if code != 200 {
		t.Fatalf("chat failed: %d %v", code, resp)
	}
	if len(fx.jobs) != 2 {
		t.Fatalf("fallback looped: %d jobs dispatched", len(fx.jobs))
	}
	choices := resp["choices"].([]any)
	if choices[0].(map[string]any)["finish_reason"] != "length" {
		t.Fatalf("second truncation not presented as length: %v", choices[0])
	}
}

// A normal thinking-on response carries the nested OpenAI-shaped
// reasoning token detail alongside the flat fields.
func TestGatewayUsageReasoningDetails(t *testing.T) {
	fx := startResendFixture(t, []scriptedRun{
		{tokens: []string{"a", "b", "c", "d", "e"}, category: "", reason: 2, content: 3},
	})
	code, resp := fx.chat(t, `{"model":"ember-mock","messages":[{"role":"user","content":"q"}],"max_tokens":64,"thinking_enabled":true,"thinking_budget":48,"answer_budget":16}`)
	if code != 200 {
		t.Fatalf("chat failed: %d %v", code, resp)
	}
	if len(fx.jobs) != 1 {
		t.Fatalf("unexpected resend on a clean receipt: %d jobs", len(fx.jobs))
	}
	usage := resp["usage"].(map[string]any)
	details, ok := usage["completion_tokens_details"].(map[string]any)
	if !ok || details["reasoning_tokens"] != float64(2) {
		t.Fatalf("missing nested reasoning_tokens detail: %v", usage)
	}
	if usage["completion_tokens"] != float64(5) || usage["content_tokens"] != float64(3) {
		t.Fatalf("usage split wrong: %v", usage)
	}
	if _, marked := resp["ember_fallback_resend"]; marked {
		t.Fatal("clean request marked as fallback resend")
	}
}
