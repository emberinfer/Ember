package tunnel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/grpc"
)

// Exercise the real Go dispatcher, signed frames, Python Agent, disk
// reassembly, and bidirectional gRPC with untouched default message limits.
func TestAuditLargePythonPeer(t *testing.T) {
	python := os.Getenv("EMBER_AUDIT_TEST_PYTHON")
	if python == "" {
		python = "python3"
	}
	if err := exec.Command(python, "-c", "import grpc, cryptography").Run(); err != nil {
		t.Skip("Python gRPC dependencies unavailable; set EMBER_AUDIT_TEST_PYTHON")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	relay := New()
	relay.Legacy = true
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	relay.SchedulerKey = key
	server := grpc.NewServer()
	pb.RegisterNodeTunnelServer(server, relay)
	go server.Serve(listener)
	defer server.Stop()
	peer := exec.Command(python, "-m", "agent.audit_transfer_peer", listener.Addr().String(), core.NodeID(pub))
	peer.Env = append(os.Environ(), "PYTHONPATH="+root+"/node:"+root+"/proto:"+root+"/core-py", "PYTHONDONTWRITEBYTECODE=1")
	var output bytes.Buffer
	peer.Stdout, peer.Stderr = &output, &output
	if err = peer.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = peer.Process.Kill(); _ = peer.Wait() }()
	deadline := time.Now().Add(20 * time.Second)
	for !relay.Online("audit-probe") {
		if time.Now().After(deadline) {
			t.Fatal("Python peer did not connect")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, size := range []int{128, 8 << 20, 90 << 20, 256 << 20} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			payload := append([]byte(`{"blob":"`), bytes.Repeat([]byte("x"), size-11)...)
			payload = append(payload, []byte(`"}`)...)
			hash := sha256.Sum256(payload)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			received := false
			err := relay.Audit(ctx, "audit-probe", "request", strconv.Itoa(size), payload, func(frame *pb.TunnelFrame) error {
				var got struct {
					Size    int    `json:"size"`
					SHA256  string `json:"sha256"`
					Retried bool   `json:"retried"`
				}
				if frame.Kind != "audit_result" {
					t.Fatalf("unexpected reply: %s %s", frame.Kind, frame.Payload)
				}
				if e := json.Unmarshal(frame.Payload, &got); e != nil {
					return e
				}
				if got.Size != len(payload) || got.SHA256 != hex.EncodeToString(hash[:]) {
					t.Fatalf("integrity mismatch: %+v", got)
				}
				if size > auditChunkBytes && !got.Retried {
					t.Fatal("single-piece retry was not exercised")
				}
				received = true
				return nil
			})
			if err != nil || !received {
				t.Fatalf("audit delivery: %v received=%v", err, received)
			}
			t.Logf("payload_bytes=%d sha256=%x received=true", len(payload), hash)
		})
	}
}
