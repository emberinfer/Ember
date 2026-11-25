package tunnel

import (
	"context"
	"crypto/sha256"
	pb "ember/proto"
	"ember/services/tracker"
	"encoding/hex"
	"encoding/json"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"net"
	"strings"
	"testing"
	"time"
)

func TestJoinProfileAssertion(t *testing.T) {
	s := New()
	s.Tracker = tracker.New()
	s.Genesis = &pb.ChainGenesis{ChainId: "ember-selected"}
	s.GenesisBytes = []byte("exact genesis")
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	server := grpc.NewServer()
	pb.RegisterNodeTunnelServer(server, s)
	go server.Serve(listener)
	defer server.Stop()
	conn, e := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	hash := sha256.Sum256(s.GenesisBytes)
	for _, tc := range []struct {
		name, id, hash string
		good           bool
	}{{"legacy", "", "", true}, {"selected", "ember-selected", "", true}, {"pinned", "ember-selected", hex.EncodeToString(hash[:]), true}, {"wrong-id", "ember-other", "", false}, {"wrong-hash", "ember-selected", strings.Repeat("0", 64), false}, {"path", "../ember-selected", "", false}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			stream, e := pb.NewNodeTunnelClient(conn).Connect(ctx)
			if e != nil {
				t.Fatal(e)
			}
			var payload []byte
			if tc.id != "" {
				payload, _ = json.Marshal(map[string]string{"profile_id": tc.id, "genesis_sha256": tc.hash})
			}
			stream.Send(&pb.TunnelFrame{Kind: "join", Payload: payload})
			frame, e := stream.Recv()
			if tc.good {
				if e != nil || frame.Kind != "challenge" {
					t.Fatalf("%v %v", frame, e)
				}
			} else if e == nil {
				t.Fatal("wrong assertion received materials")
			}
		})
	}
}
