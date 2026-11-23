package chain

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	core "ember/core-go"
	pb "ember/proto"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"google.golang.org/protobuf/proto"
	"testing"
)

func imageProfileFixture(t *testing.T) (*core.ImageProfile, map[string][]byte, map[string]int64) {
	t.Helper()
	p := &core.ImageProfile{ExecutionProfile: "diffusers-sd1.5-fp32-v2", RNGSpecID: "philox-gauss-v1", MaxSteps: 100, GuidanceRange: [2]uint32{1000, 30000}, AllowedResolutions: []core.ImageResolution{{Width: 512, Height: 512}}, AllowedSamplers: []string{"ddim", "euler"}, RangeBand: core.ImageStepBand{Metric: "relative-l2-v1", Base: 3e-6, StepCoef: 1e-6, HardMultiplier: 100, PrecisionMin: .005, PrecisionMax: .06, CalibrationHash: fmt.Sprintf("%064d", 1)}, ArchManifest: map[string][]core.ImageLayer{}}
	p.LatentSpec.Channels = 4
	p.LatentSpec.VAEScaleFactor = 8
	p.LatentSpec.Distribution = "standard-normal"
	shapes := map[string][]core.TensorShape{"unet": {{Name: "down_blocks.0.resnets.0.conv1.weight", Shape: []uint64{320, 320, 3, 3}, Dtype: "F32"}}, "text_encoder": {{Name: "emb.weight", Shape: []uint64{64, 64}, Dtype: "F32"}}, "vae": {{Name: "decoder.conv_out.weight", Shape: []uint64{3, 128, 3, 3}, Dtype: "F32"}}}
	headers := map[string][]byte{}
	sizes := map[string]int64{}
	for _, role := range []string{"unet", "text_encoder", "vae"} {
		ts := shapes[role]
		var n uint64 = 1
		for _, d := range ts[0].Shape {
			n *= d
		}
		raw, _ := json.Marshal(map[string]any{ts[0].Name: map[string]any{"dtype": "F32", "shape": ts[0].Shape, "data_offsets": []uint64{0, n * 4}}})
		header := append(binary.LittleEndian.AppendUint64(nil, uint64(len(raw))), raw...)
		name := role + "/model.safetensors"
		headers[name] = header
		sizes[name] = int64(len(header)) + int64(n*4)
		h := sha256.Sum256([]byte(name))
		p.Components = append(p.Components, core.ImageComponent{Role: role, Files: map[string]string{name: hex.EncodeToString(h[:])}, Params: n, Dtype: "F32"})
		p.ArchManifest[role], _ = core.ReconstructImageLayers(role, ts)
	}
	for _, role := range []string{"tokenizer", "scheduler_cfg", "arch_manifest"} {
		name := role + ".json"
		h := sha256.Sum256([]byte("artifact"))
		p.Components = append(p.Components, core.ImageComponent{Role: role, Files: map[string]string{name: hex.EncodeToString(h[:])}})
		sizes[name] = 8
	}
	p.PriceTable = []core.ImagePrice{{Step: 2 * 2 * 320 * 320 * 3 * 3 * 64 * 64, Text: 2 * 4096 * 77 * 2, VAE: 2 * 3 * 128 * 3 * 3 * 512 * 512}}
	if e := core.ValidateImageProfile(p, shapes); e != nil {
		t.Fatal(e)
	}
	return p, headers, sizes
}
func imagePublication(t *testing.T, who actor) Action {
	a := publicationAction(t, who, "image-synthetic")
	p, headers, sizes := imageProfileFixture(t)
	a.Kind = "text-to-image"
	a.ImageProfile = p
	a.ImageHeaders = headers
	m := &pb.ModelManifest{}
	proto.Unmarshal(a.Manifest, m)
	m.Artifacts = nil
	hash := imageProfileHash(p)
	m.GraphHash = hash[:]
	h := sha256.Sum256([]byte("artifact"))
	m.Artifacts = append(m.Artifacts, &pb.Artifact{Name: "config.json", Sha256: h[:], Size: 8, Source: "git:config.json"})
	for _, c := range p.Components {
		for name, hash := range c.Files {
			raw, _ := hex.DecodeString(hash)
			m.Artifacts = append(m.Artifacts, &pb.Artifact{Name: name, Sha256: raw, Size: sizes[name], Source: "git:" + name})
		}
	}
	g := &pb.ChainGenesis{}
	proto.Unmarshal(a.Genesis, g)
	g.ExecutionProfileVer = p.ExecutionProfile
	core.Sign(g, who.key)
	a.Genesis, _ = proto.Marshal(g)
	return resignPublication(t, who, a, m)
}
func TestImagePublicationV16(t *testing.T) {
	for _, tc := range []string{"good", "buffer", "buffer-short", "plus5", "minus5", "channel", "params", "scale", "header", "v15", "execution-v1"} {
		t.Run(tc, func(t *testing.T) {
			s, a := testState(t)
			s.Version = 16
			action := imagePublication(t, a["payer"])
			switch tc {
			case "execution-v1":
				action.ImageProfile.ExecutionProfile = "diffusers-sd1.5-fp32-v1"
			case "buffer", "buffer-short":
				name := "text_encoder/model.safetensors"
				body := []byte(`{"text_model.embeddings.position_ids":{"dtype":"I64","shape":[1,77],"data_offsets":[0,616]},"emb.weight":{"dtype":"F32","shape":[64,64],"data_offsets":[616,17000]}}`)
				action.ImageHeaders[name] = append(binary.LittleEndian.AppendUint64(nil, uint64(len(body))), body...)
				manifest := &pb.ModelManifest{}
				proto.Unmarshal(action.Manifest, manifest)
				for _, artifact := range manifest.Artifacts {
					if artifact.Name == name {
						artifact.Size = int64(len(action.ImageHeaders[name])) + 17000
						if tc == "buffer-short" {
							artifact.Size -= 77 * 6
						}
					}
				}
				action.Manifest, _ = proto.Marshal(manifest)
			case "plus5":
				action.ImageProfile.PriceTable[0].Step = action.ImageProfile.PriceTable[0].Step * 105 / 100
			case "minus5":
				action.ImageProfile.PriceTable[0].Step = action.ImageProfile.PriceTable[0].Step * 95 / 100
			case "channel":
				action.ImageProfile.ArchManifest["unet"][0].Shape[0]++
			case "params":
				action.ImageProfile.Components[0].Params++
			case "scale":
				action.ImageProfile.ArchManifest["unet"][0].Scale *= 2
			case "header":
				action.ImageHeaders["unet/model.safetensors"] = []byte("bad")
			case "v15":
				s.Version = 15
			}
			// Re-sign the altered declaration so rejection exercises derivation, not signatures.
			m := &pb.ModelManifest{}
			proto.Unmarshal(action.Manifest, m)
			h := imageProfileHash(action.ImageProfile)
			m.GraphHash = h[:]
			action = resignPublication(t, a["payer"], action, m)
			if tc != "good" && tc != "buffer" {
				rejected(t, s, a["payer"], action, 1)
				return
			}
			apply(t, s, a["payer"], 1, action, 1)
			model := s.Models[action.ModelID]
			if model.ImageProfile.PriceTable[0] != action.ImageProfile.PriceTable[0] || model.Publication.Bond != 100 {
				t.Fatal("publication not stored")
			}
			if _, e := s.Clone(); e != nil {
				t.Fatal(e)
			}
		})
	}
}
func imageRequestFixture() ImageRequest {
	ts := make([]uint32, 77)
	for i := range ts {
		ts[i] = 49407
	}
	ts[0] = 49406
	return ImageRequest{Prompt: "", PromptTokens: ts, NegativeTokens: append([]uint32(nil), ts...), Seed: 42, Sampler: "ddim", Steps: 20, Guidance: 7500}
}
func imageFrozenFixture(t *testing.T) (*State, map[string]actor, *Hold) {
	s, a := testState(t)
	s.Version = 16
	s.Parameters.DisputeBlocks = 5000
	s.Parameters.HoldTimeout = 7000
	s.Parameters.RegistrationTTL = 10000
	action := imagePublication(t, a["payer"])
	apply(t, s, a["payer"], 1, action, 1)
	root, _ := hex.DecodeString(s.Models[action.ModelID].Publication.ManifestRoot)
	for _, name := range []string{"node", "auditor", "auditor2"} {
		who := a[name]
		s.Accounts[who.id].Stake = 10000
		p := &pb.ResourceProfile{NodeId: who.id, ChainId: action.ModelID, ManifestRoot: root, ExecutionProfileVer: action.ImageProfile.ExecutionProfile, ActualDevice: name, EngineCapabilities: &pb.EngineCapabilities{EngineName: "diffusers-t2i", Roles: []string{"model", "auditor"}}}
		core.Sign(p, who.key)
		raw, _ := proto.Marshal(p)
		s.Nodes[who.id] = &NodeRegistration{NodeID: who.id, Profile: raw, ExpiresHeight: 10000}
	}
	r := imageRequestFixture()
	price, e := core.ImageActual(action.ImageProfile, 0, r.Steps, 1000000)
	if e != nil {
		t.Fatal(e)
	}
	apply(t, s, a["payer"], 2, Action{Type: "image_request", HoldID: "image-1", Payer: a["payer"].id, Node: a["node"].id, ModelID: action.ModelID, Amount: price, ImageRequest: &r}, 2)
	return s, a, s.Holds["image-1"]
}
func imageSubmissionFixture(t *testing.T, h *Hold) (*ImageSubmission, []byte) {
	r := h.Image.Request
	shape := [4]uint64{1, 4, 64, 64}
	v, _ := core.T2IInitLatent(r.Seed, shape)
	raw, _ := core.T2ISerializeLatent(v, shape)
	init := sha256.Sum256(raw)
	profile := sha256.Sum256([]byte(h.ModelID))
	idRaw, _ := hex.DecodeString(h.RequestID)
	var id [32]byte
	copy(id[:], idRaw)
	sub := &ImageSubmission{ActualDtype: "F32", ImageSHA256: fmt.Sprintf("%064d", 1), PixelSHA256: fmt.Sprintf("%064d", 2), ImageBytes: 100, SpoolReceiptHash: fmt.Sprintf("%064d", 3)}
	prev := init
	for i := uint32(0); i <= r.Steps; i++ {
		out := sha256.Sum256([]byte(fmt.Sprint(i)))
		if i == 0 {
			out = init
		}
		l := ImageLeaf{Leaf: SpotLeaf{Step: uint64(i), InputHash: prev, OutputHash: out, FormatHash: SpotFormatHash("f32", shape[:]), ProfileIDHash: profile, RequestID: id}}
		if i == 0 {
			l.AnchorHash = ImagePromptAnchor(r)
		}
		if i == r.Steps {
			pixel, _ := hex.DecodeString(sub.PixelSHA256)
			copy(l.AnchorHash[:], pixel)
			sub.FinalLatentHash = hex.EncodeToString(out[:])
		}
		sub.Leaves = append(sub.Leaves, l)
		prev = out
	}
	return sub, imageRoot(sub)
}
func imageRoot(sub *ImageSubmission) []byte {
	hs := make([][32]byte, len(sub.Leaves))
	for i, l := range sub.Leaves {
		hs[i] = ImageLeafHash(l)
	}
	root, _ := SpotMerkleRoot(hs)
	return root[:]
}
func TestImageFreezeAndSubmission(t *testing.T) {
	for _, tc := range []string{"good", "leaves", "init", "prompt", "final", "pixels", "actual", "text_submit", "root", "format"} {
		t.Run(tc, func(t *testing.T) {
			s, a, h := imageFrozenFixture(t)
			if h.Actual != h.Amount || s.exposure(h.Node) != h.Actual {
				t.Fatal("freeze Actual/exposure")
			}
			sub, root := imageSubmissionFixture(t, h)
			switch tc {
			case "leaves":
				sub.Leaves = sub.Leaves[:len(sub.Leaves)-1]
			case "init":
				sub.Leaves[0].Leaf.InputHash[0] ^= 1
			case "prompt":
				sub.Leaves[0].AnchorHash[0] ^= 1
			case "final":
				sub.FinalLatentHash = fmt.Sprintf("%064d", 4)
			case "pixels":
				sub.PixelSHA256 = fmt.Sprintf("%064d", 4)
			case "root":
				root[0] ^= 1
			case "format":
				sub.Leaves[1].Leaf.FormatHash[0] ^= 1
			}
			act := Action{Type: "submit_image", HoldID: h.ID, ImageSubmission: sub, CommitmentRoot: root}
			if tc == "actual" {
				act.Actual = h.Actual
			}
			if tc == "text_submit" {
				act.Type = "submit"
				act.Actual = h.Actual
				act.Receipt = signedReceipt(t, h.ID, a["node"])
			}
			if tc != "good" {
				rejected(t, s, a["node"], act, 3)
				return
			}
			apply(t, s, a["node"], 1, act, 3)
			h = s.Holds[h.ID]
			if h.Actual != h.Amount || s.exposure(h.Node) != h.Actual || h.Image.Submission.FinalLatentHash != sub.FinalLatentHash {
				t.Fatal("submit changed fee or exposure")
			}
		})
	}
}
func TestImagePromptBoundaryAndVersionReplay(t *testing.T) {
	s, a, h := imageFrozenFixture(t)
	r := imageRequestFixture()
	r.PromptTokens = append(r.PromptTokens, 49407)
	act := Action{Type: "image_request", HoldID: "long", Payer: h.Payer, Node: h.Node, ModelID: h.ModelID, Amount: h.Actual, ImageRequest: &r}
	rejected(t, s, a["payer"], act, 3)
	for _, v := range []uint64{15, 16} {
		s, a := testState(t)
		s.Version = v
		before, _ := s.Bytes()
		if bytes.Contains(before, []byte(`"image"`)) || bytes.Contains(before, []byte(`"kind"`)) {
			t.Fatal("legacy bytes changed")
		}
		if v == 15 {
			act.ImageRequest = &r
			rejected(t, s, a["payer"], act, 1)
		}
	}
}
func imageSpool(t *testing.T, h *Hold, g actor, received bool, height int64) *SpoolAttestation {
	c := h.Image.Delivery
	a := &SpoolAttestation{RequestID: h.ID, ChallengeHeight: c.ChallengeHeight, StepSetHash: spotObjectHash(c), Gateway: g.id, Received: received, Height: height}
	a.Signature = ed25519.Sign(g.key, SpoolSigningBytes(*a))
	return a
}
func imageSubmittedFixture(t *testing.T) (*State, map[string]actor, *Hold, actor) {
	s, a, h := imageFrozenFixture(t)
	g := gatewayFixture(t, s)
	sub, root := imageSubmissionFixture(t, h)
	temp := *h
	image := *h.Image
	temp.Image = &image
	image.Delivery = &SpotChallenge{RequestID: h.ID, ChallengeHeight: h.FreezeHeight, ObjectHash: ImageDeliveryBinding(sub)}
	receipt := imageSpool(t, &temp, g, true, 3)
	sub.SpoolReceiptHash = SpoolReceiptHash(*receipt)
	apply(t, s, a["node"], 1, Action{Type: "submit_image", HoldID: h.ID, ImageSubmission: sub, CommitmentRoot: root}, 3)
	return s, a, s.Holds[h.ID], g
}
func imageConfirmDelivery(t *testing.T, s *State, a map[string]actor, h *Hold, g actor) {
	t.Helper()
	apply(t, s, a["node"], s.Accounts[h.Node].Nonce+1, Action{Type: "image_received", HoldID: h.ID, Spool: imageSpool(t, h, g, true, 3)}, 3)
}
func imageSignedAudit(t *testing.T, s *State, a map[string]actor, h *Hold, bad, precision bool, height int64) {
	t.Helper()
	aud := a["auditor"]
	if aud.id != h.Auditor {
		aud = a["auditor2"]
	}
	if aud.id != h.Auditor {
		t.Fatal("missing audit signer")
	}
	v := &pb.AuditVerdict{Verdict: "accept", VerdictKind: "accept"}
	if h.AuditRound == 2 {
		v.VerdictKind = "escalation_bounded_accept"
	}
	if bad {
		v.Verdict = "reject"
		v.VerdictKind = "reject_step"
		if h.AuditRound == 2 {
			v.VerdictKind = "escalation_bounded_reject"
		}
	}
	if precision {
		v.VerdictKind = "precision_mismatch"
	}
	for _, step := range h.Pipeline.Spot.StepSet {
		result := &pb.SpotStepResult{Step: step, Outcome: "match"}
		if bad {
			result.Outcome = "mismatch"
			result.FirstDivergenceDistance = 1
			if precision {
				result.FirstDivergenceDistance = .01
			}
			v.VerdictDistance = result.FirstDivergenceDistance
		}
		v.PerStep = append(v.PerStep, result)
	}
	ref, _ := hex.DecodeString(h.ReceiptRef)
	proof, _ := hex.DecodeString(h.AuditProof)
	c := &pb.AuditChallenge{ReceiptRef: ref, VrfProof: proof}
	core.Sign(c, aud.key)
	v.ChallengeRef, _ = core.Digest(c)
	v.AuditorId = aud.id
	v.EvidenceRef = bytes.Repeat([]byte{7}, 32)
	core.Sign(v, aud.key)
	cb, _ := proto.Marshal(c)
	vb, _ := proto.Marshal(v)
	apply(t, s, aud, s.Accounts[aud.id].Nonce+1, Action{Type: "audit", HoldID: h.ID, Challenge: cb, Verdict: vb}, height)
}
func TestImageFatesSettlementAndSlash(t *testing.T) {
	for _, mode := range []string{"unaudited", "accept", "reject", "noauditor", "good_faith"} {
		t.Run(mode, func(t *testing.T) {
			s, a, h, g := imageSubmittedFixture(t)
			imageConfirmDelivery(t, s, a, h, g)
			h = s.Holds[h.ID]
			actual := h.Actual
			stake := s.Accounts[h.Node].Stake
			executorBalance := s.Accounts[h.Node].Balance
			payerBalance := s.Accounts[h.Payer].Balance
			if mode == "noauditor" {
				delete(s.Nodes, a["auditor"].id)
				delete(s.Nodes, a["auditor2"].id)
			}
			if mode == "unaudited" {
				s.Parameters.AuditSampling.Enabled = true
				s.freezeSampling(h)
				for i := 0; ; i++ {
					hash := sha256.Sum256([]byte(fmt.Sprint("image-unsampled-", i)))
					s.BlockHashes[4] = hash[:]
					h.Sampling = nil
					sampled, e := s.resolveSampling(h)
					if e != nil {
						t.Fatal(e)
					}
					if !sampled {
						break
					}
					if i > 1000 {
						t.Fatal("fixture seed")
					}
				}
			}
			rejected(t, s, a["coordinator"], Action{Type: "settle", HoldID: h.ID, Outcome: "accept"}, 3)
			apply(t, s, a["coordinator"], s.Accounts[a["coordinator"].id].Nonce+1, Action{Type: "assign_audit", HoldID: h.ID}, 4)
			h = s.Holds[h.ID]
			settleHeight := int64(4)
			outcome := "accept"
			if mode == "accept" || mode == "reject" || mode == "good_faith" {
				if len(h.Pipeline.Spot.StepSet) != 1 {
					t.Fatal("N=20,q=.3 k")
				}
				imageSignedAudit(t, s, a, h, mode != "accept", false, 5)
				h = s.Holds[h.ID]
				settleHeight = 5
				if mode != "accept" {
					if len(h.Slashes) != 0 || h.AuditVerdict != "" || len(h.Pipeline.Spot.StepSet) > 16 || h.Pipeline.Spot.Deadline != 605 {
						t.Fatal("escalation bounds")
					}
					imageSignedAudit(t, s, a, h, mode == "reject", false, 6)
					h = s.Holds[h.ID]
					settleHeight = 6
				}
				outcome = h.AuditVerdict
			}
			if mode == "noauditor" && (!h.NoAuditor || h.FateHeight != 4) {
				t.Fatal("NoAuditor fate")
			}
			if mode == "unaudited" && (!h.Unaudited || h.FateHeight != 4) {
				t.Fatal("Unaudited fate")
			}
			apply(t, s, a["coordinator"], s.Accounts[a["coordinator"].id].Nonce+1, Action{Type: "settle", HoldID: h.ID, Outcome: outcome}, settleHeight)
			h = s.Holds[h.ID]
			if mode == "reject" {
				if h.State != "slashed" || s.Accounts[h.Node].Stake != stake-20*actual || s.Accounts[h.Payer].Balance != payerBalance+actual+20*actual || s.Accounts[h.Node].Balance != executorBalance {
					t.Fatal("reject charge/slash", h.Slashes, h.State)
				}
			} else {
				parts := allocateFee(actual, [3]int64{80, 15, 5})
				if h.State != "settled" || s.Accounts[h.Node].Balance != executorBalance+parts[0] || s.Accounts[h.Payer].Balance != payerBalance+parts[2] || h.Shares.Executor+h.Shares.Auditor+h.Shares.Consumer != actual || s.Accounts[h.Node].Stake != stake {
					t.Fatal("80/15/5", h.Shares, h.State)
				}
				if mode == "unaudited" || mode == "noauditor" {
					if s.AuditPool[h.ModelID] == nil {
						t.Fatal("audit pool unfunded")
					}
				} else {
					if s.Accounts[h.Auditor].Balance != 1000+parts[1] {
						t.Fatal("audit payment")
					}
				}
			}
			if s.exposure(h.Node) != 0 {
				t.Fatal("exposure not released")
			}
		})
	}
}
func TestImageDeliveryAbsenceAndCounterproof(t *testing.T) {
	for _, counter := range []bool{false, true} {
		t.Run(fmt.Sprint(counter), func(t *testing.T) {
			s, a, h, g := imageSubmittedFixture(t)
			deadline := h.Image.Delivery.Deadline
			stake := s.Accounts[h.Node].Stake
			balance := s.Accounts[h.Payer].Balance
			actual := h.Actual
			absent := imageSpool(t, h, g, false, deadline)
			act := Action{Type: "image_unopened", HoldID: h.ID, Spool: absent}
			rejected(t, s, a["node"], act, deadline+1)
			apply(t, s, a["payer"], s.Accounts[h.Payer].Nonce+1, act, deadline+1)
			h = s.Holds[h.ID]
			if counter {
				apply(t, s, a["node"], s.Accounts[h.Node].Nonce+1, Action{Type: "image_counterproof", HoldID: h.ID, Spool: imageSpool(t, h, g, true, 3)}, deadline+2)
				h = s.Holds[h.ID]
				if !h.Image.Delivery.Received || s.Availability[h.Payer].Major != 1 {
					t.Fatal("counterproof attribution")
				}
			}
			if e := s.advanceSpot(deadline + SPOT_FIRST_VERDICT_BLOCKS + 2); e != nil {
				t.Fatal(e)
			}
			if !counter {
				if h.State != "cancelled" || h.AuditVerdict != "reject_undelivered" || s.Accounts[h.Payer].Balance != balance+actual || s.Availability[h.Node].Major != 1 {
					t.Fatal("missing delivery refund")
				}
			}
			if s.Accounts[h.Node].Stake != stake || len(h.Slashes) > 0 {
				t.Fatal("availability caused fraud slash")
			}
		})
	}
}
func TestImagePrecisionConfigurationAndEligibility(t *testing.T) {
	s, a, h := imageFrozenFixture(t)
	g := gatewayFixture(t, s)
	sub, root := imageSubmissionFixture(t, h)
	sub.ActualDtype = "F16"
	apply(t, s, a["node"], 1, Action{Type: "submit_image", HoldID: h.ID, ImageSubmission: sub, CommitmentRoot: root}, 3)
	h = s.Holds[h.ID]
	_ = g
	// Cross-hardware same execution dtype qualifies; alternate dtype profile and
	// shard-only profiles are excluded independently of engine library versions.
	producer := s.ActiveProfile(h.Node, 3)
	aud := s.ActiveProfile(a["auditor"].id, 3)
	if !s.EligibleAuditorProfile(a["auditor"].id, h.Node, producer, 3) {
		t.Fatal("cross hardware auditor excluded")
	}
	aud.ExecutionProfileVer = "diffusers-sd1.5-fp16-v1"
	core.Sign(aud, a["auditor"].key)
	s.Nodes[a["auditor"].id].Profile, _ = proto.Marshal(aud)
	if s.EligibleAuditorProfile(a["auditor"].id, h.Node, producer, 3) {
		t.Fatal("cross precision auditor admitted")
	}
	apply(t, s, a["coordinator"], 1, Action{Type: "assign_audit", HoldID: h.ID}, 4)
	h = s.Holds[h.ID]
	balance := s.Accounts[h.Payer].Balance
	stake := s.Accounts[h.Node].Stake
	imageSignedAudit(t, s, a, h, true, true, 5)
	h = s.Holds[h.ID]
	if !h.Image.PrecisionMismatch || h.State != "cancelled" || h.AuditVerdict != "reject_configuration" || len(h.Slashes) != 0 || s.Accounts[h.Node].Stake != stake || s.Accounts[h.Payer].Balance != balance+h.Actual || s.Availability[h.Node].Major != 1 {
		t.Fatal("configuration outcome", h.State, h.AuditVerdict)
	}
}
func TestImageTimeoutAndFallback(t *testing.T) {
	s, a, h := imageFrozenFixture(t)
	balance := s.Accounts[h.Payer].Balance
	apply(t, s, a["payer"], s.Accounts[h.Payer].Nonce+1, Action{Type: "expire_hold", HoldID: h.ID}, h.ExpireHeight)
	if s.Accounts[h.Payer].Balance != balance+h.Actual {
		t.Fatal("hold timeout")
	}
	s, _, h, _ = imageSubmittedFixture(t)
	balance = s.Accounts[h.Payer].Balance
	if e := s.advanceSpot(h.SettleHeight); e != nil {
		t.Fatal(e)
	}
	if h.State != "cancelled" || s.Accounts[h.Payer].Balance != balance+h.Actual {
		t.Fatal("SettleHeight fallback")
	}
}

func TestImageV16ActivationAndRestart(t *testing.T) {
	s, _ := testState(t)
	s.Version = 15
	app := &Application{committed: s}
	u := MembershipUpgrade{ChainID: s.Parameters.ChainID, ActivationHeight: 2, T2IV16: true}
	if e := app.ConfigureMembershipUpgrade(u); e != nil {
		t.Fatal(e)
	}
	copy, _ := s.Clone()
	if e := app.activateMembershipTrial(copy, 1); e != nil || copy.Version != 15 {
		t.Fatal("premature", e)
	}
	if e := app.activateMembershipTrial(copy, 3); e == nil {
		t.Fatal("missed activation accepted")
	}
	if e := app.activateMembershipTrial(copy, 2); e != nil || copy.Version != 16 {
		t.Fatal("activation", e)
	}
	s, _, h := imageFrozenFixture(t)
	path := t.TempDir() + "/state.json"
	before, _ := s.Bytes()
	if e := SaveState(path, s); e != nil {
		t.Fatal(e)
	}
	loaded, e := LoadState(path)
	if e != nil {
		t.Fatal(e)
	}
	after, _ := loaded.Bytes()
	if !bytes.Equal(before, after) || loaded.Holds[h.ID].Actual != h.Actual {
		t.Fatal("restart state differs")
	}
	if (Model{}).EffectiveKind() != "text" {
		t.Fatal("legacy kind")
	}
}
func TestImageFrozenMultiplierAndPrecisionProof(t *testing.T) {
	s, a, h, g := imageSubmittedFixture(t)
	actual := h.Actual
	s.priceRecord(h.ModelID).MultiplierPPM = 2000000
	if quote, e := s.ImageQuote(h.ModelID, h.Image.Request); e != nil || quote < actual*2-1 {
		t.Fatal(quote, e)
	}
	if h.Image.MultiplierPPM != 1000000 || h.Actual != actual {
		t.Fatal("frozen fee resized")
	}
	imageConfirmDelivery(t, s, a, h, g)
	apply(t, s, a["coordinator"], 1, Action{Type: "assign_audit", HoldID: h.ID}, 4)
	h = s.Holds[h.ID]
	v := &pb.AuditVerdict{Verdict: "reject", VerdictKind: "precision_mismatch", VerdictDistance: .01}
	for _, step := range h.Pipeline.Spot.StepSet {
		v.PerStep = append(v.PerStep, &pb.SpotStepResult{Step: step, Outcome: "mismatch", FirstDivergenceDistance: .01})
	}
	raw, _ := proto.Marshal(v)
	if s.validateSpotVerdict(h, raw, 5) == nil {
		t.Fatal("distance alone excused fraud")
	}
	imageSignedAudit(t, s, a, h, false, false, 5)
	h = s.Holds[h.ID]
	apply(t, s, a["coordinator"], 2, Action{Type: "settle", HoldID: h.ID, Outcome: "accept"}, 5)
	if s.Holds[h.ID].Actual != actual {
		t.Fatal("settlement changed frozen Actual")
	}
}
func TestImageForkPreservesTextReplay(t *testing.T) {
	var previous []byte
	for _, version := range []uint64{15, 16} {
		s, a, _, h := spotFixture(t)
		s.Version = version
		imageSignedAudit(t, s, a, h, false, false, 5)
		h = s.Holds[h.ID]
		apply(t, s, a["coordinator"], s.Accounts[a["coordinator"].id].Nonce+1, Action{Type: "settle", HoldID: h.ID, Outcome: "accept"}, 5)
		s.Version = 15
		raw, e := s.Bytes()
		if e != nil {
			t.Fatal(e)
		}
		if previous != nil && !bytes.Equal(previous, raw) {
			t.Fatal("v16 changed historical text state bytes")
		}
		previous = raw
	}
}
