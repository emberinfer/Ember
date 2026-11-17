package chain

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"

	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

// MoE expert-sharded execution: backbone attention stages tile the layer
// axis per frame; expert branches are bound to the signed routing records.
// Settlement splits by derived FLOPs (scarcity-weighted for experts). A
// rejected request settles granularly (Plan B): completion first, honest
// segments paid from confiscated fault dues, the unissued consumer rebate
// and slash proceeds, in that order; the consumer is made whole from the
// freeze plus any leftover slash fund.

type MoeUnitRef struct {
	Layer  uint32 `json:"layer"`
	Expert uint32 `json:"expert"`
}

type MoeAuthorization struct {
	NodeID     string       `json:"node_id"`
	Kind       string       `json:"kind"` // "attention" | "expert"
	StartLayer uint32       `json:"start_layer,omitempty"`
	EndLayer   uint32       `json:"end_layer,omitempty"`
	Units      []MoeUnitRef `json:"units,omitempty"`
	Profile    []byte       `json:"profile,omitempty"`
	Reserved   int64        `json:"reserved,omitempty"`
}

type MoeHolderSnapshot struct {
	Layer   uint32 `json:"layer"`
	Expert  uint32 `json:"expert"`
	Holders uint32 `json:"holders"`
}

type MoeSegmentDue struct {
	Key    string `json:"key"`
	NodeID string `json:"node_id"`
	Kind   string `json:"kind"`
	Layer  uint32 `json:"layer"`
	Expert uint32 `json:"expert,omitempty"`
	Frame  uint32 `json:"frame"`
	End    uint32 `json:"end,omitempty"`
	Amount int64  `json:"amount"` // share of Actual, scarcity-weighted
}

type MoeCandidate struct {
	Kind   string `json:"kind"`
	Layer  uint32 `json:"layer"`
	Expert uint32 `json:"expert,omitempty"`
	Frame  uint32 `json:"frame"`
	End    uint32 `json:"end,omitempty"`
	Work   uint64 `json:"work"`
	Rows   uint32 `json:"rows"`
}

type MoePlanBRecord struct {
	FaultSegments   []string `json:"fault_segments"`
	HonestDue       int64    `json:"honest_due"`
	HonestPaid      int64    `json:"honest_paid"`
	Confiscated     int64    `json:"confiscated"`
	RebateUsed      int64    `json:"rebate_used"`
	SlashFund       int64    `json:"slash_fund"`
	LeftoverToPayer int64    `json:"leftover_to_payer"`
	Shortfall       int64    `json:"shortfall"`
}

type MoeHold struct {
	ChunkRoot          []byte              `json:"chunk_root,omitempty"`
	ChunkListHash      []byte              `json:"chunk_list_hash,omitempty"`
	ChunkTotal         int                 `json:"chunk_total,omitempty"`
	ChunkBytes         int                 `json:"chunk_bytes,omitempty"`
	ChunkActual        int64               `json:"chunk_actual,omitempty"`
	ChunkBitmap        []byte              `json:"chunk_bitmap,omitempty"`
	SchemeHash         []byte              `json:"scheme_hash,omitempty"`
	Authorizations     []MoeAuthorization  `json:"authorizations"`
	InputTokens        uint32              `json:"input_tokens"`
	MaxOutputTokens    uint32              `json:"max_output_tokens"`
	Holders            []MoeHolderSnapshot `json:"holders,omitempty"`
	WorstSegRows       uint32              `json:"worst_seg_rows"`
	Receipt            []byte              `json:"receipt,omitempty"`
	Credits            []core.StageShare   `json:"credits,omitempty"`
	SegmentDues        []MoeSegmentDue     `json:"segment_dues,omitempty"`
	Candidates         []MoeCandidate      `json:"candidates,omitempty"`
	ChallengeCount     int                 `json:"challenge_count"`
	FirstFaultSegments []string            `json:"first_fault_segments,omitempty"`
	PlanB              *MoePlanBRecord     `json:"plan_b,omitempty"`
}

func moeSegmentKey(kind string, frame, layer, expert, end uint32) string {
	if kind == "expert" {
		return fmt.Sprintf("e:%d:%d:%d", frame, layer, expert)
	}
	return fmt.Sprintf("a:%d:%d:%d", frame, layer, end)
}

func moeParseSegmentKey(key string) (*pb.SegmentRef, error) {
	parts := strings.Split(key, ":")
	if len(parts) != 4 {
		return nil, errors.New("invalid segment key")
	}
	var a, b, c uint32
	if _, err := fmt.Sscanf(parts[1]+":"+parts[2]+":"+parts[3], "%d:%d:%d", &a, &b, &c); err != nil {
		return nil, err
	}
	switch parts[0] {
	case "e":
		return &pb.SegmentRef{Kind: "expert", Frame: a, Layer: b, Expert: c, EndLayer: b + 1}, nil
	case "a":
		return &pb.SegmentRef{Kind: "attention", Frame: a, Layer: b, EndLayer: c}, nil
	}
	return nil, errors.New("invalid segment key kind")
}

func (s *State) moeModel(modelID string, schemes ...[]byte) (*pb.ModelManifest, *pb.ChainGenesis, []byte, error) {
	m := s.Models[modelID]
	graw, mraw := modelArtifacts(m)
	if len(graw) == 0 || len(mraw) == 0 {
		return nil, nil, nil, errors.New("MoE requires anchored signed model artifacts")
	}
	manifest := &pb.ModelManifest{}
	genesis := &pb.ChainGenesis{}
	if proto.Unmarshal(mraw, manifest) != nil || proto.Unmarshal(graw, genesis) != nil {
		return nil, nil, nil, errors.New("invalid signed model artifacts")
	}
	root, err := core.Digest(manifest)
	if err != nil {
		return nil, nil, nil, err
	}
	var selected []byte
	if len(schemes) > 0 {
		selected = schemes[0]
	}
	manifest, err = selectScheme(manifest, selected)
	if err != nil {
		return nil, nil, nil, err
	}
	if manifest.GetMoeLayout() == nil {
		return nil, nil, nil, errors.New("model has no MoE layout")
	}
	if err := core.ValidateMoeLayout(manifest); err != nil {
		return nil, nil, nil, err
	}
	return manifest, genesis, root, nil
}

// validateMoeRegistration verifies MoE unit bindings at registration time:
// expert units must prove inclusion in the manifest's expert weights tree.
func (s *State) validateMoeRegistration(p *pb.ResourceProfile) error {
	switch p.MoeUnitKind {
	case "":
		return nil
	case "expert":
		manifest, _, _, err := s.moeModel(p.ChainId, p.SchemeHash)
		if err != nil {
			return err
		}
		layout := manifest.MoeLayout
		if len(p.MoeUnits) == 0 {
			return errors.New("expert registration holds no units")
		}
		seen := map[[2]uint32]bool{}
		for _, unit := range p.MoeUnits {
			key := [2]uint32{unit.Layer, unit.Expert}
			if seen[key] || unit.Layer >= layout.NumLayers || unit.Expert >= layout.ExpertsPerLayer {
				return errors.New("invalid or duplicate expert unit")
			}
			seen[key] = true
			if !core.VerifyMoeExpertProof(layout.ExpertWeightsRoot, layout.NumLayers, layout.ExpertsPerLayer,
				unit.Layer, unit.Expert, unit.ParamsHash, unit.MerkleProof, unit.LeafIndex) {
				return errors.New("expert unit Merkle proof invalid")
			}
		}
		return nil
	case "attention":
		manifest, _, _, err := s.moeModel(p.ChainId, p.SchemeHash)
		if err != nil {
			return err
		}
		if p.ShardTotalLayers != manifest.TotalLayers {
			return errors.New("attention registration must pin the model layer count")
		}
		return nil
	}
	return errors.New("unknown MoE unit kind")
}

// moeHolderCounts counts live registered holders per expert unit.
func (s *State) moeHolderCounts(modelID string, height int64, schemes ...[]byte) map[[2]uint32]uint32 {
	counts := map[[2]uint32]uint32{}
	for id := range s.Nodes {
		p := s.ActiveProfile(id, height)
		if p == nil || p.ChainId != modelID || (len(schemes) > 0 && !bytes.Equal(p.SchemeHash, schemes[0])) || p.MoeUnitKind != "expert" {
			continue
		}
		for _, unit := range p.MoeUnits {
			counts[[2]uint32{unit.Layer, unit.Expert}]++
		}
	}
	return counts
}

func (s *State) freezeMoe(signer string, a Action, height int64) error {
	if !s.Parameters.MoeEnabled {
		return errors.New("MoE execution is not enabled")
	}
	if a.HoldID == "" || s.Holds[a.HoldID] != nil || a.Payer != signer || a.Amount <= 0 ||
		len(a.MoeAuthorizations) == 0 || a.MoeInputTokens == 0 || a.MoeMaxTokens == 0 {
		return errors.New("invalid MoE freeze")
	}
	if comp := s.Models[a.ModelID].AuditComp; comp != nil && comp.FormulaVer != 3 {
		return errors.New("MoE models require audit compensation formula v3")
	}
	// Resolve the model from the first authorization, then require all
	// authorizations to belong to it.
	first := s.ActiveProfile(a.MoeAuthorizations[0].NodeID, height)
	if first == nil {
		return errors.New("unregistered MoE capability")
	}
	modelID := first.ChainId
	manifest, _, _, err := s.moeModel(modelID, first.SchemeHash)
	if err != nil {
		return err
	}
	layout := manifest.MoeLayout

	// Coverage gate: every expert unit needs at least two live registered
	// holders chain-wide, or the request may not be frozen at all.
	holderCounts := s.moeHolderCounts(modelID, height, first.SchemeHash)
	snapshot := make([]MoeHolderSnapshot, 0, layout.NumLayers*layout.ExpertsPerLayer)
	for layer := uint32(0); layer < layout.NumLayers; layer++ {
		for expert := uint32(0); expert < layout.ExpertsPerLayer; expert++ {
			n := holderCounts[[2]uint32{layer, expert}]
			if n < 2 {
				return fmt.Errorf("expert coverage gate: layer %d expert %d has %d live holders", layer, expert, n)
			}
			snapshot = append(snapshot, MoeHolderSnapshot{Layer: layer, Expert: expert, Holders: n})
		}
	}

	totalPositions := a.MoeInputTokens + a.MoeMaxTokens
	frames := core.MoeFrames(a.MoeInputTokens, a.MoeMaxTokens, layout.MaxFramePositions)
	if frames == nil {
		return errors.New("invalid MoE frame plan")
	}
	offsets := core.MoeFrameOffsets(frames)
	// Per-expert reserve caps: the uniform beta cap, raised per expert to
	// its manifest-pinned routing-share envelope where one is pinned
	// (core.ExpertRoutingCap). Reserving with the same cap the settlement
	// check enforces keeps worst-case work attribution consistent.
	maxCapE := core.MaxExpertRoutingCap(layout, totalPositions)

	workByNode := map[string]uint64{}
	var totalWork uint64
	addWork := func(node string, work uint64) {
		workByNode[node] += work
		totalWork += work
	}
	attentionSpans := [][2]uint32{}
	coveredUnits := map[[2]uint32]bool{}
	seenAuth := map[string]bool{}
	authorizations := make([]MoeAuthorization, 0, len(a.MoeAuthorizations))
	for _, auth := range a.MoeAuthorizations {
		p := s.ActiveProfile(auth.NodeID, height)
		if p == nil || (s.Version >= 10 && !s.ExecutionAllowed(auth.NodeID, height)) || auth.NodeID == signer || p.ChainId != modelID || p.MoeUnitKind != auth.Kind || !bytes.Equal(p.SchemeHash, first.SchemeHash) {
			return errors.New("unregistered MoE capability")
		}
		if err := s.validatePublishedNode(p); err != nil {
			return err
		}
		auth.Profile = append([]byte(nil), s.Nodes[auth.NodeID].Profile...)
		switch auth.Kind {
		case "attention":
			if !HasRole(p, "moe-attention") || p.ShardStartLayer != auth.StartLayer || p.ShardEndLayer != auth.EndLayer ||
				auth.StartLayer >= auth.EndLayer || auth.EndLayer > layout.NumLayers {
				return errors.New("attention authorization differs from registration")
			}
			key := fmt.Sprintf("a/%s/%d/%d", auth.NodeID, auth.StartLayer, auth.EndLayer)
			if seenAuth[key] {
				return errors.New("duplicate MoE authorization")
			}
			seenAuth[key] = true
			attentionSpans = append(attentionSpans, [2]uint32{auth.StartLayer, auth.EndLayer})
			var work uint64
			for i, size := range frames {
				work += core.AttentionFlops(layout, auth.StartLayer, auth.EndLayer, size, offsets[i])
			}
			if p.ShardHasLmHead {
				work += uint64(a.MoeMaxTokens) * core.HeadFlopsPerToken(layout)
			}
			addWork(auth.NodeID, work)
		case "expert":
			if !HasRole(p, "moe-expert") || len(auth.Units) == 0 {
				return errors.New("expert authorization differs from registration")
			}
			held := map[[2]uint32]bool{}
			for _, unit := range p.MoeUnits {
				held[[2]uint32{unit.Layer, unit.Expert}] = true
			}
			var work uint64
			for _, unit := range auth.Units {
				if !held[[2]uint32{unit.Layer, unit.Expert}] {
					return errors.New("expert authorization names an unheld unit")
				}
				coveredUnits[[2]uint32{unit.Layer, unit.Expert}] = true
				work += core.ExpertRoutingCap(layout, unit.Layer, unit.Expert, totalPositions) * 2 * core.MoeExpertParamCount(layout, unit.Layer, unit.Expert)
			}
			key := fmt.Sprintf("e/%s", auth.NodeID)
			if seenAuth[key] {
				return errors.New("duplicate MoE authorization")
			}
			seenAuth[key] = true
			addWork(auth.NodeID, work)
		default:
			return errors.New("unknown MoE authorization kind")
		}
		authorizations = append(authorizations, auth)
	}
	// Backbone tiling + full unit coverage by the plan.
	sort.Slice(attentionSpans, func(i, j int) bool { return attentionSpans[i][0] < attentionSpans[j][0] })
	var cursor uint32
	for _, span := range attentionSpans {
		if span[0] != cursor {
			return errors.New("MoE backbone does not tile the layer axis")
		}
		cursor = span[1]
	}
	if cursor != layout.NumLayers {
		return errors.New("MoE backbone does not tile the layer axis")
	}
	if uint64(len(coveredUnits)) != uint64(layout.NumLayers)*uint64(layout.ExpertsPerLayer) {
		return errors.New("MoE plan does not cover every expert unit")
	}
	if totalWork == 0 {
		return errors.New("MoE plan has no work")
	}
	// Worst-case reservations, proportional to capped work.
	reserved := map[string]int64{}
	for i := range authorizations {
		node := authorizations[i].NodeID
		n := new(big.Int).Mul(big.NewInt(a.Amount), new(big.Int).SetUint64(workByNode[node]))
		n.Add(n, new(big.Int).SetUint64(totalWork-1))
		n.Quo(n, new(big.Int).SetUint64(totalWork))
		if !n.IsInt64() {
			return errors.New("MoE exposure overflow")
		}
		authorizations[i].Reserved = n.Int64()
		reserved[node] += n.Int64()
	}
	for id, value := range reserved {
		if value > s.Accounts[id].Stake/s.newExposureDivisor(id)-s.exposure(id) {
			return errors.New("MoE stake exposure limit")
		}
	}
	if err := s.debitBalance(signer, a.Amount); err != nil {
		return err
	}
	f, err := s.freezeFee(a, modelID, "")
	if err != nil {
		return err
	}
	worstSeg := maxCapE
	if uint64(layout.MaxFramePositions) > worstSeg {
		worstSeg = uint64(layout.MaxFramePositions)
	}
	worstSegRows := uint32(8)
	if worstSeg > uint64(^uint32(0))/8 {
		worstSegRows = ^uint32(0)
	} else {
		worstSegRows = uint32(8 * worstSeg)
	}
	s.Holds[a.HoldID] = &Hold{PrivacyTier: "P0", TokenFee: f, ID: a.HoldID, Payer: signer,
		Amount: a.Amount, State: "frozen", ModelID: modelID, FreezeHeight: height,
		ExpireHeight: height + s.Parameters.HoldTimeout,
		Moe: &MoeHold{SchemeHash: append([]byte(nil), first.SchemeHash...), Authorizations: authorizations, InputTokens: a.MoeInputTokens,
			MaxOutputTokens: a.MoeMaxTokens, Holders: snapshot, WorstSegRows: worstSegRows}}
	return nil
}

func (s *State) submitMoe(signer string, a Action, height int64) error {
	if h := s.Holds[a.HoldID]; h != nil && h.Moe != nil && h.Moe.ChunkTotal > 0 {
		return errors.New("chunked MoE submission requires finalize")
	}
	return s.deriveMoeSubmission(signer, a, height, true)
}

// deriveMoeSubmission is the single validation/settlement derivation for both
// transports. Only retention of the raw receipt differs.
func (s *State) deriveMoeSubmission(signer string, a Action, height int64, retainReceipt bool) error {
	h := s.Holds[a.HoldID]
	if h == nil || h.Moe == nil || h.State != "frozen" || height >= h.ExpireHeight ||
		!s.Coordinators[signer] || a.Actual <= 0 || a.Actual > h.Amount {
		return errors.New("invalid MoE submission")
	}
	receipt := &pb.PipelineExecutionReceipt{}
	if proto.Unmarshal(a.Receipt, receipt) != nil || receipt.RequestId != h.ID ||
		receipt.InputTokens != h.Moe.InputTokens || receipt.OutputTokens == 0 ||
		receipt.OutputTokens > h.Moe.MaxOutputTokens || receipt.ElapsedMs <= 0 ||
		len(receipt.OutputCommitment) != 32 || receipt.Moe == nil {
		return errors.New("invalid MoE aggregate")
	}
	if err := verifyTokenFee(h.TokenFee, receipt.InputTokens, receipt.OutputTokens, a.Actual, nil); err != nil {
		return err
	}
	manifest, genesis, root, err := s.moeModel(h.ModelID, h.Moe.SchemeHash)
	if err != nil {
		return err
	}
	bindings := map[string]core.MoeProfileBinding{}
	for _, auth := range h.Moe.Authorizations {
		p := &pb.ResourceProfile{}
		if proto.Unmarshal(auth.Profile, p) != nil {
			return errors.New("invalid frozen MoE profile")
		}
		b := bindings[auth.NodeID]
		b.UnitKind = auth.Kind
		if auth.Kind == "attention" {
			b.ParamsHash = p.ShardParametersHash
			b.StartLayer, b.EndLayer = p.ShardStartLayer, p.ShardEndLayer
			b.HasHead = p.ShardHasLmHead
		} else {
			if b.ExpertUnits == nil {
				b.ExpertUnits = map[[2]uint32][]byte{}
			}
			for _, unit := range p.MoeUnits {
				b.ExpertUnits[[2]uint32{unit.Layer, unit.Expert}] = unit.ParamsHash
			}
		}
		bindings[auth.NodeID] = b
	}
	work, err := core.VerifyMoeExecution(manifest, receipt, h.ModelID, root, genesis.ExecutionProfileVer, bindings)
	if err != nil {
		return err
	}
	// Scarcity weighting from the frozen holder snapshot.
	counts := make([]uint32, 0, len(h.Moe.Holders))
	holdersByUnit := map[[2]uint32]uint32{}
	for _, snap := range h.Moe.Holders {
		counts = append(counts, snap.Holders)
		holdersByUnit[[2]uint32{snap.Layer, snap.Expert}] = snap.Holders
	}
	median := core.MedianHolders(counts)
	layout := manifest.MoeLayout
	weightedSeg := make([]core.WorkShare, 0, len(work.Instances))
	nodeWork := map[string]uint64{}
	segNode := map[string]string{}
	instByKey := map[string]core.MoeSegmentWork{}
	for _, seg := range work.Instances {
		w := new(big.Int).SetUint64(seg.Flops)
		if seg.Kind == "expert" {
			ppm := core.ScarcityMultiplierPPM(median, holdersByUnit[[2]uint32{seg.Layer, seg.Expert}], layout.ScarcityMax)
			w.Mul(w, new(big.Int).SetUint64(ppm))
			w.Quo(w, big.NewInt(1_000_000))
		}
		if !w.IsUint64() {
			return errors.New("MoE weighted work overflow")
		}
		key := moeSegmentKey(seg.Kind, seg.Frame, seg.Layer, seg.Expert, seg.End) + "/" + seg.NodeID
		segNode[key] = seg.NodeID
		instByKey[key] = seg
		weightedSeg = append(weightedSeg, core.WorkShare{NodeID: key, Work: w.Uint64()})
		nodeWork[seg.NodeID] += w.Uint64()
	}
	segShares, err := core.SplitWorkCharge(a.Actual, weightedSeg)
	if err != nil {
		return err
	}
	dues := make([]MoeSegmentDue, 0, len(segShares))
	for _, share := range segShares {
		seg := instByKey[share.NodeID]
		dues = append(dues, MoeSegmentDue{Key: share.NodeID, NodeID: seg.NodeID, Kind: seg.Kind,
			Layer: seg.Layer, Expert: seg.Expert, Frame: seg.Frame, End: seg.End, Amount: share.Amount})
	}
	nodeShares := make([]core.WorkShare, 0, len(nodeWork))
	for node, w := range nodeWork {
		nodeShares = append(nodeShares, core.WorkShare{NodeID: node, Work: w})
	}
	credits, err := core.SplitWorkCharge(a.Actual, nodeShares)
	if err != nil {
		return err
	}
	// Frozen challenge plan: candidates with per-instance rows for comp.
	cands := core.MoeSegmentCandidates(receipt.Moe, work)
	rowsByRef := map[string]uint32{}
	for _, seg := range work.Instances {
		rowsByRef[moeSegmentKey(seg.Kind, seg.Frame, seg.Layer, seg.Expert, seg.End)] = uint32(seg.Rows)
	}
	candidates := make([]MoeCandidate, 0, len(cands))
	for _, cand := range cands {
		key := moeSegmentKey(cand.Ref.Kind, cand.Ref.Frame, cand.Ref.Layer, cand.Ref.Expert, cand.Ref.EndLayer)
		candidates = append(candidates, MoeCandidate{Kind: cand.Ref.Kind, Layer: cand.Ref.Layer,
			Expert: cand.Ref.Expert, Frame: cand.Ref.Frame, End: cand.Ref.EndLayer,
			Work: cand.Work, Rows: rowsByRef[key]})
	}
	challengeCount := (len(candidates) + 7) / 8
	if challengeCount < 2 {
		challengeCount = 2
	}
	if challengeCount > 8 {
		challengeCount = 8
	}
	if challengeCount > len(candidates) {
		challengeCount = len(candidates)
	}
	ref, err := core.PipelineReceiptRef(receipt)
	if err != nil {
		return err
	}
	if h.TokenFee != nil {
		h.TokenFee.OutputTokens = receipt.OutputTokens
	}
	h.Actual = a.Actual
	h.ReceiptRef = hex.EncodeToString(ref)
	h.State = "dispute"
	h.SubmitHeight = height
	h.SettleHeight = height + s.Parameters.DisputeBlocks
	h.AuditInputTokens = receipt.InputTokens
	h.AuditOutputTokens = receipt.OutputTokens
	if retainReceipt {
		h.Moe.Receipt = append([]byte(nil), a.Receipt...)
	}
	h.Moe.Credits = credits
	h.Moe.SegmentDues = dues
	h.Moe.Candidates = candidates
	h.Moe.ChallengeCount = challengeCount
	return nil
}

func (s *State) selectMoeAuditor(h *Hold, exclude string, at ...int64) (string, []byte, error) {
	excluded := map[string]bool{exclude: true}
	if s.Parameters.Scheduling != nil && s.Parameters.Scheduling.ConsumerCrossCheck != nil {
		excluded[h.Payer] = true
	}
	for _, auth := range h.Moe.Authorizations {
		excluded[auth.NodeID] = true
	}
	ids := []string{}
	if s.Parameters.RegisteredAuditors {
		source := s.finalizedState()
		var producer *pb.ResourceProfile
		for _, auth := range h.Moe.Authorizations {
			if auth.Kind == "attention" && len(auth.Profile) > 0 {
				producer = &pb.ResourceProfile{}
				if proto.Unmarshal(auth.Profile, producer) != nil {
					return "", nil, errors.New("invalid frozen producer")
				}
				break
			}
		}
		if producer == nil {
			return "", nil, errors.New("no frozen MoE producer profile")
		}
		for id := range source.Nodes {
			if !excluded[id] && source.EligibleAuditorProfile(id, "", producer, source.Height) {
				ids = append(ids, id)
			}
		}
	} else {
		for id, enabled := range s.Auditors {
			if enabled && !excluded[id] {
				ids = append(ids, id)
			}
		}
	}
	return s.drawAuditor(h, ids, at...)
}

func moeRefKey(ref *pb.SegmentRef) string {
	return moeSegmentKey(ref.Kind, ref.Frame, ref.Layer, ref.Expert, ref.EndLayer)
}

// citeMoeAudit records segment work for formula v3: the challenge must name
// exactly the chain-derived draw, and the verdict must price every drawn
// segment with a distance.
func (h *Hold) citeMoeAudit(raw []byte, v *pb.AuditVerdict, challenge *pb.AuditChallenge) error {
	if v.NOutBilled != 0 {
		return errors.New("MoE verdicts bill segment work, not output prefix")
	}
	if len(h.AuditWorks) == 0 {
		return errors.New("no audit work to cite")
	}
	w := &h.AuditWorks[len(h.AuditWorks)-1]
	if w.Replayed || w.Auditor != v.AuditorId {
		return errors.New("audit work does not match assignment")
	}
	// The draw is recomputed by the caller-side state in validateMoeFaults;
	// here we bind challenge and verdict to the same stored plan by
	// comparing against the challenge the chain expects for this round.
	expected, err := h.moeExpectedDraw()
	if err != nil {
		return err
	}
	if len(challenge.SegmentRefs) != len(expected) {
		return errors.New("challenge segment set differs from frozen draw")
	}
	want := map[string]bool{}
	for _, ref := range expected {
		want[moeRefKey(ref)] = true
	}
	for _, ref := range challenge.SegmentRefs {
		if !want[moeRefKey(ref)] {
			return errors.New("challenge segment set differs from frozen draw")
		}
	}
	if len(v.SegmentDistances) != len(expected) {
		return errors.New("verdict does not price every drawn segment")
	}
	rowsByKey := map[string]uint32{}
	for _, cand := range h.Moe.Candidates {
		rowsByKey[moeSegmentKey(cand.Kind, cand.Frame, cand.Layer, cand.Expert, cand.End)] = cand.Rows
	}
	var segRows uint32
	seen := map[string]bool{}
	for _, dist := range v.SegmentDistances {
		key := moeRefKey(dist.Ref)
		if !want[key] || seen[key] {
			return errors.New("verdict prices an undrawn or duplicate segment")
		}
		seen[key] = true
		segRows += rowsByKey[key]
	}
	w.SegRows = segRows
	w.SegCount = uint32(len(expected))
	w.Prefix = 0
	w.Replayed = true
	w.Verdict = append([]byte(nil), raw...)
	return nil
}

// moeExpectedDraw recomputes the segment draw for the current audit round
// from the round's assignment seed (hold.AuditProof). Round >= 2 always
// re-draws the first round's fault segments (directed corroboration) and
// fills the rest of the plan from the new round's seed.
func (h *Hold) moeExpectedDraw() ([]*pb.SegmentRef, error) {
	seed, err := hex.DecodeString(h.AuditProof)
	if err != nil || len(seed) == 0 {
		return nil, errors.New("missing audit draw seed")
	}
	cands := make([]core.MoeSegmentCandidate, 0, len(h.Moe.Candidates))
	for _, cand := range h.Moe.Candidates {
		ref := &pb.SegmentRef{Kind: cand.Kind, Layer: cand.Layer, Expert: cand.Expert,
			Frame: cand.Frame, EndLayer: cand.End}
		if cand.Kind == "expert" {
			ref.EndLayer = cand.Layer + 1
		}
		cands = append(cands, core.MoeSegmentCandidate{Ref: ref, Work: cand.Work})
	}
	drawn := []*pb.SegmentRef{}
	used := map[string]bool{}
	if h.AuditRound >= 2 {
		for _, key := range h.Moe.FirstFaultSegments {
			ref, err := moeParseSegmentKey(key)
			if err != nil {
				return nil, err
			}
			drawn = append(drawn, ref)
			used[key] = true
		}
	}
	remaining := []core.MoeSegmentCandidate{}
	for _, cand := range cands {
		if !used[moeRefKey(cand.Ref)] {
			remaining = append(remaining, cand)
		}
	}
	if fresh := h.Moe.ChallengeCount - len(drawn); fresh > 0 {
		drawn = append(drawn, core.DrawMoeSegments(seed, remaining, fresh)...)
	}
	return drawn, nil
}

// moeSegmentTolerance resolves the signed band for one segment: the verdict
// cites band_key "moe/<kind>/<producerDevice>-><referenceDevice>"; the band
// must exist in the model's genesis policy with mode "enforce".
func (s *State) moeSegmentTolerance(h *Hold, ref *pb.SegmentRef, bandKey string) (float64, error) {
	_, genesis, _, err := s.moeModel(h.ModelID, h.Moe.SchemeHash)
	if err != nil {
		return 0, err
	}
	policy := genesis.NumericAuditPolicy
	if policy == nil {
		return 0, errors.New("model has no numeric audit policy")
	}
	parts := strings.Split(bandKey, "/")
	if len(parts) != 3 || parts[0] != "moe" || parts[1] != ref.Kind {
		return 0, errors.New("invalid MoE band key")
	}
	devices := strings.Split(parts[2], "->")
	if len(devices) != 2 {
		return 0, errors.New("invalid MoE band key devices")
	}
	for _, band := range policy.Bands {
		if band.SegmentKind == ref.Kind && band.ProducerDevice == devices[0] &&
			band.ReferenceDevice == devices[1] && band.Mode == "enforce" {
			return band.Tolerance, nil
		}
	}
	return 0, errors.New("uncalibrated MoE segment band")
}

// moeFaultSegments derives fault segments and their owner nodes from a
// verdict's per-segment distances and the signed tolerance bands. Owners
// come from the frozen segment receipts, never from the verdict itself.
func (s *State) moeFaultSegments(h *Hold, v *pb.AuditVerdict) ([]string, []string, error) {
	receipt := &pb.PipelineExecutionReceipt{}
	raw, err := s.moeReceipt(h)
	if err != nil {
		return nil, nil, err
	}
	if proto.Unmarshal(raw, receipt) != nil || receipt.Moe == nil {
		return nil, nil, errors.New("invalid frozen MoE receipt")
	}
	owners := map[string]string{}
	for _, seg := range receipt.Moe.SegmentReceipts {
		owners[moeSegmentKey(seg.Kind, seg.Frame, seg.Layer, seg.Expert, seg.EndLayer)] = seg.NodeId
	}
	faultKeys := []string{}
	faultNodes := map[string]bool{}
	for _, dist := range v.SegmentDistances {
		tolerance, err := s.moeSegmentTolerance(h, dist.Ref, dist.BandKey)
		if err != nil {
			return nil, nil, err
		}
		if dist.Distance > tolerance {
			key := moeRefKey(dist.Ref)
			owner, ok := owners[key]
			if !ok {
				return nil, nil, errors.New("fault segment has no executed owner")
			}
			faultKeys = append(faultKeys, key)
			faultNodes[owner] = true
		}
	}
	sort.Strings(faultKeys)
	nodes := make([]string, 0, len(faultNodes))
	for node := range faultNodes {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	return faultKeys, nodes, nil
}

func (s *State) validateMoeFaults(h *Hold, raw []byte) error {
	v := &pb.AuditVerdict{}
	if proto.Unmarshal(raw, v) != nil || len(v.SegmentDistances) == 0 {
		return errors.New("MoE rejection lacks segment attribution")
	}
	faultKeys, faultNodes, err := s.moeFaultSegments(h, v)
	if err != nil {
		return err
	}
	if len(faultNodes) == 0 {
		return errors.New("MoE rejection attributes no fault segment")
	}
	claimed := append([]string(nil), v.FaultNodeIds...)
	sort.Strings(claimed)
	if len(claimed) != len(faultNodes) {
		return errors.New("MoE fault nodes differ from segment derivation")
	}
	for i := range claimed {
		if claimed[i] != faultNodes[i] {
			return errors.New("MoE fault nodes differ from segment derivation")
		}
	}
	if h.AuditRound == 1 {
		h.Moe.FirstFaultSegments = faultKeys
		return nil
	}
	// Corroboration round: every first-round fault segment must be drawn
	// again (guaranteed by the draw) and must still be at fault.
	confirmed := map[string]bool{}
	for _, key := range faultKeys {
		confirmed[key] = true
	}
	for _, key := range h.Moe.FirstFaultSegments {
		if !confirmed[key] {
			return errors.New("confirming auditor cleared a first-round fault segment")
		}
	}
	return nil
}

func (s *State) settleMoe(h *Hold, outcome string, height int64) error {
	if h.Moe.ChunkTotal > 0 {
		if _, err := s.moeReceipt(h); err != nil {
			return err
		}
	}
	manifest, _, _, err := s.moeModel(h.ModelID, h.Moe.SchemeHash)
	if err != nil {
		return err
	}
	layout := manifest.MoeLayout
	if outcome == "accept" {
		return s.settleMoeAccept(h, height)
	}
	if outcome != "reject" && outcome != "service_failure" {
		return errors.New("invalid MoE settlement outcome")
	}
	if outcome == "service_failure" {
		if err := s.creditBalance(h.Payer, h.Amount); err != nil {
			return err
		}
		h.State = "cancelled"
		return nil
	}
	// reject
	v := &pb.AuditVerdict{}
	if proto.Unmarshal(h.VerdictBytes, v) != nil || len(v.FaultNodeIds) == 0 {
		return errors.New("MoE rejection needs signed fault attribution")
	}
	if layout.SegmentSettlement == "atomic" {
		return s.settleMoeAtomic(h, v, height)
	}
	return s.settleMoePlanB(h, v, height)
}

func (s *State) settleMoeAccept(h *Hold, height int64) error {
	if err := s.collectLatency(h); err != nil {
		return err
	}
	if err := s.refundInference(h, h.Amount-h.Actual); err != nil {
		return err
	}
	credits := h.Moe.Credits
	policy := s.Models[h.ModelID].Pricing
	if h.AuditCompPolicy != nil || policy != nil {
		parts := allocateFee(h.Actual, [3]int64{80, 15, 5})
		h.Shares = &SettlementShares{Fee: h.Actual, Executor: parts[0], Auditor: parts[1], Consumer: parts[2]}
		if policy != nil {
			parts = allocateFee(h.Actual, [3]int64{80, 15, 5})
			h.Shares = &SettlementShares{Fee: h.Actual, Executor: parts[0], Auditor: parts[1], Consumer: parts[2]}
		}
		if h.Unaudited {
			if err := s.fundAuditPool(h, parts[1], height); err != nil {
				return err
			}
		} else if h.AuditCompPolicy != nil {
			h.AuditEscrow = parts[1]
		} else if err := s.creditBalance(h.Auditor, parts[1]); err != nil {
			return err
		}
		if err := s.consumerRebate(h, parts[2]); err != nil {
			return err
		}
		credits = append([]core.StageShare(nil), credits...)
		left := parts[0]
		for i := range credits {
			n := new(big.Int).Mul(big.NewInt(credits[i].Amount), big.NewInt(parts[0]))
			if h.Actual > 0 {
				credits[i].Amount = n.Quo(n, big.NewInt(h.Actual)).Int64()
			} else {
				credits[i].Amount = 0
			}
			left -= credits[i].Amount
		}
		for i := int64(0); i < left; i++ {
			credits[i%int64(len(credits))].Amount++
		}
		for _, r := range h.AuditReceipts {
			h.AuditEscrow -= r.AuditComp.CompSubunits
		}
		if h.AuditEscrow < 0 {
			return errors.New("MoE audit funding exceeded")
		}
		if h.AuditCompPolicy != nil {
			if err := s.payAuditComp(h); err != nil {
				return err
			}
		}
		h.Moe.Credits = credits
	}
	for _, share := range credits {
		if err := s.creditBalance(share.NodeID, share.Amount); err != nil {
			return err
		}
	}
	h.State = "settled"
	if m := s.Models[h.ModelID]; m.Publication != nil {
		m.Publication.SettledRequests++
	}
	return nil
}

func (s *State) settleMoeAtomic(h *Hold, v *pb.AuditVerdict, height int64) error {
	if err := s.creditBalance(h.Payer, h.Amount); err != nil {
		return err
	}
	h.State = "cancelled"
	faults := map[string]bool{}
	for _, id := range v.FaultNodeIds {
		if faults[id] {
			return errors.New("duplicate MoE fault")
		}
		faults[id] = true
	}
	for _, share := range h.Moe.Credits {
		if !faults[share.NodeID] {
			continue
		}
		delete(faults, share.NodeID)
		slash, err := s.recordOffense(h, share.NodeID, height)
		if err != nil {
			return err
		}
		if !slash {
			continue
		}
		if err := s.slashOffender(h, share.NodeID, share.Amount, height); err != nil {
			return err
		}
	}
	if len(faults) > 0 {
		return errors.New("fault names an uncredited MoE node")
	}
	if len(h.Slashes) > 0 {
		h.State = "slashed"
	}
	return nil
}

// settleMoePlanB is the granular waterfall. The executor pool is split by
// segment dues; fault segments' dues are confiscated, honest segments are
// paid from (1) confiscated dues, (2) the unissued consumer rebate, (3)
// slash proceeds at 20x the fault dues; any slash leftover goes to the
// consumer, whose unspent freeze and the (unpaid-on-reject) audit share are
// refunded in full. Every subunit is accounted in MoeHold.PlanB.
func (s *State) settleMoePlanB(h *Hold, v *pb.AuditVerdict, height int64) error {
	faultKeys, _, err := s.moeFaultSegments(h, v)
	if err != nil {
		return err
	}
	faultSet := map[string]bool{}
	for _, key := range faultKeys {
		faultSet[key] = true
	}
	pool, auditShare, rebate := h.Actual, int64(0), int64(0)
	if h.AuditCompPolicy != nil {
		parts := allocateFee(h.Actual, [3]int64{80, 15, 5})
		pool, auditShare, rebate = parts[0], parts[1], parts[2]
	}
	// Rescale segment dues (shares of Actual) onto the executor pool.
	dueShares := make([]core.WorkShare, 0, len(h.Moe.SegmentDues))
	for _, due := range h.Moe.SegmentDues {
		dueShares = append(dueShares, core.WorkShare{NodeID: due.Key, Work: uint64(due.Amount)})
	}
	poolDues, err := core.SplitWorkCharge(pool, dueShares)
	if err != nil {
		return err
	}
	poolDueByKey := map[string]int64{}
	for _, share := range poolDues {
		poolDueByKey[share.NodeID] = share.Amount
	}
	nodeByKey := map[string]string{}
	var honestDue, confiscated int64
	faultDueByNode := map[string]int64{}
	for _, due := range h.Moe.SegmentDues {
		nodeByKey[due.Key] = due.NodeID
		segKey := due.Key
		if moeDueIsFault(due, faultSet) {
			confiscated += poolDueByKey[segKey]
			faultDueByNode[due.NodeID] += poolDueByKey[segKey]
		} else {
			honestDue += poolDueByKey[segKey]
		}
	}
	// Slash fault nodes at 20x their fault dues (stake-capped by the
	// existing slash machinery); proceeds enter the fund, not the payer.
	var slashFund int64
	for node, revenue := range faultDueByNode {
		slash, err := s.recordOffense(h, node, height)
		if err != nil {
			return err
		}
		if !slash {
			continue
		}
		amount, err := s.slashAmount(h, node, revenue, height)
		if err != nil {
			return err
		}
		slashFund += amount
	}
	available := confiscated + rebate + slashFund
	honestPaid := honestDue
	if honestPaid > available {
		honestPaid = available
	}
	rebateUsed := honestPaid - confiscated
	if rebateUsed < 0 {
		rebateUsed = 0
	}
	if rebateUsed > rebate {
		rebateUsed = rebate
	}
	// Pay honest segments, pro-rata if the fund fell short.
	if honestPaid > 0 {
		honestShares := []core.WorkShare{}
		for _, due := range h.Moe.SegmentDues {
			if !moeDueIsFault(due, faultSet) && poolDueByKey[due.Key] > 0 {
				honestShares = append(honestShares, core.WorkShare{NodeID: due.Key, Work: uint64(poolDueByKey[due.Key])})
			}
		}
		paid, err := core.SplitWorkCharge(honestPaid, honestShares)
		if err != nil {
			return err
		}
		perNode := map[string]int64{}
		for _, share := range paid {
			perNode[nodeByKey[share.NodeID]] += share.Amount
		}
		for node, amount := range perNode {
			if err := s.creditBalance(node, amount); err != nil {
				return err
			}
		}
	}
	leftover := available - honestPaid
	payerCredit := (h.Amount - h.Actual) + auditShare + leftover
	if err := s.creditBalance(h.Payer, payerCredit); err != nil {
		return err
	}
	h.Moe.PlanB = &MoePlanBRecord{
		FaultSegments: faultKeys, HonestDue: honestDue, HonestPaid: honestPaid,
		Confiscated: confiscated, RebateUsed: rebateUsed, SlashFund: slashFund,
		LeftoverToPayer: leftover, Shortfall: honestDue - honestPaid,
	}
	h.State = "cancelled"
	if len(h.Slashes) > 0 {
		h.State = "slashed"
	}
	return nil
}

// moeDueIsFault reports whether a stored per-instance segment due is one
// of the fault segment instances.
func moeDueIsFault(due MoeSegmentDue, faultSet map[string]bool) bool {
	return faultSet[moeSegmentKey(due.Kind, due.Frame, due.Layer, due.Expert, due.End)]
}
