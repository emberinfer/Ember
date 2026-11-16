package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	core "ember/core-go"
	pb "ember/proto"
	"ember/services/ledger"
	"ember/services/tracker"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

func TestJoinAuthentication(t *testing.T) {
	book, err := ledger.Open(filepath.Join(t.TempDir(), "book.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	s := New()
	s.Tracker = tracker.New()
	s.Ledger = book
	s.JoinGrant = 100000
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s.Genesis = &pb.ChainGenesis{ChainId: "ember-test", ModelManifestRoot: make([]byte, 32), ExecutionProfileVer: "v1"}
	s.GenesisBytes, _ = proto.Marshal(s.Genesis)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	server := grpc.NewServer()
	pb.RegisterNodeTunnelServer(server, s)
	go server.Serve(l)
	defer server.Stop()
	conn, err := grpc.NewClient(l.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id := core.NodeID(pub)
	for _, bad := range []bool{true, false} {
		stream, err := pb.NewNodeTunnelClient(conn).Connect(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = stream.Send(&pb.TunnelFrame{NodeId: id, Kind: "join"}); err != nil {
			t.Fatal(err)
		}
		challenge, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		var data JoinData
		if err = json.Unmarshal(challenge.Payload, &data); err != nil {
			t.Fatal(err)
		}
		observed, score, err := tracker.Benchmark(ctx, func(_ context.Context, q []string) ([]string, error) { return q, nil })
		if err != nil || score != 2 {
			t.Fatal(err, score)
		}
		p := &pb.ResourceProfile{NodeId: id, ChainId: "ember-test", MemoryBytes: 16 << 30, EngineVer: "mock-v1", BenchmarkResultRef: observed, ExpiresUnix: time.Now().Add(time.Minute).Unix(), ManifestRoot: make([]byte, 32), ExecutionProfileVer: "v1"}
		if err = core.Sign(p, key); err != nil {
			t.Fatal(err)
		}
		wire, _ := proto.Marshal(p)
		var signature []byte
		if !bad {
			signature = ed25519.Sign(key, core.JoinChallenge(data.Nonce))
		}
		payload, _ := json.Marshal(JoinProof{Profile: wire, Signature: signature, Answers: data.Questions})
		if err = stream.Send(&pb.TunnelFrame{NodeId: id, Kind: "authenticate", Payload: payload}); err != nil {
			t.Fatal(err)
		}
		ready, err := stream.Recv()
		if bad {
			if err == nil || s.Online(id) {
				t.Fatalf("unsigned join accepted: %v", ready)
			}
			continue
		}
		if err != nil || ready.Kind != "ready" {
			t.Fatalf("valid join: %v %v", ready, err)
		}
		for !s.Online(id) {
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			default:
				time.Sleep(time.Millisecond)
			}
		}
		_, stake, err := book.Balance(id)
		if err != nil || stake != 50000 {
			t.Fatalf("stake=%d %v", stake, err)
		}
		_ = stream.CloseSend()
	}
}
