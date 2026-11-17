package chain

import (
	"bytes"
	"crypto/sha256"
	core "ember/core-go"
	"ember/internal/computefixtures"
	pb "ember/proto"
	"encoding/hex"
	"google.golang.org/protobuf/proto"
	"reflect"
	"strings"
	"testing"
)

func computePublication(t *testing.T, who actor, id string, in core.DerivationInput) Action {
	t.Helper()
	a := publicationAction(t, who, id)
	m := &pb.ModelManifest{}
	proto.Unmarshal(a.Manifest, m)
	r, e := core.DeriveCompute(in)
	if e != nil {
		t.Fatal(e)
	}
	m.CanonicalConfig, e = CanonicalModelConfig(in.ConfigJSON)
	if e != nil {
		t.Fatal(e)
	}
	m.TotalLayers = uint32(len(r.LayerFlops))
	m.LayerFlops = r.LayerFlops
	m.HeadFlops = r.HeadFlops
	m.Artifacts = append(m.Artifacts, &pb.Artifact{Name: "model.safetensors", Sha256: bytes.Repeat([]byte{42}, 32), Size: int64(r.TensorBytes + 1000000), Source: "git:model.safetensors"})
	a = resignPublication(t, who, a, m)
	a.TensorShapes = in.Tensors
	a.DeclaredTotalParams = r.TotalParams
	a.DeclaredActiveParams = r.ActiveParams
	a.DeclaredFlopsPerToken = r.FlopsPerToken
	return a
}
func TestV11KimiPublication(t *testing.T) {
	for _, tc := range []string{"correct", "floor1", "wrongtable", "wrongtotal", "hidden", "bytes", "missing", "floor2", "scheme", "configHash"} {
		t.Run(tc, func(t *testing.T) {
			s, actors := testState(t)
			s.Version = 11
			who := actors["payer"]
			a := computePublication(t, who, "kimi-v11", computefixtures.Kimi())
			m := &pb.ModelManifest{}
			proto.Unmarshal(a.Manifest, m)
			switch tc {
			case "floor1":
				m.EmbeddingFlops = 1
			case "wrongtable":
				m.LayerFlops[0] += 250580697088 - a.DeclaredFlopsPerToken
			case "wrongtotal":
				a.DeclaredFlopsPerToken = 250580697088
			case "hidden":
				a.TensorShapes[0].Shape = []uint64{163840, 7167}
			case "bytes":
				m.Artifacts[len(m.Artifacts)-1].Size = 1
			case "missing":
				a.TensorShapes = nil
			case "floor2":
				m.EmbeddingFlops = 2
			case "scheme":
				m.Schemes = []*pb.LayoutScheme{{TotalLayers: m.TotalLayers}}
			}
			a = resignPublication(t, who, a, m)
			if tc == "configHash" {
				m.ConfigBytes = []byte(`{}`)
				core.Sign(m, who.key)
				a.Manifest, _ = proto.Marshal(m)
			}
			if tc != "correct" && tc != "floor1" {
				rejected(t, s, who, a, 1)
				return
			}
			apply(t, s, who, 1, a, 1)
			model := s.Models[a.ModelID]
			v := model.VerifiedCompute
			root, _ := core.ShapesRoot(a.TensorShapes)
			if v == nil || v.FlopsPerToken != 208736282112 || v.TotalParams != 2780110363392 || v.ActiveParams != 105542546176 || v.ShapesRoot != root || model.Publication.ShapesRoot != root || v.CorrectedHeight != 0 {
				t.Fatal("missing compute anchors", v)
			}
			if e := validateModelArtifacts(model); e != nil {
				t.Fatal("replay", e)
			}
		})
	}
}
func TestV10ComputeFieldsIgnored(t *testing.T) {
	s, actors := testState(t)
	s.Version = 10
	who := actors["payer"]
	a := weightedPublication(t, who, "v10-shapes")
	b := a
	b.TensorShapes = []core.TensorShape{{Name: "junk", Shape: []uint64{0}, Dtype: "invalid"}}
	b.DeclaredTotalParams = 1
	b.DeclaredActiveParams = 2
	b.DeclaredFlopsPerToken = 3
	baseline, err := s.Clone()
	if err != nil {
		t.Fatal(err)
	}
	if e := s.publish(who.id, a, 1); e != nil {
		t.Fatal(e)
	}
	if e := baseline.publish(who.id, b, 1); e != nil {
		t.Fatal(e)
	}
	one, _ := s.Bytes()
	two, _ := baseline.Bytes()
	if !bytes.Equal(one, two) || bytes.Contains(one, []byte("verified_compute")) || bytes.Contains(one, []byte("shapes_root")) {
		t.Fatal("legacy state JSON changed")
	}
}
func TestV11GovernanceComputeCorrection(t *testing.T) {
	s, actors := testState(t)
	s.Version = 10
	who := actors["payer"]
	a := computePublication(t, who, "kimi-correction", computefixtures.Kimi())
	m := &pb.ModelManifest{}
	proto.Unmarshal(a.Manifest, m)
	m.LayerFlops[0] += 250580697088 - a.DeclaredFlopsPerToken
	a = resignPublication(t, who, a, m)
	apply(t, s, who, 1, a, 1)
	original := s.Models[a.ModelID]
	config := append([]byte(nil), original.Publication.Manifest...)
	correction := Action{Type: "correct_model_compute", ModelID: a.ModelID, TensorShapes: a.TensorShapes, DeclaredTotalParams: a.DeclaredTotalParams, DeclaredActiveParams: a.DeclaredActiveParams, DeclaredFlopsPerToken: a.DeclaredFlopsPerToken, Reason: "tensor-truth Kimi correction"}
	rejected(t, s, actors["governor"], correction, 2)
	s.Version = 11
	rejected(t, s, who, correction, 2)
	bad := correction
	bad.DeclaredActiveParams++
	rejected(t, s, actors["governor"], bad, 2)
	bad = correction
	bad.TensorShapes = append([]core.TensorShape(nil), correction.TensorShapes...)
	bad.TensorShapes[0].Shape = []uint64{1, 1}
	rejected(t, s, actors["governor"], bad, 2)
	bad = correction
	bad.ModelID = "absent"
	rejected(t, s, actors["governor"], bad, 2)
	apply(t, s, actors["governor"], 1, correction, 2)
	model := s.Models[a.ModelID]
	v := model.VerifiedCompute
	if v == nil || v.CorrectedHeight != 2 || v.FlopsPerToken != 208736282112 || len(model.ComputeCorrections) != 1 {
		t.Fatal("correction missing")
	}
	history := model.ComputeCorrections[0]
	if history.OldFlopsPerToken != 250580697088 || history.NewFlopsPerToken != 208736282112 || history.OldTotalParams != 0 || history.Governor != actors["governor"].id || history.Reason != correction.Reason {
		t.Fatal(history)
	}
	if !bytes.Equal(config, model.Publication.Manifest) || !reflect.DeepEqual(model.Publication, original.Publication) || model.GenesisHash != original.GenesisHash || model.ProfileHash != original.ProfileHash || !reflect.DeepEqual(model.Pricing, original.Pricing) {
		t.Fatal("correction changed identity/materials")
	}
	// Recompute credits immediately after correction using the original skewed
	// manifest and two range authorizations. Amount=F+lookup avoids rounding.
	receipt := &pb.PipelineExecutionReceipt{StageReceipts: []*pb.ShardStepReceipt{{NodeId: "first", StartLayer: 0, EndLayer: 1}, {NodeId: "rest", StartLayer: 1, EndLayer: 92}}}
	receiptRaw, _ := proto.Marshal(receipt)
	first, _ := proto.Marshal(&pb.ResourceProfile{ShardHasEmbedding: true})
	rest, _ := proto.Marshal(&pb.ResourceProfile{ShardHasLmHead: true})
	hold := &Hold{ModelID: a.ModelID, Pipeline: &PipelineHold{TotalLayers: 92, Receipt: receiptRaw, Authorizations: []PipelineAuthorization{{NodeID: "first", StartLayer: 0, EndLayer: 1, Profile: first}, {NodeID: "rest", StartLayer: 1, EndLayer: 92, Profile: rest}}}}
	shares, e := s.pipelineExecutorCredits(hold, int64(v.FlopsPerToken+1))
	if e != nil {
		t.Fatal(e)
	}
	for _, share := range shares {
		want := v.LayerFlops[0] + 1
		if share.NodeID == "rest" {
			want = v.FlopsPerToken - v.LayerFlops[0]
		}
		if share.Amount != int64(want) {
			t.Fatal("correction did not switch settlement", shares)
		}
	}
	effective := s.verifiedComputeManifest(model, m)
	if core.TotalCompute(effective) != 208736282113 || reflect.DeepEqual(effective.LayerFlops, m.LayerFlops) {
		t.Fatal("corrected table not selected")
	}
	apply(t, s, actors["governor"], 2, correction, 3)
	history = s.Models[a.ModelID].ComputeCorrections[1]
	if history.OldTotalParams != a.DeclaredTotalParams || history.OldFlopsPerToken != 208736282112 {
		t.Fatal("history not appended from prior verified values")
	}
}
func TestV11VerifiedPipelineWeights(t *testing.T) {
	s, actors, nodes, auth, r := pipelineState(t)
	s.Version = 11
	id := "ember-unit-test"
	model := s.Models[id]
	// Construct a signed skewed table, then keep its anchors unchanged while
	// verified compute supplies the settlement weights.
	manifest := &pb.ModelManifest{}
	proto.Unmarshal(model.Manifest, manifest)
	manifest.LayerFlops = []uint64{1, 1, 1, 1}
	manifest.HeadFlops = 1
	manifest.EmbeddingFlops = 1
	core.Sign(manifest, actors["governor"].key)
	model.Manifest, _ = proto.Marshal(manifest)
	root, _ := core.Digest(manifest)
	genesis := &pb.ChainGenesis{}
	proto.Unmarshal(model.Genesis, genesis)
	genesis.ModelManifestRoot = root
	core.Sign(genesis, actors["governor"].key)
	model.Genesis, _ = proto.Marshal(genesis)
	gh := sha256.Sum256(model.Genesis)
	mh := sha256.Sum256(model.Manifest)
	model.GenesisHash = hex.EncodeToString(gh[:])
	model.ProfileHash = hex.EncodeToString(mh[:])
	for i, node := range nodes {
		profile := &pb.ResourceProfile{}
		proto.Unmarshal(s.Nodes[node.id].Profile, profile)
		profile.ManifestRoot = root
		core.Sign(profile, node.key)
		s.Nodes[node.id].Profile, _ = proto.Marshal(profile)
		r.StageReceipts[i].ManifestRoot = root
		core.Sign(r.StageReceipts[i], node.key)
	}
	model.VerifiedCompute = &VerifiedCompute{TotalParams: 101, ActiveParams: 101, FlopsPerToken: 100, LayerFlops: []uint64{10, 20, 30, 10}, HeadFlops: 30}
	s.Models[id] = model
	apply(t, s, actors["payer"], 1, Action{Type: "freeze_pipeline", HoldID: "units", Payer: actors["payer"].id, Amount: 101, TotalLayers: 4, PipelineAuthorizations: auth}, 2)
	h := s.Holds["units"]
	for i, want := range []int64{1, 70, 30} {
		if h.Pipeline.Authorizations[i].Reserved != want {
			t.Fatal("exposure did not use verified compute", h.Pipeline.Authorizations)
		}
	}
	raw, _ := proto.Marshal(r)
	apply(t, s, actors["coordinator"], 1, Action{Type: "submit_pipeline", HoldID: "units", Actual: 101, Receipt: raw}, 3)
	h = s.Holds["units"]
	for i, want := range []int64{1, 70, 30} {
		for _, credit := range h.Pipeline.Credits {
			if credit.NodeID == nodes[i].id && credit.Amount != want {
				t.Fatal("submit weights", h.Pipeline.Credits)
			}
		}
	}
	shares, e := s.pipelineExecutorCredits(h, 101)
	if e != nil {
		t.Fatal(e)
	}
	for i, want := range []int64{1, 70, 30} {
		found := false
		for _, share := range shares {
			if share.NodeID == nodes[i].id {
				found = true
				if share.Amount != want {
					t.Fatal("wrong verified allocation", shares)
				}
			}
		}
		if !found {
			t.Fatal("missing allocation")
		}
	}
	s.Version = 10
	legacy := s.verifiedComputeManifest(model, &pb.ModelManifest{})
	if len(legacy.LayerFlops) != 0 {
		t.Fatal("v10 switch enabled")
	}
}
func TestV11TiedHeadAndDerivationErrors(t *testing.T) {
	s, actors := testState(t)
	s.Version = 11
	a := computePublication(t, actors["payer"], "qwen25-tied", computefixtures.Model("qwen25"))
	apply(t, s, actors["payer"], 1, a, 1)
	if e := validateModelArtifacts(s.Models[a.ModelID]); e != nil {
		t.Fatal(e)
	}
	a.ModelID = "another"
	m := &pb.ModelManifest{}
	proto.Unmarshal(a.Manifest, m)
	a.TensorShapes = nil
	if _, e := derivePublicationCompute(m, a, true, 11); e == nil || !strings.Contains(e.Error(), "compute derivation mismatch") {
		t.Fatal(e)
	}
}

func TestV11TiedHeadOverflowBoundary(t *testing.T) {
	m := &pb.ModelManifest{TotalLayers: 1, LayerFlops: []uint64{^uint64(0) - 1}}
	if e := validateComputeRangeRules(m, true); e != nil {
		t.Fatal("valid lookup+layer boundary rejected", e)
	}
	if e := validateComputeRangeRules(m, false); e == nil {
		t.Fatal("v10 accepted zero head")
	}
	m.LayerFlops[0] = ^uint64(0)
	if e := validateComputeRangeRules(m, true); e == nil {
		t.Fatal("overflow accepted")
	}
}

// Both consensus entry points must select the state version, not binary defaults.
func TestKimiRealConsensusVersionGate(t *testing.T) {
	for _, version := range []uint64{11, 12} {
		in := computefixtures.KimiReal()
		in.AppVersion = 12
		s, actors := testState(t)
		who := actors["payer"]
		a := computePublication(t, who, "kimi-real-gate", in)
		s.Version = version
		if version == 11 {
			if err := s.publish(who.id, a, 1); err == nil {
				t.Fatal("v11 accepted v12 declared compute")
			}
		} else {
			if err := s.publish(who.id, a, 1); err != nil {
				t.Fatal(err)
			}
			if s.Models[a.ModelID].VerifiedCompute.ActiveParams != 105542546176 {
				t.Fatal("publication did not select v12")
			}
		}
		// Import the original publication under pre-verification rules, then
		// exercise correction independently of the publication entry point.
		s, actors = testState(t)
		s.Version = 10
		if err := s.publish(who.id, a, 1); err != nil {
			t.Fatal(err)
		}
		s.Version = version
		correction := Action{Type: "correct_model_compute", ModelID: a.ModelID, TensorShapes: a.TensorShapes, DeclaredTotalParams: a.DeclaredTotalParams, DeclaredActiveParams: a.DeclaredActiveParams, DeclaredFlopsPerToken: a.DeclaredFlopsPerToken}
		err := s.correctModelCompute(actors["governor"].id, correction, 2)
		if (err != nil) != (version == 11) {
			t.Fatalf("version=%d correction err=%v", version, err)
		}
		if version == 12 && s.Models[a.ModelID].VerifiedCompute.ActiveParams != 105542546176 {
			t.Fatal("correction did not select v12")
		}
	}
}
