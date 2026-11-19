package gateway

import (
	"bytes"
	"crypto/sha256"
	chain "ember/services/chain"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

func TestGatewaySpotGolden(t *testing.T) {
	raw, err := os.ReadFile("../chain/testdata/spot-merkle-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var vector struct {
		Leaves []struct {
			Step                                                                          uint64
			Tensor, InputHash, OutputHash, FormatHash, ProfileIDHash, RequestID, LeafHash string
		}
		Root string
	}
	// The fixture uses snake_case keys.
	var doc map[string]json.RawMessage
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var entries []map[string]json.RawMessage
	_ = json.Unmarshal(doc["leaves"], &entries)
	_ = json.Unmarshal(doc["root"], &vector.Root)
	leaves := []chain.SpotLeaf{}
	for _, entry := range entries {
		get := func(k string) string { var v string; _ = json.Unmarshal(entry[k], &v); return v }
		hash := func(k string) (v [32]byte) {
			b, e := hex.DecodeString(get(k))
			if e != nil || len(b) != 32 {
				t.Fatalf("invalid fixture %s", k)
			}
			copy(v[:], b)
			return
		}
		var step uint64
		_ = json.Unmarshal(entry["step"], &step)
		tensor, _ := hex.DecodeString(get("tensor"))
		h, e := HashSpotTensor(bytes.NewReader(tensor[14:]), "f32", []uint64{1})
		if e != nil || h != sha256.Sum256(tensor) {
			t.Fatalf("tensor: %x %v", h, e)
		}
		leaf := chain.SpotLeaf{Step: step, InputHash: hash("input_hash"), OutputHash: hash("output_hash"), FormatHash: hash("format_hash"), ProfileIDHash: hash("profile_id_hash"), RequestID: hash("request_id")}
		if chain.SpotLeafHash(leaf) != hash("leaf_hash") {
			t.Fatal("leaf mismatch")
		}
		leaves = append(leaves, leaf)
	}
	root, err := SpotRoot(leaves)
	if err != nil || hex.EncodeToString(root[:]) != vector.Root {
		t.Fatalf("root %x %v", root, err)
	}
	if _, err = HashSpotTensor(bytes.NewReader([]byte{1}), "f32", []uint64{1}); err == nil {
		t.Fatal("truncation accepted")
	}
}
