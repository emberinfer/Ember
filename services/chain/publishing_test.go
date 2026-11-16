package chain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

func publicationAction(t *testing.T, publisher actor, id string) Action {
	t.Helper()
	h := sha256.Sum256([]byte("artifact"))
	m := &pb.ModelManifest{ChainId: id, ConfigHash: h[:], GraphHash: h[:], Artifacts: []*pb.Artifact{
		{Name: "config.json", Sha256: h[:], Size: 8, Source: "git:config.json"},
		{Name: "tokenizer.json", Sha256: h[:], Size: 8, Source: "git:tokenizer.json"},
	}}
	if err := core.Sign(m, publisher.key); err != nil {
		t.Fatal(err)
	}
	root, _ := core.Digest(m)
	g := &pb.ChainGenesis{ChainId: id, ModelManifestRoot: root, TokenizerHash: h[:], ExecutionProfileVer: "cpu-test-v1", PriceOutput: 1}
	if err := core.Sign(g, publisher.key); err != nil {
		t.Fatal(err)
	}
	gRaw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(g)
	mRaw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	gh := sha256.Sum256(gRaw)
	ph, _ := core.Digest(g)
	return Action{Type: "register_model", ModelID: id, GenesisHash: hex.EncodeToString(gh[:]), ProfileHash: hex.EncodeToString(ph), Genesis: gRaw, Manifest: mRaw,
		Origin: &Origin{Repository: "https://example.org/model", Revision: "1111111111111111111111111111111111111111"}}
}

func rejected(t *testing.T, s *State, a actor, action Action, height int64) {
	t.Helper()
	before, _ := s.Bytes()
	raw, _ := Sign(s.Parameters.ChainID, a.key, s.Accounts[a.id].Nonce+1, action)
	if _, err := s.Apply(raw, height); err == nil {
		t.Fatal("invalid action accepted")
	}
	after, _ := s.Bytes()
	if !bytes.Equal(before, after) {
		t.Fatal("rejection mutated consensus state")
	}
}

func TestPermissionlessBondEndorsementTrust(t *testing.T) {
	s, a := testState(t)
	pub := a["payer"] // no governor key
	action := publicationAction(t, pub, "ember-new")
	apply(t, s, pub, 1, action, 1)
	m := s.Models[action.ModelID]
	if s.Accounts[pub.id].Balance != 900 || m.Publication.Bond != 100 {
		t.Fatal("bond not locked")
	}
	if s.CanServeModel(m.ID, nil) == nil {
		t.Fatal("unendorsed model served by default")
	}
	root := m.Publication.ManifestRoot
	policy := &TrustPolicy{Profiles: map[string]ProfileTrust{m.ID: {MinEndorsers: 1}}}
	if s.CanServeModel(m.ID, policy) == nil {
		t.Fatal("unendorsed threshold passed")
	}
	apply(t, s, a["auditor"], 1, Action{Type: "endorse_profile", ProfileHash: root}, 2)
	if s.CanServeModel(m.ID, policy) != nil || s.CanServeModel(m.ID, nil) == nil {
		t.Fatal("endorsement trust OR semantics failed")
	}
	rejected(t, s, a["auditor"], Action{Type: "endorse_profile", ProfileHash: root}, 3)
	apply(t, s, a["auditor"], 2, Action{Type: "revoke_endorsement", ProfileHash: root}, 3)
	if s.CanServeModel(m.ID, policy) == nil {
		t.Fatal("revocation ignored")
	}
	apply(t, s, a["governor"], 1, Action{Type: "endorse_profile", ProfileHash: root}, 4)
	if err := s.CanServeModel(m.ID, nil); err != nil {
		t.Fatal(err)
	}
	policy.Profiles[m.ID] = ProfileTrust{Publishers: []string{pub.id}, MinEndorsers: 5}
	if err := s.CanServeModel(m.ID, policy); err != nil {
		t.Fatal(err)
	}
	rejected(t, s, pub, publicationAction(t, pub, "ember-second"), 5)
	rejected(t, s, pub, Action{Type: "claim_publisher_bond", ModelID: m.ID}, 10)
	apply(t, s, pub, 2, Action{Type: "claim_publisher_bond", ModelID: m.ID}, 101)
	if s.Accounts[pub.id].Balance != 1000 || s.Models[m.ID].Publication.Bond != 0 {
		t.Fatal("bond not refunded")
	}
	rejected(t, s, pub, Action{Type: "claim_publisher_bond", ModelID: m.ID}, 102)
}

func TestPublicationRejectsJunkAndWrongSigner(t *testing.T) {
	s, a := testState(t)
	action := publicationAction(t, a["payer"], "ember-new")
	action.GenesisHash = "1111111111111111111111111111111111111111111111111111111111111111"
	rejected(t, s, a["payer"], action, 1)
	action = publicationAction(t, a["payer"], "ember-new")
	rejected(t, s, a["auditor"], action, 1)
	action.Manifest[len(action.Manifest)-1] ^= 1
	rejected(t, s, a["payer"], action, 1)
}

func TestPinnedOriginFraudAndForgedProof(t *testing.T) {
	s, a := testState(t)
	action := publicationAction(t, a["payer"], "ember-fraud")
	blob := []byte("actual origin bytes")
	tree := append([]byte("100644 config.json\x00"), gitHash("blob", blob)...)
	commit := []byte("tree " + hex.EncodeToString(gitHash("tree", tree)) + "\nauthor publisher\n\nrelease\n")
	action.Origin.Revision = hex.EncodeToString(gitHash("commit", commit))
	apply(t, s, a["payer"], 1, action, 1)
	root := s.Models[action.ModelID].Publication.ManifestRoot
	apply(t, s, a["auditor2"], 1, Action{Type: "endorse_profile", ProfileHash: root}, 1)
	proof := &OriginProof{Artifact: "config.json", Commit: commit, Trees: [][]byte{tree}, Blob: []byte("forged")}
	challenge := Action{Type: "challenge_publication", ModelID: action.ModelID, OriginProof: proof}
	rejected(t, s, a["auditor"], challenge, 2)
	proof.Blob = blob
	apply(t, s, a["auditor"], 1, challenge, 2)
	apply(t, s, a["auditor2"], 2, Action{Type: "revoke_endorsement", ProfileHash: root}, 3)
	p := s.Models[action.ModelID].Publication
	if !p.Invalid || p.Bond != 0 || s.PublisherForfeited != 100 || s.Accounts[a["payer"].id].Balance != 900 {
		t.Fatal("fraud did not forfeit bond")
	}
	policy := &TrustPolicy{Profiles: map[string]ProfileTrust{action.ModelID: {Publishers: []string{a["payer"].id}}}}
	if s.CanServeModel(action.ModelID, policy) == nil {
		t.Fatal("invalid publication served")
	}
	rejected(t, s, a["payer"], Action{Type: "claim_publisher_bond", ModelID: action.ModelID}, 200)
}

func TestPublisherEarlyRefundAfterAuditedSettlements(t *testing.T) {
	s, a := testState(t)
	action := publicationAction(t, a["payer"], "ember-settled")
	apply(t, s, a["payer"], 1, action, 1)
	// Exercise the settlement accounting path, not a counter-only refund test.
	for _, id := range []string{"one", "two", "three"} {
		h := &Hold{ID: id, Payer: a["coordinator"].id, Node: a["node"].id, ModelID: action.ModelID, Amount: 1, Actual: 1, State: "dispute", AuditVerdict: "accept"}
		if err := s.settle(h, "accept"); err != nil {
			t.Fatal(err)
		}
	}
	apply(t, s, a["payer"], 2, Action{Type: "claim_publisher_bond", ModelID: action.ModelID}, 2)
	if s.Accounts[a["payer"].id].Balance != 1000 {
		t.Fatal("settled-request bond refund failed")
	}
}

func TestPublishDynamicComputeTable(t *testing.T) {
	for _, tc := range []struct {
		name            string
		layers          []uint64
		embedding, head uint64
		valid           bool
	}{
		{"legacy", nil, 0, 0, true},
		{"wide", []uint64{1 << 33, 2 << 33}, 0, 3 << 33, true},
		{"short", []uint64{1}, 0, 1, false},
		{"long", []uint64{1, 2, 3}, 0, 1, false},
		{"overflow", []uint64{^uint64(0), 1}, 0, 1, false},
		{"zero-layer", []uint64{0, 1}, 0, 1, false},
		{"zero-head", []uint64{1, 1}, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, a := testState(t)
			pub := a["payer"]
			action := publicationAction(t, pub, "ember-compute")
			m := &pb.ModelManifest{}
			proto.Unmarshal(action.Manifest, m)
			m.DynamicRanges = true
			m.TotalLayers = 2
			m.MinStageLayers = 1
			m.LayerFlops = tc.layers
			m.EmbeddingFlops = tc.embedding
			m.HeadFlops = tc.head
			core.Sign(m, pub.key)
			root, _ := core.Digest(m)
			g := &pb.ChainGenesis{}
			proto.Unmarshal(action.Genesis, g)
			g.ModelManifestRoot = root
			core.Sign(g, pub.key)
			action.Genesis, _ = proto.Marshal(g)
			action.Manifest, _ = proto.Marshal(m)
			gh := sha256.Sum256(action.Genesis)
			ph, _ := core.Digest(g)
			action.GenesisHash = hex.EncodeToString(gh[:])
			action.ProfileHash = hex.EncodeToString(ph)
			if tc.valid {
				apply(t, s, pub, 1, action, 1)
			} else {
				rejected(t, s, pub, action, 1)
			}
		})
	}
}

// customPublicationAction builds a register_model action whose manifest
// carries exactly the given artifacts; manifest ConfigHash is
// sha256("artifact") and the genesis tokenizer hash is caller-chosen.
func customPublicationAction(t *testing.T, publisher actor, id string, artifacts []*pb.Artifact, tokenizerHash []byte) Action {
	t.Helper()
	h := sha256.Sum256([]byte("artifact"))
	m := &pb.ModelManifest{ChainId: id, ConfigHash: h[:], GraphHash: h[:], Artifacts: artifacts}
	if err := core.Sign(m, publisher.key); err != nil {
		t.Fatal(err)
	}
	root, _ := core.Digest(m)
	g := &pb.ChainGenesis{ChainId: id, ModelManifestRoot: root, TokenizerHash: tokenizerHash, ExecutionProfileVer: "cpu-test-v1", PriceOutput: 1}
	if err := core.Sign(g, publisher.key); err != nil {
		t.Fatal(err)
	}
	gRaw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(g)
	mRaw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	gh := sha256.Sum256(gRaw)
	ph, _ := core.Digest(g)
	return Action{Type: "register_model", ModelID: id, GenesisHash: hex.EncodeToString(gh[:]), ProfileHash: hex.EncodeToString(ph), Genesis: gRaw, Manifest: mRaw,
		Origin: &Origin{Repository: "https://example.org/model", Revision: "1111111111111111111111111111111111111111"}}
}

func rejectedWith(t *testing.T, s *State, a actor, action Action, height int64, want string) {
	t.Helper()
	before, _ := s.Bytes()
	raw, _ := Sign(s.Parameters.ChainID, a.key, s.Accounts[a.id].Nonce+1, action)
	_, err := s.Apply(raw, height)
	if err == nil {
		t.Fatal("invalid action accepted")
	}
	if !bytes.Contains([]byte(err.Error()), []byte(want)) {
		t.Fatalf("rejection error %q does not mention %q", err.Error(), want)
	}
	after, _ := s.Bytes()
	if !bytes.Equal(before, after) {
		t.Fatal("rejection mutated consensus state")
	}
}

// Tokenizer binding is by genesis hash, not by file name (train/next):
// tiktoken layouts ship tiktoken.model and no tokenizer.json.
func TestPublishTokenizerHashBinding(t *testing.T) {
	cfgHash := sha256.Sum256([]byte("artifact"))
	tokHash := sha256.Sum256([]byte("tokenizer-bytes"))
	otherHash := sha256.Sum256([]byte("other-bytes"))
	mk := func(name string, hash [32]byte) *pb.Artifact {
		return &pb.Artifact{Name: name, Sha256: hash[:], Size: 8, Source: "git:" + name}
	}

	// 1. tiktoken layout: tokenizer artifact named tiktoken.model whose
	// hash matches the genesis tokenizer hash -> accepted.
	s, a := testState(t)
	pub := a["payer"]
	action := customPublicationAction(t, pub, "ember-tiktoken", []*pb.Artifact{
		mk("config.json", cfgHash), mk("tiktoken.model", tokHash),
	}, tokHash[:])
	apply(t, s, pub, 1, action, 1)
	if s.Models["ember-tiktoken"].Publication == nil {
		t.Fatal("tiktoken-layout publication not recorded")
	}

	// 2. No artifact hash matches the genesis tokenizer hash -> rejected
	// with a diagnosable hash-mismatch error, state untouched.
	s, a = testState(t)
	pub = a["payer"]
	action = customPublicationAction(t, pub, "ember-notokenizer", []*pb.Artifact{
		mk("config.json", cfgHash), mk("tiktoken.model", otherHash),
	}, tokHash[:])
	rejectedWith(t, s, pub, action, 1, "no artifact sha256 matches genesis tokenizer hash")

	// 2b. Legacy name with a wrong hash no longer suffices on its own.
	s, a = testState(t)
	pub = a["payer"]
	action = customPublicationAction(t, pub, "ember-wrongtok", []*pb.Artifact{
		mk("config.json", cfgHash), mk("tokenizer.json", otherHash),
	}, tokHash[:])
	rejectedWith(t, s, pub, action, 1, "no artifact sha256 matches genesis tokenizer hash")

	// 3. Original tokenizer.json layout -> still accepted (no regression).
	s, a = testState(t)
	pub = a["payer"]
	action = customPublicationAction(t, pub, "ember-legacy", []*pb.Artifact{
		mk("config.json", cfgHash), mk("tokenizer.json", tokHash),
	}, tokHash[:])
	apply(t, s, pub, 1, action, 1)
	if s.Models["ember-legacy"].Publication == nil {
		t.Fatal("legacy tokenizer.json publication not recorded")
	}

	// 4. Config artifact missing -> still rejected.
	s, a = testState(t)
	pub = a["payer"]
	action = customPublicationAction(t, pub, "ember-noconfig", []*pb.Artifact{
		mk("tiktoken.model", tokHash),
	}, tokHash[:])
	rejectedWith(t, s, pub, action, 1, "config artifact required")
}
