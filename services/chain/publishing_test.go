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
