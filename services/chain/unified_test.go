package chain

import (
	"bytes"
	"crypto/sha256"
	core "ember/core-go"
	pb "ember/proto"
	"encoding/hex"
	"google.golang.org/protobuf/proto"
	"strings"
	"testing"
)

func weightedPublication(t *testing.T, who actor, id string) Action {
	a := publicationAction(t, who, id)
	m := &pb.ModelManifest{}
	proto.Unmarshal(a.Manifest, m)
	m.CanonicalConfig = []byte(`{"hidden_size":8,"num_hidden_layers":2}`)
	m.Artifacts = append(m.Artifacts, &pb.Artifact{Name: "model.safetensors", Sha256: bytes.Repeat([]byte{42}, 32), Size: 100, Source: "git:model.safetensors"})
	return resignPublication(t, who, a, m)
}
func resignPublication(t *testing.T, who actor, a Action, m *pb.ModelManifest) Action {
	t.Helper()
	if len(m.CanonicalConfig) > 0 {
		m.ConfigBytes = append([]byte(nil), m.CanonicalConfig...)
		hash := sha256.Sum256(m.ConfigBytes)
		m.ConfigHash = hash[:]
		for _, artifact := range m.Artifacts {
			if artifact.Name == "config.json" {
				artifact.Sha256 = hash[:]
				artifact.Size = int64(len(m.ConfigBytes))
			}
		}
	}
	core.Sign(m, who.key)
	a.Manifest, _ = proto.Marshal(m)
	g := &pb.ChainGenesis{}
	proto.Unmarshal(a.Genesis, g)
	g.ModelManifestRoot, _ = core.Digest(m)
	core.Sign(g, who.key)
	a.Genesis, _ = proto.Marshal(g)
	gh := sha256.Sum256(a.Genesis)
	a.GenesisHash = hex.EncodeToString(gh[:])
	ph, _ := core.Digest(g)
	a.ProfileHash = hex.EncodeToString(ph)
	return a
}
func TestUnifiedPublicationAndFreeze(t *testing.T) {
	s, a := testState(t)
	first := weightedPublication(t, a["payer"], "unified-a")
	apply(t, s, a["payer"], 1, first, 1)
	duplicate := weightedPublication(t, a["payer"], "unified-b")
	raw, _ := Sign(s.Parameters.ChainID, a["payer"].key, 2, duplicate)
	if _, err := s.Apply(raw, 20); err == nil || !strings.Contains(err.Error(), "unified-a") {
		t.Fatalf("duplicate identity: %v", err)
	}
	apply(t, s, a["governor"], 1, Action{Type: "freeze_model_registration", ModelID: first.ModelID}, 21)
	m := &pb.ModelManifest{}
	g := &pb.ChainGenesis{}
	proto.Unmarshal(first.Manifest, m)
	proto.Unmarshal(first.Genesis, g)
	p := &pb.ResourceProfile{NodeId: a["node"].id, ChainId: first.ModelID, ManifestRoot: g.ModelManifestRoot, ExecutionProfileVer: g.ExecutionProfileVer}
	core.Sign(p, a["node"].key)
	pr, _ := proto.Marshal(p)
	rejected(t, s, a["node"], Action{Type: "register_node", Profile: pr, ExpiresHeight: 90}, 22)
	if !s.Models[first.ModelID].RegistrationFrozen {
		t.Fatal("freeze missing")
	}
}

func TestPinnedSources(t *testing.T) {
	for _, rev := range []string{"main", "latest", "1234"} {
		if validateRangeRules(&pb.ModelManifest{Sources: []*pb.SourceLocator{{Repository: "https://huggingface.co/example/model", Revision: rev}}}) == nil {
			t.Fatal("floating source accepted")
		}
	}
	if err := validateRangeRules(&pb.ModelManifest{Sources: []*pb.SourceLocator{{Repository: "https://huggingface.co/example/model", Revision: strings.Repeat("a", 40)}}}); err != nil {
		t.Fatal(err)
	}
}

func TestModelAndSchemeIdentity(t *testing.T) {
	s, a := testState(t)
	act := weightedPublication(t, a["payer"], "identity")
	m := &pb.ModelManifest{}
	g := &pb.ChainGenesis{}
	proto.Unmarshal(act.Manifest, m)
	proto.Unmarshal(act.Genesis, g)
	id, err := identityFor(g, m)
	if err != nil || id == nil {
		t.Fatal(err)
	}
	g.TokenizerHash = bytes.Repeat([]byte{5}, 32)
	other, _ := identityFor(g, m)
	if id.ModelHash == other.ModelHash {
		t.Fatal("tokenizer ignored")
	}
	one := &pb.LayoutScheme{TotalLayers: 2, MinStageLayers: 1, AllowedLayerCuts: []uint32{0, 1, 2}, LayerFlops: []uint64{1, 1}, EmbeddingFlops: 1, HeadFlops: 1}
	for i := uint32(0); i < 4; i++ {
		start, end := uint32(0), uint32(0)
		kind := "embedding"
		if i == 3 {
			start = 2
			end = 2
			kind = "lm_head"
		} else if i > 0 {
			start = i - 1
			end = i
			kind = "layers"
		}
		one.Atoms = append(one.Atoms, &pb.ShardSpec{Index: i, UnitKind: kind, StartLayer: start, EndLayer: end, HasEmbedding: i == 0, HasLmHead: i == 3, ParametersHash: bytes.Repeat([]byte{byte(i + 1)}, 32)})
	}
	two := proto.Clone(one).(*pb.LayoutScheme)
	two.LayerFlops = []uint64{2, 2}
	m.Schemes = []*pb.LayoutScheme{one, two}
	act = resignPublication(t, a["payer"], act, m)
	apply(t, s, a["payer"], 1, act, 1)
	proto.Unmarshal(act.Genesis, g)
	for i, who := range []actor{a["node"], a["auditor"]} {
		s.Accounts[who.id].Stake = 5000
		p := &pb.ResourceProfile{NodeId: who.id, ChainId: act.ModelID, ManifestRoot: g.ModelManifestRoot, ExecutionProfileVer: g.ExecutionProfileVer, SchemeHash: schemeHash(m.Schemes[i]), ShardDynamicRange: true, ShardUnitKind: "range", ShardTotalLayers: 2, ShardStartLayer: uint32(i), ShardEndLayer: uint32(i + 1), ShardHasEmbedding: i == 0, ShardHasLmHead: i == 1, ShardParametersHash: core.RangeParameters(g.ModelManifestRoot, uint32(i), uint32(i+1)), EngineCapabilities: &pb.EngineCapabilities{Roles: []string{"shard-stage"}}}
		core.Sign(p, who.key)
		raw, _ := proto.Marshal(p)
		apply(t, s, who, 1, Action{Type: "register_node", Profile: raw, ExpiresHeight: 90}, 2)
	}
	freeze := Action{Type: "freeze_pipeline", HoldID: "cross-scheme", Payer: a["payer"].id, Amount: 10, TotalLayers: 2, PipelineAuthorizations: []PipelineAuthorization{{NodeID: a["node"].id, StartLayer: 0, EndLayer: 1}, {NodeID: a["auditor"].id, StartLayer: 1, EndLayer: 2}}}
	raw, _ := Sign(s.Parameters.ChainID, a["payer"].key, 2, freeze)
	if _, err := s.Apply(raw, 3); err == nil || !strings.Contains(err.Error(), "cross-scheme") {
		t.Fatalf("mixed layout: %v", err)
	}
}

func TestIdentityConfigMustMatchArtifact(t *testing.T) {
	_, a := testState(t)
	act := weightedPublication(t, a["payer"], "config-anchor")
	m := &pb.ModelManifest{}
	g := &pb.ChainGenesis{}
	proto.Unmarshal(act.Manifest, m)
	proto.Unmarshal(act.Genesis, g)
	m.ConfigBytes = []byte(`{"hidden_size":999}`)
	if _, err := identityFor(g, m); err == nil {
		t.Fatal("invented config accepted")
	}
}

func TestMoeSchemeCoverageAndSettlement(t *testing.T) {
	f := moeChainState(t)
	sc := &pb.LayoutScheme{TotalLayers: 2, Atoms: f.manifest.Shards, MoeLayout: proto.Clone(f.manifest.MoeLayout).(*pb.MoeLayout)}
	other := proto.Clone(sc).(*pb.LayoutScheme)
	other.MoeLayout.RoutingCapBeta++
	f.manifest.Schemes = []*pb.LayoutScheme{sc, other}
	core.Sign(f.manifest, f.a["governor"].key)
	f.root, _ = core.Digest(f.manifest)
	f.genesis.ModelManifestRoot = f.root
	core.Sign(f.genesis, f.a["governor"].key)
	model := f.s.Models[f.manifest.ChainId]
	model.Manifest, _ = proto.Marshal(f.manifest)
	model.Genesis, _ = proto.Marshal(f.genesis)
	mh := sha256.Sum256(model.Manifest)
	gh := sha256.Sum256(model.Genesis)
	model.ProfileHash = hex.EncodeToString(mh[:])
	model.GenesisHash = hex.EncodeToString(gh[:])
	f.s.Models[model.ID] = model
	for _, who := range []actor{f.attn, f.expB, f.expC, f.expB2, f.expC2} {
		p := &pb.ResourceProfile{}
		proto.Unmarshal(f.s.Nodes[who.id].Profile, p)
		p.ManifestRoot = f.root
		p.SchemeHash = schemeHash(sc)
		core.Sign(p, who.key)
		raw, _ := proto.Marshal(p)
		f.s.Nodes[who.id].Profile = raw
		if err := f.s.validateMoeRegistration(p); err != nil {
			t.Fatal(err)
		}
	}
	original := f.s.Nodes[f.expB2.id].Profile
	p := &pb.ResourceProfile{}
	proto.Unmarshal(original, p)
	p.SchemeHash = schemeHash(other)
	core.Sign(p, f.expB2.key)
	f.s.Nodes[f.expB2.id].Profile, _ = proto.Marshal(p)
	counts := f.s.moeHolderCounts(model.ID, 2, schemeHash(sc))
	if counts[[2]uint32{0, 0}] >= 2 {
		t.Fatal("cross-scheme coverage counted")
	}
	f.s.Nodes[f.expB2.id].Profile = original
	f.freezeSubmit(t)
	h := f.s.Holds["moe-req"]
	if !bytes.Equal(h.Moe.SchemeHash, schemeHash(sc)) {
		t.Fatal("scheme not frozen")
	}
	f.audit(t, f.auditorActor(t), 1, 5, "accept", nil, nil)
	h = f.s.Holds["moe-req"]
	receipts, err := ExpectedAuditReceipts(h, "accept")
	if err != nil {
		t.Fatal(err)
	}
	apply(t, f.s, f.a["coordinator"], 3, Action{Type: "settle", HoldID: h.ID, Outcome: "accept", AuditReceipts: receipts}, 6)
	if f.s.Holds[h.ID].State != "settled" {
		t.Fatal("scheme settlement")
	}
}

func TestCanonicalConfigPublisherBytes(t *testing.T) {
	raw, err := CanonicalModelConfig([]byte("{\"z\": 1.0, \"epsilon\":1e-6, \"layers\":2}"))
	if err != nil || string(raw) != `{"epsilon":0.000001,"layers":2,"z":1}` {
		t.Fatalf("%s %v", raw, err)
	}
	if _, err := CanonicalModelConfig([]byte(`{"layers":2} {}`)); err == nil {
		t.Fatal("trailing config accepted")
	}
}
