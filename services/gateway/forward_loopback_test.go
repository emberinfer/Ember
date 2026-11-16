package gateway

import (
	"bufio"
	"context"
	"crypto/ed25519"
	core "ember/core-go"
	pb "ember/proto"
	"ember/services/evidence"
	"ember/services/ledger"
	"ember/services/scheduler"
	"ember/services/tunnel"
	"encoding/hex"
	"google.golang.org/grpc"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Opt-in local harness: real Python Agent, TCP gRPC tunnel and TCP HTTP SSE.
func TestForwardLoopback(t *testing.T) {
	python := os.Getenv("EMBER_LOOPBACK_PYTHON")
	if python == "" {
		t.Skip("set EMBER_LOOPBACK_PYTHON to run local forwarding measurement")
	}
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	id := core.NodeID(key.Public().(ed25519.PublicKey))
	book, err := ledger.Open(filepath.Join(t.TempDir(), "loop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	token := strings.Repeat("t", 32)
	for _, err := range []error{book.IssueToken("consumer", 100000, token), book.Create(id, 100000), book.Stake(id, 50000)} {
		if err != nil {
			t.Fatal(err)
		}
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
	defer server.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, python, filepath.Join(root, "ops/forward_loopback/fake_engine_node.py"), listener.Addr().String(), hex.EncodeToString(key.Seed()))
	cmd.Env = append(os.Environ(), "PYTHONPATH="+strings.Join([]string{filepath.Join(root, "node"), filepath.Join(root, "core-py"), filepath.Join(root, "proto")}, ":"))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	for !relay.Online(id) {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	profile := &pb.ResourceProfile{NodeId: id, ChainId: "ember-mock", ExecutionProfileVer: "v1", ManifestRoot: []byte{1}, MemoryBytes: 1}
	core.Sign(profile, key)
	g := &Gateway{Ledger: book, Tunnel: relay, Price: 1, Profile: "v1", Manifest: []byte{1}, Candidates: func() []scheduler.Candidate {
		return []scheduler.Candidate{{Profile: profile, Online: true, Stake: 50000}}
	}}
	httpServer := httptest.NewServer(g)
	defer httpServer.Close()
	for trial := 0; trial < 3; trial++ {
		req, _ := http.NewRequestWithContext(ctx, "POST", httpServer.URL+"/v1/chat/completions", strings.NewReader(`{"model":"ember-mock","messages":[{"role":"user","content":"hello"}],"max_tokens":400,"stream":true}`))
		req.Header.Set("Authorization", "Bearer "+token)
		start := time.Now()
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		scanner := bufio.NewScanner(response.Body)
		n := 0
		var first, last time.Time
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, `"delta"`) && strings.Contains(line, `"content"`) {
				n++
				now := time.Now()
				if first.IsZero() {
					first = now
				}
				last = now
			}
		}
		response.Body.Close()
		if err = scanner.Err(); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != 200 || n != 400 {
			t.Fatalf("status=%d tokens=%d", response.StatusCode, n)
		}
		t.Logf("FORWARD_LOOPBACK trial=%d tokens=%d first_to_last_s=%.6f delivered_tok_s=%.2f total_s=%.6f", trial, n, last.Sub(first).Seconds(), float64(n-1)/last.Sub(first).Seconds(), time.Since(start).Seconds())
	}
}
