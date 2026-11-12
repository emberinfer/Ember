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

// A slow evidence consumer must backpressure the stream, not disconnect a long
// generation because an arbitrary fixed-size message queue filled up.
func TestLongDispatchBackpressure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	s := New()
	s.Legacy = true
	pb.RegisterNodeTunnelServer(server, s)
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
	if err = stream.Send(&pb.TunnelFrame{NodeId: "node", Kind: "hello"}); err != nil {
		t.Fatal(err)
	}
	sent := make(chan error, 1)
	go func() {
		job, err := stream.Recv()
		if err != nil {
			sent <- err
			return
		}
		for i := 0; i < 2048; i++ {
			if err = stream.Send(&pb.TunnelFrame{NodeId: "node", RequestId: job.RequestId, AttemptId: job.AttemptId, Kind: "token", Payload: []byte("x")}); err != nil {
				sent <- err
				return
			}
		}
		sent <- stream.Send(&pb.TunnelFrame{NodeId: "node", RequestId: job.RequestId, AttemptId: job.AttemptId, Kind: "receipt"})
	}()
	for !s.Online("node") {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	count := 0
	err = s.Run(ctx, "node", "long", "attempt", nil, func(frame *pb.TunnelFrame) error {
		if frame.Kind == "token" {
			count++
			time.Sleep(100 * time.Microsecond)
		}
		return nil
	})
	if err != nil || count != 2048 {
		t.Fatalf("long stream: tokens=%d error=%v", count, err)
	}
	if err = <-sent; err != nil {
		t.Fatal(err)
	}
}
