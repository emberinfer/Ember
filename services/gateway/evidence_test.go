package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"net"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

func TestGatewayChunkTransfer(t *testing.T) {
	for _, mode := range []string{"complete", "tampered-with-good-cache", "missing"} {
		t.Run(mode, func(t *testing.T) {
			_, key, _ := ed25519.GenerateKey(rand.Reader)
			ident := core.NodeID(key.Public().(ed25519.PublicKey))
			book, err := ledger.Open(filepath.Join(t.TempDir(), "gateway.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
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
			group := make([]int, 512)
			data, _ := json.Marshal(group)
			hash := sha256.Sum256(data)
			m := &pb.EvidenceManifest{ChunkCount: 4, TotalTokens: 2048, ChunkHashes: [][]byte{hash[:], hash[:], hash[:], hash[:]}}
			m.MerkleRoot = evidence.MerkleRoot(m.ChunkHashes)
			// A cached good chunk must not hide tampering in a new upload.
			if _, err = relay.Evidence.Put(data); err != nil {
				t.Fatal(err)
			}
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
			if err = stream.Send(&pb.TunnelFrame{NodeId: ident, Kind: "hello"}); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			holdID := ""
			go func() {
				job, err := stream.Recv()
				if err != nil {
					done <- err
					return
				}
				send := func(kind string, payload []byte) error {
					return stream.Send(&pb.TunnelFrame{NodeId: ident, RequestId: job.RequestId, AttemptId: job.AttemptId, Kind: kind, Payload: payload})
				}
				for i := 0; i < 2048; i++ {
					if err = send("token", []byte("x")); err != nil {
						done <- err
						return
					}
				}
				r := &pb.ExecutionReceipt{RequestId: job.RequestId, NodeId: ident, OutputTokens: 2048, EvidenceManifest: m}
				var previous []byte
				for i := uint32(0); i < 128; i++ {
					root := &pb.ChunkRoot{RequestId: job.RequestId, Seq: i, UptoStep: (i + 1) * 16, MerkleRoot: hash[:], PrevRoot: previous}
					core.Sign(root, key)
					r.ChunkRoots = append(r.ChunkRoots, root)
					previous, _ = core.Digest(root)
				}
				core.Sign(r, key)
				for i := uint32(0); i < 4; i++ {
					if mode == "missing" && i == 3 {
						continue
					}
					payload := data
					if mode == "tampered-with-good-cache" && i == 0 {
						payload = []byte(`[9]`)
					}
					raw, _ := proto.Marshal(&pb.EvidenceChunk{ManifestRoot: m.MerkleRoot, Index: i, Data: payload})
					if err = send("evidence_chunk", raw); err != nil {
						done <- err
						return
					}
				}
				raw, _ := proto.Marshal(r)
				done <- send("receipt", raw)
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
			submitted := false
			g := &Gateway{Ledger: book, Tunnel: relay, Price: 1, Profile: "v1", Manifest: []byte{1},
				Candidates: func() []scheduler.Candidate {
					return []scheduler.Candidate{{Profile: profile, Online: true, Stake: 50000}}
				},
				BeforeSubmit: func(r *pb.ExecutionReceipt, inline []byte, _ string, _ int64, _ int64) error {
					submitted = true
					holdID = r.RequestId
					if len(inline) != 0 || !proto.Equal(r.EvidenceManifest, m) {
						t.Error("chunked evidence was inlined or manifest changed")
					}
					return nil
				},
			}
			request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"ember-mock","messages":[{"role":"user","content":"long"}],"max_tokens":2048}`))
			request.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()
			g.ServeHTTP(response, request)
			if err = <-done; err != nil {
				t.Fatal(err)
			}
			if mode == "complete" {
				if response.Code != 200 || !submitted {
					t.Fatalf("long request failed: %d %s", response.Code, response.Body)
				}
				if state, err := book.State(holdID); err != nil || state != "dispute" {
					t.Fatal(state, err)
				}
			} else if response.Code == 200 || submitted {
				t.Fatal("invalid chunk upload submitted", response.Code)
			}
		})
	}
}
