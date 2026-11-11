package core

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

func TestVectors(t *testing.T) {
	raw, err := os.ReadFile("../testdata/vectors/signed.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct{ Type, Wire, Digest string }
	if err = json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	types := map[string]func() proto.Message{
		"ChainGenesis": func() proto.Message { return &pb.ChainGenesis{} }, "ModelManifest": func() proto.Message { return &pb.ModelManifest{} },
		"ResourceProfile": func() proto.Message { return &pb.ResourceProfile{} }, "ExecutionCommitment": func() proto.Message { return &pb.ExecutionCommitment{} },
		"ChunkRoot": func() proto.Message { return &pb.ChunkRoot{} }, "ExecutionReceipt": func() proto.Message { return &pb.ExecutionReceipt{} },
		"AuditChallenge": func() proto.Message { return &pb.AuditChallenge{} }, "AuditVerdict": func() proto.Message { return &pb.AuditVerdict{} },
		"LedgerTx": func() proto.Message { return &pb.LedgerTx{} },
	}
	var chunks []proto.Message
	for _, v := range vectors {
		m := types[v.Type]()
		wire, _ := hex.DecodeString(v.Wire)
		if err = proto.Unmarshal(wire, m); err != nil {
			t.Fatal(err)
		}
		if err = Verify(m); err != nil {
			t.Fatalf("%s: %v", v.Type, err)
		}
		d, err := Digest(m)
		if err != nil || hex.EncodeToString(d) != v.Digest {
			t.Fatalf("%s digest: %x %v", v.Type, d, err)
		}
		sig, _ := field(m, "Signature")
		sig.SetBytes(make([]byte, 64))
		if Verify(m) == nil {
			t.Fatalf("%s tamper accepted", v.Type)
		}
		if v.Type == "ChunkRoot" {
			c := &pb.ChunkRoot{}
			_ = proto.Unmarshal(wire, c)
			chunks = append(chunks, c)
		}
	}
	extract := func(m proto.Message) (string, uint32, uint32, []byte, []byte) {
		c := m.(*pb.ChunkRoot)
		return c.RequestId, c.Seq, c.UptoStep, c.PrevRoot, c.MerkleRoot
	}
	if err := VerifyChunks(chunks, "req", 20, extract); err != nil {
		t.Fatal(err)
	}
	chunks[1].(*pb.ChunkRoot).PrevRoot = []byte("bad")
	if VerifyChunks(chunks, "req", 20, extract) == nil {
		t.Fatal("broken chain accepted")
	}
}

func TestTolerance(t *testing.T) {
	if Spark != 1000000 || !WithinTolerance([]float64{1, 2}, []float64{1, 2.01}, .01) {
		t.Fatal("numeric mismatch")
	}
}
