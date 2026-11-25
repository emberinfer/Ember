package ledger

// RPC is an operator-selected trust anchor, not a light-client proof.
import (
	"bytes"
	"context"
	"crypto/sha256"
	core "ember/core-go"
	pb "ember/proto"
	chain "ember/services/chain"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	rpcclient "github.com/cometbft/cometbft/rpc/client"
	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	"google.golang.org/protobuf/proto"
	"regexp"
	"strings"
	"sync"
)

var profileID = regexp.MustCompile(`^ember-[a-zA-Z0-9_.-]{1,192}$`)

type ModelArtifacts struct {
	Genesis, Manifest                                     []byte
	GenesisSHA256, ProfileDigest, ManifestRoot, Publisher string
	Height                                                int64
}

// ArtifactClient serializes fetches and bounds its exact-byte cache to one entry.
// Every fetch checks the latest height; entries from older heights are never served.
type ArtifactClient struct {
	RPC     *rpchttp.HTTP
	ChainID string
	mu      sync.Mutex
	key     string
	cached  ModelArtifacts
}

func NewArtifactClient(rpc, chainID string) (*ArtifactClient, error) {
	if chainID == "" {
		return nil, errors.New("ledger chain ID required")
	}
	c, e := rpchttp.New(rpc)
	return &ArtifactClient{RPC: c, ChainID: chainID}, e
}
func (c *ArtifactClient) Fetch(ctx context.Context, id string) (*ModelArtifacts, error) {
	if !profileID.MatchString(id) {
		return nil, errors.New("invalid profile ID")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	status, e := c.RPC.Status(ctx)
	if e != nil {
		return nil, e
	}
	if status.NodeInfo.Network != c.ChainID {
		return nil, errors.New("ledger chain ID mismatch")
	}
	height := status.SyncInfo.LatestBlockHeight
	prefix := fmt.Sprintf("%s/%s/%d/", c.ChainID, id, height)
	clone := func(a ModelArtifacts) *ModelArtifacts {
		a.Genesis = bytes.Clone(a.Genesis)
		a.Manifest = bytes.Clone(a.Manifest)
		return &a
	}
	if height > 0 && strings.HasPrefix(c.key, prefix) {
		return clone(c.cached), nil
	}
	r, e := c.RPC.ABCIQueryWithOptions(ctx, "/model/"+id, nil, rpcclient.ABCIQueryOptions{Height: height})
	if e != nil {
		return nil, e
	}
	if r.Response.Code != 0 {
		return nil, fmt.Errorf("model query: %s", r.Response.Log)
	}
	if len(r.Response.Value) > 32<<20 {
		return nil, errors.New("model response exceeds limit")
	}
	var m chain.Model
	if e = json.Unmarshal(r.Response.Value, &m); e != nil {
		return nil, e
	}
	a, e := VerifyModelArtifacts(id, m)
	if e != nil {
		return nil, e
	}
	a.Height = r.Response.Height
	key := fmt.Sprintf("%s/%s/%d/%s/%s", c.ChainID, id, a.Height, a.GenesisSHA256, a.ManifestRoot)
	c.key = key
	c.cached = *a
	return clone(c.cached), nil
}
func VerifyModelArtifacts(id string, m chain.Model) (*ModelArtifacts, error) {
	if !profileID.MatchString(id) || m.ID != id {
		return nil, errors.New("model ID mismatch")
	}
	graw, mraw := m.Genesis, m.Manifest
	if p := m.Publication; p != nil {
		if p.Invalid {
			return nil, errors.New("publication invalid")
		}
		graw, mraw = p.Genesis, p.Manifest
	}
	if len(graw) == 0 || len(mraw) == 0 {
		return nil, errors.New("signed model artifacts unavailable")
	}
	if len(graw)+len(mraw) > 16<<20 {
		return nil, errors.New("model artifacts exceed limit")
	}
	g, mf := &pb.ChainGenesis{}, &pb.ModelManifest{}
	if proto.Unmarshal(graw, g) != nil || proto.Unmarshal(mraw, mf) != nil || core.Verify(g) != nil || core.Verify(mf) != nil {
		return nil, errors.New("invalid artifact signature")
	}
	if !bytes.Equal(g.Signer, mf.Signer) {
		return nil, errors.New("artifact signer mismatch")
	}
	root, _ := core.Digest(mf)
	pd, _ := core.Digest(g)
	gh := sha256.Sum256(graw)
	mh := sha256.Sum256(mraw)
	a := &ModelArtifacts{Genesis: bytes.Clone(graw), Manifest: bytes.Clone(mraw), GenesisSHA256: hex.EncodeToString(gh[:]), ProfileDigest: hex.EncodeToString(pd), ManifestRoot: hex.EncodeToString(root), Publisher: core.NodeID(g.Signer)}
	if g.ChainId != id || mf.ChainId != id || !bytes.Equal(root, g.ModelManifestRoot) || a.GenesisSHA256 != m.GenesisHash {
		return nil, errors.New("artifact anchor mismatch")
	}
	if p := m.Publication; p != nil {
		if p.Publisher != a.Publisher || p.ManifestRoot != a.ManifestRoot || m.ProfileHash != a.ProfileDigest {
			return nil, errors.New("publication anchor mismatch")
		}
	} else if m.ProfileHash != hex.EncodeToString(mh[:]) {
		return nil, errors.New("legacy manifest SHA256 mismatch")
	}
	return a, nil
}
