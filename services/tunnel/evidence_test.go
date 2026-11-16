package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	core "ember/core-go"
	pb "ember/proto"
	"ember/services/evidence"
	"encoding/hex"
	"errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestFetchEvidenceRPC(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	id := core.NodeID(key.Public().(ed25519.PublicKey))
	s := New()
	s.Evidence = &evidence.Store{Dir: t.TempDir()}
	h, _ := s.Evidence.Put([]byte(`[1,2]`))
	h2, _ := s.Evidence.Put([]byte(`[3]`))
	m := &pb.EvidenceManifest{ChunkCount: 2, TotalTokens: 3, ChunkHashes: [][]byte{h, h2}}
	m.MerkleRoot = evidence.MerkleRoot(m.ChunkHashes)
	s.AuthorizeEvidence = func(hold, requester string) (*pb.EvidenceManifest, uint32, error) {
		if hold != "hold" || requester != id {
			return nil, 0, errors.New("not assigned")
		}
		return m, 2, nil
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	pb.RegisterNodeTunnelServer(server, s)
	go server.Serve(listener)
	defer server.Stop()
	connection, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	client := pb.NewNodeTunnelClient(connection)
	fetch := func(root []byte, index uint32, hold string, sign bool) (*pb.EvidenceChunk, error) {
		raw, _ := proto.Marshal(&pb.EvidenceChunk{ManifestRoot: root, Index: index})
		request := &pb.TunnelFrame{NodeId: id, RequestId: hold, Kind: "fetch_evidence", Payload: raw}
		if sign {
			core.Sign(request, key)
		}
		return client.FetchEvidence(context.Background(), request)
	}
	chunk, err := fetch(m.MerkleRoot, 0, "hold", true)
	if err != nil || string(chunk.Data) != `[1,2]` {
		t.Fatal(chunk, err)
	}
	for _, test := range []struct {
		root  []byte
		index uint32
		hold  string
		sign  bool
		code  codes.Code
	}{
		{make([]byte, 32), 0, "hold", true, codes.PermissionDenied},
		{m.MerkleRoot, 2, "hold", true, codes.NotFound},
		{m.MerkleRoot, 0, "other-hold", true, codes.PermissionDenied},
		{m.MerkleRoot, 0, "hold", false, codes.PermissionDenied},
	} {
		_, err = fetch(test.root, test.index, test.hold, test.sign)
		if status.Code(err) != test.code {
			t.Fatalf("wrong rejection %v", err)
		}
	}
	_, stranger, _ := ed25519.GenerateKey(rand.Reader)
	queryRaw, _ := proto.Marshal(&pb.EvidenceChunk{ManifestRoot: m.MerkleRoot})
	unauthorized := &pb.TunnelFrame{NodeId: core.NodeID(stranger.Public().(ed25519.PublicKey)), RequestId: "hold", Kind: "fetch_evidence", Payload: queryRaw}
	core.Sign(unauthorized, stranger)
	if _, err = client.FetchEvidence(context.Background(), unauthorized); status.Code(err) != codes.PermissionDenied {
		t.Fatal("unassigned signer fetched evidence", err)
	}
	path := filepath.Join(s.Evidence.Dir, hex.EncodeToString(h))
	os.WriteFile(path, []byte(`[9,2]`), 0600)
	if _, err = fetch(m.MerkleRoot, 0, "hold", true); status.Code(err) != codes.Unavailable {
		t.Fatal("corrupt chunk served", err)
	}
	os.Remove(path)
	if _, err = fetch(m.MerkleRoot, 0, "hold", true); status.Code(err) != codes.Unavailable {
		t.Fatal("missing chunk served", err)
	}
}
