package evidence

import (
	"bytes"
	"crypto/sha256"
	pb "ember/proto"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestManifestIntegrityAndAvailability(t *testing.T) {
	data := [][]byte{[]byte(`[1,2]`), []byte(`[3,4]`), []byte(`[5]`)}
	store := &Store{Dir: t.TempDir()}
	m := &pb.EvidenceManifest{TotalTokens: 5, ChunkCount: 3}
	for _, chunk := range data {
		h, err := store.Put(chunk)
		if err != nil {
			t.Fatal(err)
		}
		m.ChunkHashes = append(m.ChunkHashes, h)
	}
	m.MerkleRoot = MerkleRoot(m.ChunkHashes)
	// Fixed cross-language vector, including odd-leaf duplication.
	if got := hex.EncodeToString(m.MerkleRoot); got != "d8910d1608eb864fb08c37a2903e98f55970c11702948e10247be90e1553f81a" {
		t.Fatalf("Merkle vector %s", got)
	}
	if err := Validate(m, 5, 2); err != nil {
		t.Fatal(err)
	}
	full, err := store.Assemble(m, 2)
	if err != nil || string(full) != `[1,2,3,4,5]` {
		t.Fatal(string(full), err)
	}
	if _, err = VerifyChunk(m, 0, []byte(`[9,2]`), 2); err == nil {
		t.Fatal("tamper accepted")
	}
	if _, err = store.Fetch(m, 3, 2); err == nil {
		t.Fatal("unknown index accepted")
	}
	if _, err = store.Fetch(m, 0, 1); err == nil {
		t.Fatal("frame parameter ignored")
	}
	path := filepath.Join(store.Dir, hex.EncodeToString(m.ChunkHashes[1]))
	if err = os.WriteFile(path, []byte(`[9,4]`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Assemble(m, 2); err == nil {
		t.Fatal("corrupt stored chunk accepted")
	}
	os.Remove(path)
	if _, err = store.Assemble(m, 2); err == nil {
		t.Fatal("missing chunk accepted")
	}
	m.MerkleRoot = bytes.Repeat([]byte{1}, sha256.Size)
	if err = Validate(m, 5, 2); err == nil {
		t.Fatal("wrong root accepted")
	}
}
