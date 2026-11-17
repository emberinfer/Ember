package tunnel

import (
	"context"
	pb "ember/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"net"
	"testing"
	"time"
)

func TestDispatch(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	s := New()
	s.Legacy = true
	server := grpc.NewServer()
	pb.RegisterNodeTunnelServer(server, s)
	go server.Serve(l)
	defer server.Stop()
	c, err := grpc.NewClient(l.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := pb.NewNodeTunnelClient(c).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&pb.TunnelFrame{NodeId: "node", Kind: "hello"}); err != nil {
		t.Fatal(err)
	}
	go func() {
		f, e := stream.Recv()
		if e == nil {
			_ = stream.Send(&pb.TunnelFrame{NodeId: "node", RequestId: f.RequestId, AttemptId: f.AttemptId, Kind: "receipt"})
		}
	}()
	for !s.Online("node") {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	called := false
	if err := s.Run(ctx, "node", "req", "attempt", []byte("payload"), func(f *pb.TunnelFrame) error { called = true; return nil }); err != nil || !called {
		t.Fatalf("dispatch: %v %v", err, called)
	}
}
