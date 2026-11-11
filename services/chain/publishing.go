package chain

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"

	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

// PublishingParameters are consensus configuration, in blocks and Spark subunits.
// Nil preserves historical state hashes; EffectivePublishing exposes the defaults.
type PublishingParameters struct {
	Bond               int64  `json:"bond"`
	ChallengeBlocks    int64  `json:"challenge_blocks"`
	SettledRequests    uint64 `json:"settled_requests"`
	SubmissionInterval int64  `json:"submission_interval"`
}

func (p *PublishingParameters) validate() error {
	if p != nil && (p.Bond < 1 || p.ChallengeBlocks < 2 || p.SettledRequests < 1 || p.SubmissionInterval < 1) {
		return errors.New("invalid publisher parameters")
	}
	return nil
}

func (s *State) EffectivePublishing() PublishingParameters {
	if s.Parameters.Publishing != nil {
		return *s.Parameters.Publishing
	}
	return PublishingParameters{Bond: 100, ChallengeBlocks: 100, SettledRequests: 3, SubmissionInterval: 10}
}

type Origin struct {
	Repository string `json:"repository"`
	Revision   string `json:"revision"` // pinned Git commit object ID, never a branch name
}

type Publication struct {
	Publisher           string `json:"publisher"`
	RegisteredHeight    int64  `json:"registered_height"`
	ManifestRoot        string `json:"manifest_root"`
	Genesis             []byte `json:"genesis"`
	Manifest            []byte `json:"manifest"`
	Origin              Origin `json:"origin"`
	PreviousProfileHash string `json:"previous_profile_hash,omitempty"`
	Bond                int64  `json:"bond"`
	ReleaseHeight       int64  `json:"release_height"`
	SettledRequests     uint64 `json:"settled_requests"`
	RequiredRequests    uint64 `json:"required_requests"`
	Invalid             bool   `json:"invalid,omitempty"`
}

type Endorsement struct {
	Account string `json:"account"`
	Height  int64  `json:"height"`
	Revoked bool   `json:"revoked,omitempty"`
}

func (s *State) publish(signer string, a Action, height int64) error {
	m := Model{ID: a.ModelID, GenesisHash: a.GenesisHash, ProfileHash: a.ProfileHash, Pricing: a.Pricing}
	if validateModel(m) != nil || s.Models[m.ID].ID != "" || (m.Pricing != nil && !s.Parameters.RegisteredAuditors) {
		return errors.New("invalid or duplicate model")
	}
	g, manifest := &pb.ChainGenesis{}, &pb.ModelManifest{}
	if proto.Unmarshal(a.Genesis, g) != nil || proto.Unmarshal(a.Manifest, manifest) != nil || core.Verify(g) != nil || core.Verify(manifest) != nil ||
		core.NodeID(g.Signer) != signer || !bytes.Equal(g.Signer, manifest.Signer) || g.ChainId != m.ID || manifest.ChainId != m.ID || g.ExecutionProfileVer == "" {
		return errors.New("publisher must sign matching genesis and manifest")
	}
	if _, err := core.TokenFee(g, 0, 0); err != nil {
		return err
	}
	if err := validateRangeRules(manifest); err != nil {
		return err
	}
	root, _ := core.Digest(manifest)
	genesisHash := sha256.Sum256(a.Genesis)
	profileHash, _ := core.Digest(g)
	if !bytes.Equal(root, g.ModelManifestRoot) || hex.EncodeToString(genesisHash[:]) != m.GenesisHash || hex.EncodeToString(profileHash) != m.ProfileHash {
		return errors.New("manifest/profile root does not recompute")
	}
	if len(manifest.Artifacts) == 0 || len(manifest.ConfigHash) != 32 || len(manifest.GraphHash) != 32 || g.PriceOutput < 1 || g.PriceInput < 0 || g.PriceContext < 0 {
		return errors.New("incomplete manifest or pricing")
	}
	if a.Origin == nil {
		return errors.New("pinned origin required")
	}
	u, err := url.Parse(a.Origin.Repository)
	revision, e := hex.DecodeString(a.Origin.Revision)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || e != nil || len(revision) != 20 || a.Origin.Revision != strings.ToLower(a.Origin.Revision) {
		return errors.New("origin requires HTTPS repository and lowercase Git commit ID")
	}
	seen := map[string]bool{}
	for _, artifact := range manifest.Artifacts {
		if artifact.Name == "" || path.Clean(artifact.Name) != artifact.Name || strings.HasPrefix(artifact.Name, "/") || strings.HasPrefix(artifact.Name, "../") || seen[artifact.Name] || len(artifact.Sha256) != 32 || artifact.Size <= 0 || artifact.Source != "git:"+artifact.Name {
			return errors.New("invalid, duplicate or unpinned origin artifact")
		}
		seen[artifact.Name] = true
	}
	if !seen["config.json"] || !seen["tokenizer.json"] {
		return errors.New("config and tokenizer artifacts required")
	}
	for _, artifact := range manifest.Artifacts {
		if artifact.Name == "config.json" && !bytes.Equal(artifact.Sha256, manifest.ConfigHash) {
			return errors.New("config root mismatch")
		}
		if artifact.Name == "tokenizer.json" && !bytes.Equal(artifact.Sha256, g.TokenizerHash) {
			return errors.New("tokenizer root mismatch")
		}
	}
	if a.PreviousProfileHash != "" {
		found := false
		for _, old := range s.Models {
			if old.ProfileHash == a.PreviousProfileHash {
				found = true
			}
		}
		if !found {
			return errors.New("previous profile anchor not found")
		}
	}
	p := s.EffectivePublishing()
	if last, ok := s.PublisherLast[signer]; ok && height-last < p.SubmissionInterval {
		return errors.New("publisher submission rate limited")
	}
	if height > int64(^uint64(0)>>1)-p.ChallengeBlocks {
		return errors.New("publisher release height overflow")
	}
	if err := s.debitBalance(signer, p.Bond); err != nil {
		return err
	}
	m.Publication = &Publication{Publisher: signer, RegisteredHeight: height, ManifestRoot: hex.EncodeToString(root),
		Genesis: append([]byte(nil), a.Genesis...), Manifest: append([]byte(nil), a.Manifest...), Origin: *a.Origin,
		PreviousProfileHash: a.PreviousProfileHash, Bond: p.Bond, ReleaseHeight: height + p.ChallengeBlocks, RequiredRequests: p.SettledRequests}
	if s.PublisherLast == nil {
		s.PublisherLast = map[string]int64{}
	}
	s.PublisherLast[signer] = height
	s.Models[m.ID] = m
	return nil
}

func (s *State) modelByRoot(root string) (Model, bool) {
	for _, m := range s.Models {
		if m.Publication != nil && m.Publication.ManifestRoot == root && !m.Publication.Invalid {
			return m, true
		}
	}
	return Model{}, false
}

func (s *State) endorse(signer string, a Action, height int64) error {
	if a.Type != "revoke_endorsement" {
		if _, ok := s.modelByRoot(a.ProfileHash); !ok {
			return errors.New("unknown or invalid profile manifest root")
		}
	}
	if s.Endorsements == nil {
		s.Endorsements = map[string]map[string]Endorsement{}
	}
	if s.Endorsements[a.ProfileHash] == nil {
		s.Endorsements[a.ProfileHash] = map[string]Endorsement{}
	}
	prior, exists := s.Endorsements[a.ProfileHash][signer]
	revoke := a.Type == "revoke_endorsement"
	if (revoke && (!exists || prior.Revoked)) || (!revoke && exists && !prior.Revoked) {
		return errors.New("endorsement already in requested state")
	}
	s.Endorsements[a.ProfileHash][signer] = Endorsement{Account: signer, Height: height, Revoked: revoke}
	return nil
}

func (s *State) claimPublisherBond(signer, id string, height int64) error {
	m := s.Models[id]
	p := m.Publication
	if p == nil || p.Publisher != signer || p.Bond == 0 || p.Invalid || (height < p.ReleaseHeight && p.SettledRequests < p.RequiredRequests) {
		return errors.New("publisher bond is not releasable")
	}
	if err := s.creditBalance(signer, p.Bond); err != nil {
		return err
	}
	p.Bond = 0
	return nil
}

func modelArtifacts(m Model) ([]byte, []byte) {
	if m.Publication != nil {
		return m.Publication.Genesis, m.Publication.Manifest
	}
	return m.Genesis, m.Manifest
}
func validateModelArtifacts(m Model) error {
	graw, mraw := modelArtifacts(m)
	if len(graw) == 0 && len(mraw) == 0 {
		return nil
	}
	g, manifest := &pb.ChainGenesis{}, &pb.ModelManifest{}
	if proto.Unmarshal(graw, g) != nil || proto.Unmarshal(mraw, manifest) != nil || core.Verify(g) != nil || core.Verify(manifest) != nil {
		return errors.New("invalid signed model artifacts")
	}
	if _, err := core.TokenFee(g, 0, 0); err != nil {
		return err
	}
	if err := validateRangeRules(manifest); err != nil {
		return err
	}
	root, _ := core.Digest(manifest)
	gh := sha256.Sum256(graw)
	mh := sha256.Sum256(mraw)
	if g.ChainId != m.ID || manifest.ChainId != m.ID || !bytes.Equal(root, g.ModelManifestRoot) || hex.EncodeToString(gh[:]) != m.GenesisHash {
		return errors.New("signed model anchor mismatch")
	}
	if m.Publication == nil && hex.EncodeToString(mh[:]) != m.ProfileHash {
		return errors.New("genesis manifest hash mismatch")
	}
	return nil
}
func (s *State) validatePublishedNode(p *pb.ResourceProfile) error {
	m := s.Models[p.ChainId]
	graw, mraw := modelArtifacts(m)
	if len(graw) == 0 {
		return nil
	}
	g, manifest := &pb.ChainGenesis{}, &pb.ModelManifest{}
	if (m.Publication != nil && m.Publication.Invalid) || proto.Unmarshal(graw, g) != nil || proto.Unmarshal(mraw, manifest) != nil || !bytes.Equal(p.ManifestRoot, g.ModelManifestRoot) || p.ExecutionProfileVer != g.ExecutionProfileVer {
		return errors.New("node does not match published profile")
	}
	if p.ShardTotalLayers > 0 {
		if manifest.DynamicRanges {
			if !p.ShardDynamicRange || p.ShardUnitKind != "range" || p.ShardTotalLayers != manifest.TotalLayers || p.ShardStartLayer >= p.ShardEndLayer || p.ShardEndLayer > manifest.TotalLayers || p.ShardHasEmbedding != (p.ShardStartLayer == 0) || p.ShardHasLmHead != (p.ShardEndLayer == manifest.TotalLayers) || !bytes.Equal(p.ShardParametersHash, core.RangeParameters(g.ModelManifestRoot, p.ShardStartLayer, p.ShardEndLayer)) {
				return errors.New("invalid dynamic range capability")
			}
			return nil
		}
		if p.ShardDynamicRange {
			return errors.New("pinned profile cannot register dynamic ranges")
		}
		for _, spec := range manifest.Shards {
			if spec.UnitKind == p.ShardUnitKind && spec.Index == p.ShardIndex && spec.StartLayer == p.ShardStartLayer && spec.EndLayer == p.ShardEndLayer && spec.HasEmbedding == p.ShardHasEmbedding && spec.HasLmHead == p.ShardHasLmHead && bytes.Equal(spec.ParametersHash, p.ShardParametersHash) && p.ShardTotalLayers == manifest.TotalLayers {
				if !core.ValidUnitKind(spec.UnitKind, spec.StartLayer, spec.EndLayer, manifest.TotalLayers, spec.HasEmbedding, spec.HasLmHead) {
					return errors.New("invalid stage unit kind")
				}
				_, _, err := core.StageSpan(spec.StartLayer, spec.EndLayer, manifest.TotalLayers, spec.HasEmbedding, spec.HasLmHead)
				return err
			}
		}
		return errors.New("shard capability differs from anchored layout")
	}
	return nil
}

// OriginProof authenticates one Git blob through a pinned commit and tree path.
// Large LFS weights need only their small committed pointer, never the weight bytes.
// No network access, oracle, caller-selected hash or governor participates.
type OriginProof struct {
	Artifact string   `json:"artifact"`
	Commit   []byte   `json:"commit"`
	Trees    [][]byte `json:"trees"`
	Blob     []byte   `json:"blob"`
}

func gitHash(kind string, raw []byte) []byte {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", kind, len(raw))
	h.Write(raw)
	return h.Sum(nil)
}

func originBlob(o Origin, p *OriginProof) ([]byte, error) {
	if p == nil || len(p.Trees) == 0 || len(p.Trees) > 32 || hex.EncodeToString(gitHash("commit", p.Commit)) != o.Revision {
		return nil, errors.New("invalid pinned commit proof")
	}
	line, _, _ := bytes.Cut(p.Commit, []byte("\n"))
	if !bytes.HasPrefix(line, []byte("tree ")) {
		return nil, errors.New("missing commit tree")
	}
	want, err := hex.DecodeString(string(line[5:]))
	parts := strings.Split(p.Artifact, "/")
	if err != nil || len(want) != 20 || len(parts) != len(p.Trees) {
		return nil, errors.New("invalid origin path")
	}
	for i, tree := range p.Trees {
		if !bytes.Equal(gitHash("tree", tree), want) {
			return nil, errors.New("origin tree hash mismatch")
		}
		found := false
		for len(tree) > 0 {
			entry, rest, ok := bytes.Cut(tree, []byte{0})
			if !ok || len(rest) < 20 {
				return nil, errors.New("invalid Git tree")
			}
			mode, name, ok := bytes.Cut(entry, []byte(" "))
			if !ok {
				return nil, errors.New("invalid Git tree entry")
			}
			if string(name) == parts[i] {
				if (i < len(parts)-1 && string(mode) != "40000") || (i == len(parts)-1 && string(mode) != "100644" && string(mode) != "100755") {
					return nil, errors.New("origin path is not a regular artifact")
				}
				want = append([]byte(nil), rest[:20]...)
				found = true
				break
			}
			tree = rest[20:]
		}
		if !found {
			return nil, errors.New("artifact missing from pinned tree")
		}
	}
	if !bytes.Equal(gitHash("blob", p.Blob), want) {
		return nil, errors.New("origin blob hash mismatch")
	}
	return p.Blob, nil
}

func (s *State) challengePublication(a Action, height int64) error {
	m := s.Models[a.ModelID]
	p := m.Publication
	if p == nil || p.Invalid || p.Bond == 0 || height >= p.ReleaseHeight {
		return errors.New("publication challenge window closed")
	}
	blob, err := originBlob(p.Origin, a.OriginProof)
	if err != nil {
		return err
	}
	manifest := &pb.ModelManifest{}
	if proto.Unmarshal(p.Manifest, manifest) != nil {
		return errors.New("invalid stored manifest")
	}
	var artifact *pb.Artifact
	for _, v := range manifest.Artifacts {
		if v.Name == a.OriginProof.Artifact {
			artifact = v
			break
		}
	}
	if artifact == nil {
		return errors.New("proof is not for a published artifact")
	}
	hash := sha256.Sum256(blob)
	size := int64(len(blob))
	actual := hash[:]
	if bytes.HasPrefix(blob, []byte("version https://git-lfs.github.com/spec/v1\n")) {
		lines := strings.Split(strings.TrimSpace(string(blob)), "\n")
		if len(lines) != 3 || !strings.HasPrefix(lines[1], "oid sha256:") || !strings.HasPrefix(lines[2], "size ") {
			return errors.New("unsupported LFS pointer")
		}
		actual, err = hex.DecodeString(strings.TrimPrefix(lines[1], "oid sha256:"))
		if err != nil || len(actual) != 32 {
			return errors.New("invalid LFS object hash")
		}
		size, err = strconv.ParseInt(strings.TrimPrefix(lines[2], "size "), 10, 64)
		if err != nil || size < 0 {
			return errors.New("invalid LFS size")
		}
	}
	if bytes.Equal(actual, artifact.Sha256) && size == artifact.Size {
		return errors.New("origin agrees with manifest; no fraud")
	}
	if err := add(&s.PublisherForfeited, p.Bond); err != nil {
		return err
	}
	p.Bond = 0
	p.Invalid = true
	return nil
}

func validateRangeRules(m *pb.ModelManifest) error {
	if !m.DynamicRanges {
		return nil
	}
	if m.TotalLayers == 0 || m.TotalLayers > ^uint32(0)-2 || m.MinStageLayers == 0 || m.MinStageLayers > m.TotalLayers || len(m.Shards) > 0 {
		return errors.New("invalid dynamic sharding rules")
	}
	if len(m.AllowedLayerCuts) > 4096 || (len(m.AllowedLayerCuts) == 0 && uint64(m.TotalLayers)/uint64(m.MinStageLayers)+2 > 4096) {
		return errors.New("dynamic cut catalog exceeds protocol bound")
	}
	if len(m.AllowedLayerCuts) > 0 {
		if m.AllowedLayerCuts[0] != 0 || m.AllowedLayerCuts[len(m.AllowedLayerCuts)-1] != m.TotalLayers {
			return errors.New("dynamic cuts omit model endpoints")
		}
		for i, v := range m.AllowedLayerCuts {
			if v > m.TotalLayers || (i > 0 && m.AllowedLayerCuts[i-1] >= v) {
				return errors.New("unsorted or duplicate cut points")
			}
		}
	}
	return nil
}
