package chain

import (
	"bytes"
	"crypto/sha256"
	core "ember/core-go"
	pb "ember/proto"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

type ImageRequest struct {
	Prompt         string `json:"prompt"`
	NegativePrompt string `json:"negative_prompt"`
	// Canonical tokenizer output is supplied by the payer, signed in the request.
	// Both executor and auditor must reproduce it from the pinned tokenizer.
	PromptTokens   []uint32 `json:"prompt_tokens"`
	NegativeTokens []uint32 `json:"negative_tokens"`
	Seed           uint64   `json:"seed"`
	Sampler        string   `json:"sampler"`
	Steps          uint32   `json:"steps"`
	Guidance       uint32   `json:"guidance"`
	Resolution     uint32   `json:"resolution"`
}
type ImageLeaf struct {
	Leaf       SpotLeaf `json:"leaf"`
	AnchorHash [32]byte `json:"anchor_hash"`
}
type ImageSubmission struct {
	ActualDtype      string      `json:"actual_dtype"` // signed before audit entropy; configuration admission
	Leaves           []ImageLeaf `json:"leaves"`
	ImageSHA256      string      `json:"image_sha256"` // transport file hash
	PixelSHA256      string      `json:"pixel_sha256"` // RGB uint8 row-major
	ImageBytes       uint64      `json:"image_bytes"`
	FinalLatentHash  string      `json:"final_latent_hash"`
	SpoolReceiptHash string      `json:"spool_receipt_hash"`
}
type ImageHold struct {
	MultiplierPPM     int64            `json:"multiplier_ppm"`
	Request           ImageRequest     `json:"request"`
	Submission        *ImageSubmission `json:"submission,omitempty"`
	Delivery          *SpotChallenge   `json:"delivery,omitempty"`
	PrecisionMismatch bool             `json:"precision_mismatch,omitempty"`
}

func ImageLeafHash(l ImageLeaf) [32]byte {
	h := SpotLeafHash(l.Leaf)
	b := append([]byte("EMBER-IMAGELEAF-V1\x00"), h[:]...)
	return sha256.Sum256(append(b, l.AnchorHash[:]...))
}
func ImagePromptAnchor(r ImageRequest) [32]byte {
	b := []byte("EMBER-IMAGEPROMPT-V1\x00")
	for _, ts := range [][]uint32{r.PromptTokens, r.NegativeTokens} {
		for _, t := range ts {
			b = binary.LittleEndian.AppendUint32(b, t)
		}
	}
	return sha256.Sum256(b)
}
func imageDigest(s string) bool {
	raw, e := hex.DecodeString(s)
	return e == nil && len(raw) == 32 && hex.EncodeToString(raw) == s && !bytes.Equal(raw, make([]byte, 32))
}
func imageProfileHash(p *core.ImageProfile) [32]byte {
	raw, _ := json.Marshal(p)
	return sha256.Sum256(raw)
}
func (s *State) validateImagePublication(a Action, m *pb.ModelManifest) (*core.ImageProfile, error) {
	if s.Version < 16 || a.Kind != "text-to-image" || a.ImageProfile == nil {
		return nil, errors.New("image publication requires v16")
	}
	p := a.ImageProfile
	hash := imageProfileHash(p)
	if !bytes.Equal(hash[:], m.GraphHash) {
		return nil, errors.New("image profile not bound to signed graph hash")
	}
	artifacts := map[string]*pb.Artifact{}
	for _, f := range m.Artifacts {
		artifacts[f.Name] = f
	}
	shapes := map[string][]core.TensorShape{}
	used := map[string]bool{}
	for _, c := range p.Components {
		for name, hash := range c.Files {
			f := artifacts[name]
			if f == nil || hex.EncodeToString(f.Sha256) != hash || used[name] {
				return nil, errors.New("component artifact hash mismatch")
			}
			used[name] = true
			if c.Role != "unet" && c.Role != "vae" && c.Role != "text_encoder" {
				continue
			}
			if !strings.HasSuffix(name, ".safetensors") {
				return nil, errors.New("image weights must use safetensors")
			}
			header, ok := a.ImageHeaders[name]
			if !ok {
				return nil, errors.New("missing safetensors shard header")
			}
			ts, e := core.ParseSafeTensorHeader(header)
			if e != nil {
				return nil, e
			}
			var size uint64
			for _, t := range ts {
				n, err := core.SafeTensorDtypeSize(t.Dtype)
				if err != nil {
					return nil, err
				}
				for _, d := range t.Shape {
					n *= d
				}
				if n > math.MaxUint64-size {
					return nil, errors.New("artifact size overflow")
				}
				size += n
			}
			if size > math.MaxUint64-uint64(len(header)) || size+uint64(len(header)) != uint64(f.Size) {
				return nil, errors.New("header/payload artifact size mismatch")
			}
			shapes[c.Role] = append(shapes[c.Role], ts...)
		}
	}
	for name := range artifacts {
		if strings.HasSuffix(name, ".safetensors") && !used[name] {
			return nil, errors.New("unassigned image weight shard")
		}
	}
	if err := core.ValidateImageProfile(p, shapes); err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(p)
	var copy core.ImageProfile
	_ = json.Unmarshal(raw, &copy)
	return &copy, nil
}
func validateImageRequest(p *core.ImageProfile, r *ImageRequest) error {
	if p == nil || r == nil || !utf8.ValidString(r.Prompt) || !utf8.ValidString(r.NegativePrompt) || len(r.Prompt) > 65536 || len(r.NegativePrompt) > 65536 || r.Steps == 0 || r.Steps > p.MaxSteps || uint64(r.Resolution) >= uint64(len(p.AllowedResolutions)) || r.Guidance < p.GuidanceRange[0] || r.Guidance > p.GuidanceRange[1] {
		return errors.New("invalid image request")
	}
	ok := false
	for _, v := range p.AllowedSamplers {
		ok = ok || v == r.Sampler
	}
	if !ok {
		return errors.New("sampler outside profile")
	}
	for _, ts := range [][]uint32{r.PromptTokens, r.NegativeTokens} {
		if len(ts) != 77 || ts[0] != 49406 {
			return errors.New("prompt must encode BOS/tokens/EOS/pad within 77")
		}
		eos := false
		for _, v := range ts[1:] {
			if v > 49407 || v == 49406 || (eos && v != 49407) {
				return errors.New("invalid prompt tokens")
			}
			eos = eos || v == 49407
		}
		if !eos {
			return errors.New("prompt too long")
		}
	}
	return nil
}
func (s *State) freezeImage(signer string, a Action, height int64) error {
	if s.Version < 16 || a.Actual != 0 || a.Payer != signer || a.Node == signer || a.HoldID == "" || s.Holds[a.HoldID] != nil || !s.ExecutionAllowed(a.Node, height) {
		return errors.New("invalid image freeze")
	}
	if r := s.Availability[a.Node]; r != nil && height < r.PauseUntil {
		return errors.New("image executor availability paused")
	}
	m := s.Models[a.ModelID]
	p := s.ActiveProfile(a.Node, height)
	if m.Kind != "text-to-image" || m.ImageProfile == nil || p == nil || p.ChainId != m.ID || p.ShardTotalLayers != 0 || p.ExecutionProfileVer != m.ImageProfile.ExecutionProfile || s.validatePublishedNode(p) != nil {
		return errors.New("image requires registered whole execution profile")
	}
	if err := validateImageRequest(m.ImageProfile, a.ImageRequest); err != nil {
		return err
	}
	actual, err := core.ImageActual(m.ImageProfile, a.ImageRequest.Resolution, a.ImageRequest.Steps, s.PriceForProfile(m.ID).MultiplierPPM)
	if err != nil {
		return err
	}
	if a.Amount != actual {
		return fmt.Errorf("image freeze must equal calculated Actual %d", actual)
	}
	node := s.Accounts[a.Node]
	if node == nil || node.Stake < s.Parameters.MinNodeStake || actual > node.Stake/s.newExposureDivisor(a.Node)-s.exposure(a.Node) {
		return errors.New("image stake exposure limit")
	}
	marked, err := s.debitAllInference(signer, actual)
	if err != nil {
		return err
	}
	r := *a.ImageRequest
	r.PromptTokens = append([]uint32(nil), r.PromptTokens...)
	r.NegativeTokens = append([]uint32(nil), r.NegativeTokens...)
	h := &Hold{ID: a.HoldID, Node: a.Node, Payer: signer, ModelID: m.ID, Amount: actual, Actual: actual, State: "frozen", PrivacyTier: "P0", Marked: marked, SubsidyDebit: s.takeInferenceDebit(), FreezeHeight: height, ExpireHeight: height + s.Parameters.HoldTimeout, ProducerProfile: append([]byte(nil), s.Nodes[a.Node].Profile...), Image: &ImageHold{Request: r, MultiplierPPM: s.PriceForProfile(m.ID).MultiplierPPM}}
	s.Holds[h.ID] = h
	s.freezeConsumerGray(h, height)
	return nil
}
func (s *State) submitImage(signer string, a Action, height int64) error {
	h := s.Holds[a.HoldID]
	if s.Version < 16 || h == nil || h.Image == nil || h.State != "frozen" || signer != h.Node || height >= h.ExpireHeight || a.Actual != 0 || a.ImageSubmission == nil {
		return errors.New("invalid image submission")
	}
	if r := s.Availability[h.Node]; r != nil && height < r.PauseUntil {
		return errors.New("image executor availability paused")
	}
	sub := a.ImageSubmission
	if sub.ActualDtype != "F32" && sub.ActualDtype != "F16" && sub.ActualDtype != "BF16" {
		return errors.New("actual execution dtype required")
	}
	r := h.Image.Request
	p := s.Models[h.ModelID].ImageProfile
	if len(sub.Leaves) != int(r.Steps)+1 || len(a.CommitmentRoot) != 32 || !imageDigest(sub.ImageSHA256) || !imageDigest(sub.PixelSHA256) || !imageDigest(sub.FinalLatentHash) || !imageDigest(sub.SpoolReceiptHash) || sub.ImageBytes == 0 {
		return errors.New("invalid image leaf count or delivery anchors")
	}
	resolution := p.AllowedResolutions[r.Resolution]
	shape := [4]uint64{1, 4, uint64(resolution.Height) / 8, uint64(resolution.Width) / 8}
	values, err := core.T2IInitLatent(r.Seed, shape)
	if err != nil {
		return err
	}
	raw, err := core.T2ISerializeLatent(values, shape)
	if err != nil {
		return err
	}
	init := sha256.Sum256(raw)
	format := SpotFormatHash("f32", shape[:])
	profile := sha256.Sum256([]byte(h.ModelID))
	requestRaw, err := hex.DecodeString(h.RequestID)
	if err != nil || len(requestRaw) != 32 {
		return errors.New("missing pinned request identity")
	}
	var request [32]byte
	copy(request[:], requestRaw)
	hashes := make([][32]byte, len(sub.Leaves))
	zero := [32]byte{}
	for i, l := range sub.Leaves {
		if l.Leaf.Step != uint64(i) || l.Leaf.FormatHash != format || l.Leaf.ProfileIDHash != profile || l.Leaf.RequestID != request {
			return errors.New("image leaf identity/format mismatch")
		}
		if i == 0 {
			if l.Leaf.InputHash != init || l.Leaf.OutputHash != init || l.AnchorHash != ImagePromptAnchor(r) {
				return errors.New("image init/prompt anchor mismatch")
			}
		} else {
			if l.Leaf.InputHash != sub.Leaves[i-1].Leaf.OutputHash {
				return errors.New("image latent discontinuity")
			}
			if i < len(sub.Leaves)-1 && l.AnchorHash != zero {
				return errors.New("unexpected image anchor")
			}
		}
		hashes[i] = ImageLeafHash(l)
	}
	last := sub.Leaves[len(sub.Leaves)-1]
	pixels, _ := hex.DecodeString(sub.PixelSHA256)
	var pixelHash [32]byte
	copy(pixelHash[:], pixels)
	final, _ := hex.DecodeString(sub.FinalLatentHash)
	if !bytes.Equal(last.Leaf.OutputHash[:], final) || last.AnchorHash != pixelHash {
		return errors.New("image final latent/pixel anchor mismatch")
	}
	root, _ := SpotMerkleRoot(hashes)
	if !bytes.Equal(root[:], a.CommitmentRoot) {
		return errors.New("image commitment root mismatch")
	}
	// The full receipt is optional until an absence accusation. A signed receipt
	// counterproof is checked by the same spool protocol as audit evidence.
	copyRaw, _ := json.Marshal(sub)
	var cp ImageSubmission
	_ = json.Unmarshal(copyRaw, &cp)
	h.Image.Submission = &cp
	h.Pipeline = &PipelineHold{SpotStepCount: uint64(r.Steps), CommitmentRoot: append([]byte(nil), a.CommitmentRoot...), Authorizations: []PipelineAuthorization{{NodeID: h.Node, Profile: h.ProducerProfile, Reserved: h.Actual}}, Credits: []core.StageShare{{NodeID: h.Node, Amount: h.Actual}}}
	h.State = "dispute"
	h.SubmitHeight = height
	h.SettleHeight = height + s.Parameters.DisputeBlocks
	h.ReceiptRef = hex.EncodeToString(root[:])
	h.AuditOutputTokens = r.Steps
	h.Image.Delivery = &SpotChallenge{ObjectHash: ImageDeliveryBinding(sub), RequestID: h.ID, Auditor: h.Payer, ChallengeHeight: h.FreezeHeight, Deadline: height + SPOT_OPEN_ROUND_BLOCKS, VerdictDeadline: height + SPOT_OPEN_ROUND_BLOCKS + SPOT_FIRST_VERDICT_BLOCKS}
	if a.Spool != nil {
		return s.imageSpoolAction(signer, Action{Type: "image_received", HoldID: h.ID, Spool: a.Spool}, height)
	}
	return nil
}

// ImageDeliveryBinding binds the generic spool evidence protocol to this exact
// delivery object, independently of any audit challenge or PNG encoder.
func ImageDeliveryBinding(sub *ImageSubmission) []byte {
	b := []byte("EMBER-IMAGE-DELIVERY-V1\x00" + sub.ImageSHA256 + sub.PixelSHA256 + sub.FinalLatentHash)
	b = binary.BigEndian.AppendUint64(b, sub.ImageBytes)
	h := sha256.Sum256(b)
	return h[:]
}
func SpoolReceiptHash(a SpoolAttestation) string {
	raw, _ := json.Marshal(a)
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}
func (s *State) imageSpoolAction(signer string, a Action, height int64) error {
	h := s.Holds[a.HoldID]
	if s.Version < 16 || h == nil || h.Image == nil || h.Image.Delivery == nil || h.State != "dispute" {
		return errors.New("invalid image delivery evidence")
	}
	c := h.Image.Delivery
	switch a.Type {
	case "image_received":
		if signer != h.Payer && signer != h.Node {
			return errors.New("invalid delivery claimant")
		}
		if c.Accusation != nil || c.Received {
			return errors.New("delivery already decided")
		}
		if err := s.verifySpotSpool(c, a.Spool, height); err != nil {
			return err
		}
		if !a.Spool.Received || SpoolReceiptHash(*a.Spool) != h.Image.Submission.SpoolReceiptHash {
			return errors.New("image receipt hash mismatch")
		}
		c.Received = true
		return nil
	case "image_unopened":
		if height >= h.SettleHeight-SPOT_FIRST_VERDICT_BLOCKS {
			return errors.New("delivery absence evidence window closed")
		}
		a.Type = "spot_unopened"
	case "image_counterproof":
		a.Type = "spot_counterproof"
		if a.Spool == nil || SpoolReceiptHash(*a.Spool) != h.Image.Submission.SpoolReceiptHash {
			return errors.New("image counterproof receipt hash mismatch")
		}
	default:
		return errors.New("invalid image evidence action")
	}
	return s.spoolEvidenceAction(h, c, signer, a, height)
}
func (s *State) advanceImageDelivery(h *Hold, height int64) error {
	c := h.Image.Delivery
	if h.State != "dispute" || c == nil || c.Received || c.EvidenceResolved {
		return nil
	}
	if c.Accusation != nil && height > c.AccusedAt+SPOT_FIRST_VERDICT_BLOCKS {
		c.EvidenceResolved = true
		s.availability(h.Node).Executor.UnopenedTotal++
		s.availabilityEvent(h.Node, true, height)
		h.AuditVerdict = "reject_undelivered"
		h.FateHeight = height
		return s.settleAt(h, "service_failure", height)
	}
	if height >= h.SettleHeight {
		h.AuditVerdict = "unconfirmed_delivery_timeout"
		h.FateHeight = height
		return s.settleAt(h, "service_failure", height)
	}
	return nil
}
func (s *State) imagePrecisionMismatch(h *Hold, v *pb.AuditVerdict) bool {
	if h.Image == nil || h.Image.Submission == nil || h.Image.Submission.ActualDtype == "F32" || v.Verdict != "reject" || len(v.PerStep) == 0 {
		return false
	}
	b := s.Models[h.ModelID].ImageProfile.RangeBand
	for _, p := range v.PerStep {
		if p.Outcome != "mismatch" || p.FirstDivergenceDistance < b.PrecisionMin || p.FirstDivergenceDistance > b.PrecisionMax {
			return false
		}
	}
	return true
}

// EffectiveKind keeps legacy persisted models byte-identical: missing kind is
// text and is never backfilled by loading, quoting or replaying old state.
func (m Model) EffectiveKind() string {
	if m.Kind == "" {
		return "text"
	}
	return m.Kind
}
func (s *State) ImageQuote(model string, r ImageRequest) (int64, error) {
	m := s.Models[model]
	if s.Version < 16 || m.EffectiveKind() != "text-to-image" {
		return 0, errors.New("not an active image profile")
	}
	if e := validateImageRequest(m.ImageProfile, &r); e != nil {
		return 0, e
	}
	return core.ImageActual(m.ImageProfile, r.Resolution, r.Steps, s.PriceForProfile(model).MultiplierPPM)
}
