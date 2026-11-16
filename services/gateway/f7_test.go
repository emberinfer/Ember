package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	core "ember/core-go"
	pb "ember/proto"
	"ember/services/ledger"
	"ember/services/scheduler"
	"ember/services/tunnel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"net"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestF7TokenizerFailureDoesNotOccupyAdmission(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	id := core.NodeID(key.Public().(ed25519.PublicKey))
	book, err := ledger.Open(filepath.Join(t.TempDir(), "f7.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	token := strings.Repeat("t", 32)
	if err = book.IssueToken("consumer", 100000, token); err != nil {
		t.Fatal(err)
	}
	relay := tunnel.New()
	relay.Legacy = true
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	pb.RegisterNodeTunnelServer(server, relay)
	go server.Serve(listener)
	defer server.Stop()
	connection, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := pb.NewNodeTunnelClient(connection).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = stream.Send(&pb.TunnelFrame{NodeId: id, Kind: "hello"}); err != nil {
		t.Fatal(err)
	}
	for !relay.Online(id) {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	p := &pb.ResourceProfile{NodeId: id, ChainId: "ember-mock", ExecutionProfileVer: "v1", ManifestRoot: []byte{1}, MemoryBytes: 1}
	core.Sign(p, key)
	g := &Gateway{Ledger: book, Tunnel: relay, Price: 1, PriceInput: 1, Profile: "v1", Manifest: []byte{1}, Admission: make(chan struct{}, 1), Candidates: func() []scheduler.Candidate { return []scheduler.Candidate{{Profile: p, Online: true, Stake: 50000}} }}
	seen := make(chan int, 4)
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 4; i++ {
			job, e := stream.Recv()
			if e != nil {
				done <- e
				return
			}
			seen <- len(g.Admission)
			if e = stream.Send(&pb.TunnelFrame{NodeId: id, RequestId: job.RequestId, AttemptId: job.AttemptId, Kind: "error", Payload: []byte("tokenizer unavailable")}); e != nil {
				done <- e
				return
			}
		}
		done <- nil
	}()
	for _, priceInput := range []int64{0, 1} {
		g.PriceInput = priceInput
		for _, path := range []string{"/v1/quote", "/v1/chat/completions"} {
			request := httptest.NewRequest("POST", path, strings.NewReader(`{"model":"ember-mock","messages":[{"role":"user","content":"prompt"}],"max_tokens":1}`))
			request.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()
			g.ServeHTTP(response, request)
			if response.Code != 503 || <-seen != 0 || len(g.Admission) != 0 || g.active != 0 {
				t.Fatal("tokenizer failure reserved an admission slot", response.Code, g.active)
			}
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	balance, _, err := book.Balance("consumer")
	if err != nil || balance != 100000 {
		t.Fatal("tokenizer failure froze consumer funds", balance, err)
	}
}
