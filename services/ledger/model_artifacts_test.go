package ledger

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	core "ember/core-go"
	pb "ember/proto"
	chain "ember/services/chain"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"google.golang.org/protobuf/proto"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

func artifactFixture(t *testing.T) chain.Model {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	m := &pb.ModelManifest{ChainId: "ember-unbundled"}
	core.Sign(m, key)
	root, _ := core.Digest(m)
	g := &pb.ChainGenesis{ChainId: m.ChainId, ModelManifestRoot: root, ExecutionProfileVer: "test"}
	core.Sign(g, key)
	gr, _ := proto.Marshal(g)
	mr, _ := proto.Marshal(m)
	gh := sha256.Sum256(gr)
	pd, _ := core.Digest(g)
	return chain.Model{ID: g.ChainId, GenesisHash: hex.EncodeToString(gh[:]), ProfileHash: hex.EncodeToString(pd), Publication: &chain.Publication{Genesis: gr, Manifest: mr, Publisher: core.NodeID(g.Signer), ManifestRoot: hex.EncodeToString(root)}}
}
func TestModelArtifacts(t *testing.T) {
	for _, test := range []string{"publication", "legacy", "invalid", "signer", "root", "id", "bytes", "missing", "profile-hash"} {
		t.Run(test, func(t *testing.T) {
			m := artifactFixture(t)
			switch test {
			case "legacy":
				m.Genesis, m.Manifest = m.Publication.Genesis, m.Publication.Manifest
				m.Publication = nil
				h := sha256.Sum256(m.Manifest)
				m.ProfileHash = hex.EncodeToString(h[:])
			case "invalid":
				m.Publication.Invalid = true
			case "signer":
				mf := &pb.ModelManifest{}
				proto.Unmarshal(m.Publication.Manifest, mf)
				_, key, _ := ed25519.GenerateKey(rand.Reader)
				core.Sign(mf, key)
				m.Publication.Manifest, _ = proto.Marshal(mf)
			case "root":
				m.Publication.ManifestRoot = "bad"
			case "id":
				m.ID = "ember-other"
			case "bytes":
				m.Publication.Genesis[0] ^= 1
			case "missing":
				m.Publication = nil
			case "profile-hash":
				m.ProfileHash = "bad"
			}
			a, e := VerifyModelArtifacts("ember-unbundled", m)
			good := test == "publication" || test == "legacy"
			if good && (e != nil || len(a.Genesis) == 0) {
				t.Fatalf("%v", e)
			}
			if !good && e == nil {
				t.Fatal("accepted bad artifacts")
			}
		})
	}
}

func TestArtifactClientCacheHeightAndIsolation(t *testing.T) {
	model := artifactFixture(t)
	var height atomic.Int64
	height.Store(1)
	var queries atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		var result any
		switch request.Method {
		case "status":
			result = map[string]any{"node_info": map[string]any{"network": "local-ledger"}, "sync_info": map[string]any{"latest_block_height": fmt.Sprint(height.Load())}}
		case "abci_query":
			queries.Add(1)
			m := model
			p := *m.Publication
			m.Publication = &p
			if height.Load() > 1 {
				m.Publication.Invalid = true
			}
			raw, _ := json.Marshal(m)
			result = map[string]any{"response": map[string]any{"code": 0, "height": fmt.Sprint(height.Load()), "value": base64.StdEncoding.EncodeToString(raw)}}
		default:
			t.Errorf("unexpected method %s", request.Method)
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	defer server.Close()
	client, e := NewArtifactClient(server.URL, "local-ledger")
	if e != nil {
		t.Fatal(e)
	}
	a, e := client.Fetch(context.Background(), model.ID)
	if e != nil {
		t.Fatal(e)
	}
	a.Genesis[0] ^= 1
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, e := client.Fetch(context.Background(), model.ID)
			if e != nil || !bytes.Equal(a.Genesis, model.Publication.Genesis) {
				t.Errorf("cache mutation or fetch error: %v", e)
			}
		}()
	}
	wg.Wait()
	if queries.Load() != 1 {
		t.Fatal("concurrent fetches not deduplicated", queries.Load())
	}
	height.Store(2)
	if _, e = client.Fetch(context.Background(), model.ID); e == nil {
		t.Fatal("new-height invalidation hidden by cache")
	}
	client.ChainID = "wrong-ledger"
	if _, e = client.Fetch(context.Background(), model.ID); e == nil {
		t.Fatal("wrong ledger accepted")
	}
	if _, e = client.Fetch(context.Background(), "../escape"); e == nil {
		t.Fatal("path ID accepted")
	}
}
