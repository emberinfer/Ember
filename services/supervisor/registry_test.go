package supervisor

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	core "ember/core-go"
	pb "ember/proto"
	"ember/services/chain"
	"ember/services/ledger"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"google.golang.org/protobuf/proto"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func registryModel(t *testing.T, id, engine string, shards bool) chain.Model {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	mf := &pb.ModelManifest{ChainId: id, DynamicRanges: shards}
	if e := core.Sign(mf, key); e != nil {
		t.Fatal(e)
	}
	root, _ := core.Digest(mf)
	g := &pb.ChainGenesis{ChainId: id, ExecutionProfileVer: engine, ModelManifestRoot: root}
	core.Sign(g, key)
	gr, _ := proto.Marshal(g)
	mr, _ := proto.Marshal(mf)
	gh := sha256.Sum256(gr)
	pd, _ := core.Digest(g)
	return chain.Model{ID: id, GenesisHash: hex.EncodeToString(gh[:]), ProfileHash: hex.EncodeToString(pd), Publication: &chain.Publication{Genesis: gr, Manifest: mr, Publisher: core.NodeID(g.Signer), ManifestRoot: hex.EncodeToString(root)}}
}
func TestRegistryVerifiedAnchorsUnsupportedAndInvalid(t *testing.T) {
	models := map[string]chain.Model{}
	for _, row := range []struct {
		id, engine string
		shards     bool
	}{{"ember-text", "cpu-text-v1", false}, {"ember-moe", "moe-v1", false}, {"ember-image", "diffusers-sd1.5-v2", false}, {"ember-shard", "pipeline-v1", true}, {"ember-invalid", "cpu-v1", false}} {
		models[row.id] = registryModel(t, row.id, row.engine, row.shards)
	}
	models["ember-invalid"].Publication.Invalid = true
	legacy := registryModel(t, "ember-legacy", "cpu-v1", false)
	legacy.Genesis, legacy.Manifest = legacy.Publication.Genesis, legacy.Publication.Manifest
	legacy.Publication = nil
	mh := sha256.Sum256(legacy.Manifest)
	legacy.ProfileHash = hex.EncodeToString(mh[:])
	models[legacy.ID] = legacy
	network := "test-ledger"
	broken := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		json.NewDecoder(r.Body).Decode(&request)
		var result any
		if request.Method == "status" {
			result = map[string]any{"node_info": map[string]any{"network": network}, "sync_info": map[string]any{"latest_block_height": "12"}}
		} else {
			raw, _ := json.Marshal(map[string]any{"models": models, "holds": map[string]any{"pending": map[string]any{"model_id": "ember-text", "state": "dispute"}}})
			if broken {
				raw = []byte("bad json")
			}
			result = map[string]any{"response": map[string]any{"code": 0, "height": "12", "value": base64.StdEncoding.EncodeToString(raw)}}
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	defer server.Close()
	client, e := ledger.NewArtifactClient(server.URL, network)
	if e != nil {
		t.Fatal(e)
	}
	registry := Registry{client}
	rows, e := registry.Discover(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if len(rows) != 6 {
		t.Fatal(rows)
	}
	for _, row := range rows {
		if row.ProfileID == "ember-legacy" && row.Published {
			t.Fatal("historical anchor treated as publication")
		}
		if row.ProfileID == "ember-text" && row.OrdersActive != 1 {
			t.Fatal("pending settlement was lost")
		}
		if row.Height != 12 {
			t.Fatal(row)
		}
		if row.ProfileID == "ember-text" || row.ProfileID == "ember-legacy" {
			if !row.Supported || row.GenesisSHA256 == "" || row.ManifestRoot == "" {
				t.Fatal(row)
			}
		} else if row.Supported || row.Reason == "" {
			t.Fatal(row)
		}
		if row.ProfileID == "ember-invalid" && !strings.Contains(row.Reason, "invalid") {
			t.Fatal(row)
		}
	}
	broken = true
	if _, e = registry.Discover(context.Background()); e == nil {
		t.Fatal("accepted broken registry")
	}
	broken = false
	network = "other-ledger"
	if _, e = registry.Discover(context.Background()); e == nil {
		t.Fatal("accepted wrong chain")
	}
}
