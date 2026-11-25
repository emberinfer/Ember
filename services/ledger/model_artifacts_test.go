package ledger

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	core "ember/core-go"
	pb "ember/proto"
	chain "ember/services/chain"
	"encoding/hex"
	"google.golang.org/protobuf/proto"
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
