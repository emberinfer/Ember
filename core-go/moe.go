package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"

	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

// MoE coverage algebra v2: a request is covered when the backbone attention
// stages tile the layer axis for every frame AND, for every (frame, layer),
// the expert segment receipts exactly cover the experts named by the signed
// routing record for that layer. Work (FLOPs) is a deterministic function of
// the routing records, the segment receipt signers and manifest constants
// only — never of self-reported timings or counts outside the records.

// ---------- hashes ----------

func MoeRoutingRecordHash(record *pb.RoutingRecord) ([]byte, error) {
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(record)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write([]byte("ember/moe-routing-record/v1\x00"))
	h.Write(raw)
	return h.Sum(nil), nil
}

// MoeRoutingSetHash binds the ordered records of one stage span at one frame.
func MoeRoutingSetHash(records []*pb.RoutingRecord) ([]byte, error) {
	sorted := append([]*pb.RoutingRecord(nil), records...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Layer < sorted[j].Layer })
	h := sha256.New()
	h.Write([]byte("ember/moe-routing-set/v1\x00"))
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(len(sorted)))
	h.Write(b[:])
	for _, record := range sorted {
		rh, err := MoeRoutingRecordHash(record)
		if err != nil {
			return nil, err
		}
		h.Write(rh)
	}
	return h.Sum(nil), nil
}

func MoeSegmentReceiptHash(receipt *pb.SegmentReceipt) ([]byte, error) {
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(receipt)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write([]byte("ember/moe-segment-receipt/v1\x00"))
	h.Write(raw)
	return h.Sum(nil), nil
}

// MoeFrameRoot is the Merkle root over one node's segment receipt hashes at
// one frame. Leaves are sorted; an odd leaf is promoted unhashed (the same
// convention as the evidence trees elsewhere in the codebase).
func MoeFrameRoot(leafHashes [][]byte) []byte {
	if len(leafHashes) == 0 {
		return nil
	}
	level := append([][]byte(nil), leafHashes...)
	sort.Slice(level, func(i, j int) bool { return bytes.Compare(level[i], level[j]) < 0 })
	for len(level) > 1 {
		next := make([][]byte, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			if i+1 == len(level) {
				next = append(next, level[i])
				continue
			}
			h := sha256.New()
			h.Write([]byte("ember/moe-frame-node/v1\x00"))
			h.Write(level[i])
			h.Write(level[i+1])
			next = append(next, h.Sum(nil))
		}
		level = next
	}
	return level[0]
}

func moeExpertLeaf(layer, expert uint32, paramsHash []byte) []byte {
	h := sha256.New()
	h.Write([]byte("ember/moe-expert-leaf/v1\x00"))
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], layer)
	h.Write(b[:])
	binary.BigEndian.PutUint32(b[:], expert)
	h.Write(b[:])
	h.Write(paramsHash)
	return h.Sum(nil)
}

// MoeExpertRoot builds the expert weights Merkle root over leaves in
// (layer, expert) lexicographic order; paramsHashes is indexed layer*E+expert.
func MoeExpertRoot(layers, experts uint32, paramsHashes [][]byte) ([]byte, error) {
	if layers == 0 || experts == 0 || uint64(len(paramsHashes)) != uint64(layers)*uint64(experts) {
		return nil, errors.New("invalid expert leaf set")
	}
	leaves := make([][]byte, 0, len(paramsHashes))
	for layer := uint32(0); layer < layers; layer++ {
		for expert := uint32(0); expert < experts; expert++ {
			ph := paramsHashes[layer*experts+expert]
			if len(ph) != 32 {
				return nil, errors.New("invalid expert params hash")
			}
			leaves = append(leaves, moeExpertLeaf(layer, expert, ph))
		}
	}
	// Ordered tree (NOT sorted): position carries the (layer, expert) binding.
	level := leaves
	for len(level) > 1 {
		next := make([][]byte, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			if i+1 == len(level) {
				next = append(next, level[i])
				continue
			}
			h := sha256.New()
			h.Write([]byte("ember/moe-expert-node/v1\x00"))
			h.Write(level[i])
			h.Write(level[i+1])
			next = append(next, h.Sum(nil))
		}
		level = next
	}
	return level[0], nil
}

// VerifyMoeExpertProof checks a Merkle inclusion proof for one expert leaf
// in the ordered tree built by MoeExpertRoot. proof holds sibling hashes
// from the leaf level upward; leafIndex is layer*experts+expert.
func VerifyMoeExpertProof(root []byte, layers, experts, layer, expert uint32, paramsHash []byte, proof [][]byte, leafIndex uint32) bool {
	if len(root) != 32 || len(paramsHash) != 32 || layers == 0 || experts == 0 ||
		layer >= layers || expert >= experts || leafIndex != layer*experts+expert {
		return false
	}
	total := uint64(layers) * uint64(experts)
	node := moeExpertLeaf(layer, expert, paramsHash)
	idx := uint64(leafIndex)
	width := total
	step := 0
	for width > 1 {
		if idx%2 == 1 {
			if step >= len(proof) || len(proof[step]) != 32 {
				return false
			}
			h := sha256.New()
			h.Write([]byte("ember/moe-expert-node/v1\x00"))
			h.Write(proof[step])
			h.Write(node)
			node = h.Sum(nil)
			step++
		} else if idx+1 < width {
			if step >= len(proof) || len(proof[step]) != 32 {
				return false
			}
			h := sha256.New()
			h.Write([]byte("ember/moe-expert-node/v1\x00"))
			h.Write(node)
			h.Write(proof[step])
			node = h.Sum(nil)
			step++
		}
		// odd tail node is promoted without a sibling
		idx /= 2
		width = (width + 1) / 2
	}
	return step == len(proof) && bytes.Equal(node, root)
}

// ---------- layout ----------

func ValidateMoeLayout(manifest *pb.ModelManifest) error {
	layout := manifest.GetMoeLayout()
	if layout == nil {
		return errors.New("manifest has no MoE layout")
	}
	if layout.NumLayers == 0 || layout.NumLayers != manifest.TotalLayers ||
		layout.ExpertsPerLayer < 2 || layout.TopK == 0 || layout.TopK > layout.ExpertsPerLayer ||
		layout.HiddenSize == 0 || layout.AttentionParamCount == 0 ||
		layout.MaxFramePositions == 0 || layout.MaxFramePositions > 4096 ||
		layout.RoutingCapBeta < 1 || layout.ScarcityMax < 1 || layout.VocabSize == 0 ||
		len(layout.ExpertWeightsRoot) != 32 {
		return errors.New("invalid MoE layout constants")
	}
	if layout.ExpertParamCount == 0 && uint64(len(layout.ExpertParamCounts)) != uint64(layout.NumLayers)*uint64(layout.ExpertsPerLayer) {
		return errors.New("MoE layout lacks expert parameter counts")
	}
	for _, count := range layout.ExpertParamCounts {
		if count == 0 {
			return errors.New("zero expert parameter count")
		}
	}
	switch layout.SegmentDtype {
	case "bf16", "fp8-e4m3-block128":
	default:
		return errors.New("unsupported MoE segment dtype")
	}
	switch layout.SegmentSettlement {
	case "", "granular", "atomic":
	default:
		return errors.New("unsupported MoE segment settlement mode")
	}
	switch layout.MisjudgeSlashMode {
	case "", "on", "off":
	default:
		return errors.New("unsupported MoE misjudge slash mode")
	}
	if n := len(layout.RoutingUniversalPpm); n != 0 {
		if uint64(n) != uint64(layout.NumLayers)*uint64(layout.ExpertsPerLayer) {
			return errors.New("MoE routing universal table length mismatch")
		}
		perLayer := make([]uint64, layout.NumLayers)
		for i, ppm := range layout.RoutingUniversalPpm {
			if ppm > 1_000_000 {
				return errors.New("MoE routing universal share exceeds 1")
			}
			perLayer[uint32(i)/layout.ExpertsPerLayer] += uint64(ppm)
		}
		// Envelope budget (sanity bound, not the primary defense): a single
		// request's realized share vector sums to exactly top_k, and a
		// calibrated per-expert envelope inflates that sum (measured
		// 2.25-3.6x top_k on OLMoE). The budget top_k*(ceil(beta)+1) admits
		// honest envelopes with headroom while rejecting blanket near-1.0
		// declarations, whose per-layer sum would approach experts_per_layer.
		budget := uint64(layout.TopK) * (uint64(math.Ceil(layout.RoutingCapBeta)) + 1) * 1_000_000
		for layer, sum := range perLayer {
			if sum > budget {
				return fmt.Errorf("MoE routing universal envelope over budget at layer %d", layer)
			}
		}
	}
	return nil
}

func MoeExpertParamCount(layout *pb.MoeLayout, layer, expert uint32) uint64 {
	if len(layout.ExpertParamCounts) > 0 {
		return layout.ExpertParamCounts[layer*layout.ExpertsPerLayer+expert]
	}
	return layout.ExpertParamCount
}

// MoeFrames partitions the processed positions of a request into frames:
// the prefill (inputTokens positions) is chunked at maxFramePositions, then
// every decode step is its own one-position frame. The partition is a pure
// function of (inputTokens, outputTokens, maxFramePositions) so executor,
// chain and auditor all derive the same frames.
func MoeFrames(inputTokens, outputTokens, maxFramePositions uint32) []uint32 {
	if inputTokens == 0 || outputTokens == 0 || maxFramePositions == 0 {
		return nil
	}
	frames := []uint32{}
	remaining := inputTokens
	for remaining > maxFramePositions {
		frames = append(frames, maxFramePositions)
		remaining -= maxFramePositions
	}
	frames = append(frames, remaining)
	for i := uint32(1); i < outputTokens; i++ {
		frames = append(frames, 1)
	}
	return frames
}

// MoeFrameOffsets returns the global position offset of each frame.
func MoeFrameOffsets(frames []uint32) []uint32 {
	offsets := make([]uint32, len(frames))
	var cursor uint32
	for i, size := range frames {
		offsets[i] = cursor
		cursor += size
	}
	return offsets
}

// RoutingCap is the per-(layer, expert) per-request routing cap
// C_e = ceil(beta * k * T / E), with T the request's (frozen) position cap.
func RoutingCap(layout *pb.MoeLayout, totalPositions uint32) uint64 {
	numerator := float64(layout.RoutingCapBeta) * float64(layout.TopK) * float64(totalPositions)
	return uint64(math.Ceil(numerator / float64(layout.ExpertsPerLayer)))
}

// UniversalSharePPM returns the manifest-pinned routing-share envelope for
// (layer, expert), in parts per million of processed positions, or 0 when
// the layout pins no envelope entry for it.
func UniversalSharePPM(layout *pb.MoeLayout, layer, expert uint32) uint32 {
	table := layout.GetRoutingUniversalPpm()
	if len(table) == 0 || layer >= layout.GetNumLayers() || expert >= layout.GetExpertsPerLayer() {
		return 0
	}
	return table[layer*layout.ExpertsPerLayer+expert]
}

// ExpertRoutingCap is the per-(layer, expert) routing cap with the
// universal-expert exemption applied:
// C_e = max(ceil(beta * k * T / E), ceil(u_e * T)), where u_e is the
// expert's pinned routing-share envelope. Experts without a pinned
// envelope (or whose envelope sits below the beta bound) keep the tight
// uniform beta cap; the exemption matches an expert's genuine heat only
// and never transfers to any other expert.
func ExpertRoutingCap(layout *pb.MoeLayout, layer, expert uint32, totalPositions uint32) uint64 {
	capE := RoutingCap(layout, totalPositions)
	if ppm := UniversalSharePPM(layout, layer, expert); ppm > 0 {
		if exempt := (uint64(ppm)*uint64(totalPositions) + 999_999) / 1_000_000; exempt > capE {
			capE = exempt
		}
	}
	return capE
}

// MaxExpertRoutingCap is the largest per-expert routing cap in the layout
// at the given position count (the beta cap when no envelope is pinned).
func MaxExpertRoutingCap(layout *pb.MoeLayout, totalPositions uint32) uint64 {
	maxCap := RoutingCap(layout, totalPositions)
	for layer := uint32(0); layer < layout.GetNumLayers(); layer++ {
		for expert := uint32(0); expert < layout.GetExpertsPerLayer(); expert++ {
			if c := ExpertRoutingCap(layout, layer, expert, totalPositions); c > maxCap {
				maxCap = c
			}
		}
	}
	return maxCap
}

// CheckRoutingCaps verifies realized per-(layer, expert) routed rows —
// derived from the request's committed routing records — against the
// per-expert cap. Realized counts are the only input: a pinned envelope
// raises the cap for its own expert alone and cannot launder realized
// concentration on any other expert under the limit.
func CheckRoutingCaps(layout *pb.MoeLayout, expertRows map[[2]uint32]uint64, totalPositions uint32) error {
	for key, rows := range expertRows {
		if capE := ExpertRoutingCap(layout, key[0], key[1], totalPositions); rows > capE {
			return fmt.Errorf("routing cap exceeded at layer %d expert %d: %d > %d", key[0], key[1], rows, capE)
		}
	}
	return nil
}

// ---------- coverage + work derivation ----------

type MoeSegmentWork struct {
	NodeID string
	Kind   string // "attention" | "expert"
	Layer  uint32 // expert: layer; attention: span start
	End    uint32 // attention: span end (exclusive); expert: layer+1
	Expert uint32
	Frame  uint32 // instance frame (Instances only)
	Frames uint32 // instance count (merged Segments only)
	Rows   uint64 // token rows processed (attention: positions*layers; expert: routed rows)
	Flops  uint64
}

type MoeWork struct {
	Segments    []MoeSegmentWork // merged per (kind, layer, expert, node)
	Instances   []MoeSegmentWork // one entry per executed segment instance
	PerNode     map[string]uint64
	TotalFlops  uint64
	ExpertRows  map[[2]uint32]uint64 // (layer, expert) -> rows over the request
	Frames      []uint32
	HeadNode    string // node that ran the head-bearing stage (informational)
	FrameOwners map[uint32]map[string]bool
}

// MoeProfileBinding resolves a node ID to its registered MoE profile facts.
// The chain supplies this from live registrations; core stays state-free.
type MoeProfileBinding struct {
	UnitKind string
	// attention units: pinned span + range parameters hash
	ParamsHash []byte
	StartLayer uint32
	EndLayer   uint32
	HasHead    bool
	// expert units: (layer, expert) -> committed params hash
	ExpertUnits map[[2]uint32][]byte
}

// VerifyMoeExecution verifies MoE coverage algebra v2 over a pipeline
// aggregate receipt and derives the request's FLOPs work table. profiles
// maps node ID -> registered MoE binding for every segment signer.
func VerifyMoeExecution(manifest *pb.ModelManifest, receipt *pb.PipelineExecutionReceipt,
	chain string, manifestRoot []byte, executionProfile string,
	profiles map[string]MoeProfileBinding) (*MoeWork, error) {
	var err error
	receipt, err = ExpandMoeReceipt(receipt)
	if err != nil {
		return nil, err
	}
	layout := manifest.GetMoeLayout()
	if err := ValidateMoeLayout(manifest); err != nil {
		return nil, err
	}
	data := receipt.GetMoe()
	if data == nil || receipt.RequestId == "" || receipt.InputTokens == 0 || receipt.OutputTokens == 0 {
		return nil, errors.New("missing MoE execution data")
	}
	frames := MoeFrames(receipt.InputTokens, receipt.OutputTokens, layout.MaxFramePositions)
	if frames == nil {
		return nil, errors.New("invalid MoE frame partition")
	}
	offsets := MoeFrameOffsets(frames)
	numFrames := uint32(len(frames))

	// Routing records: exactly one per (frame, layer).
	records := make(map[[2]uint32]*pb.RoutingRecord)
	for _, record := range data.RoutingRecords {
		key := [2]uint32{record.Frame, record.Layer}
		if record.Frame >= numFrames || record.Layer >= layout.NumLayers ||
			record.Positions != frames[record.Frame] ||
			uint64(len(record.Experts)) != uint64(record.Positions)*uint64(layout.TopK) ||
			len(record.InputHash) != 32 {
			return nil, fmt.Errorf("invalid routing record at frame %d layer %d", record.Frame, record.Layer)
		}
		if _, dup := records[key]; dup {
			return nil, errors.New("duplicate routing record")
		}
		for _, expert := range record.Experts {
			if expert >= layout.ExpertsPerLayer {
				return nil, errors.New("routing record names unknown expert")
			}
		}
		records[key] = record
	}
	if uint64(len(records)) != uint64(numFrames)*uint64(layout.NumLayers) {
		return nil, errors.New("incomplete routing record set")
	}
	recordHash := make(map[[2]uint32][]byte)
	for key, record := range records {
		rh, err := MoeRoutingRecordHash(record)
		if err != nil {
			return nil, err
		}
		recordHash[key] = rh
	}
	// Per (frame, layer): routed counts per expert + cap enforcement.
	expertRows := make(map[[2]uint32]uint64)
	counts := make(map[[2]uint32]map[uint32]uint32)
	for frame := uint32(0); frame < numFrames; frame++ {
		for layer := uint32(0); layer < layout.NumLayers; layer++ {
			record := records[[2]uint32{frame, layer}]
			per := make(map[uint32]uint32)
			for _, expert := range record.Experts {
				per[expert]++
			}
			counts[[2]uint32{frame, layer}] = per
			for expert, n := range per {
				expertRows[[2]uint32{layer, expert}] += uint64(n)
			}
		}
	}
	totalPositions := receipt.InputTokens + receipt.OutputTokens - 1
	if err := CheckRoutingCaps(layout, expertRows, totalPositions); err != nil {
		return nil, err
	}

	// Segment receipts.
	type segKey struct {
		kind   string
		frame  uint32
		start  uint32
		end    uint32
		expert uint32
	}
	seen := make(map[segKey]bool)
	attentionByFrame := make(map[uint32][]*pb.SegmentReceipt)
	expertSeen := make(map[[3]uint32]*pb.SegmentReceipt) // (frame, layer, expert)
	work := &MoeWork{PerNode: map[string]uint64{}, ExpertRows: expertRows, Frames: frames,
		FrameOwners: map[uint32]map[string]bool{}}
	segWorkIndex := make(map[segKey]*MoeSegmentWork)
	nodeFrameSegs := make(map[string]map[uint32][][]byte)
	addSegHash := func(node string, frame uint32, hash []byte) {
		if nodeFrameSegs[node] == nil {
			nodeFrameSegs[node] = map[uint32][][]byte{}
		}
		nodeFrameSegs[node][frame] = append(nodeFrameSegs[node][frame], hash)
	}
	for _, seg := range data.SegmentReceipts {
		if Verify(seg) != nil || seg.NodeId != NodeID(seg.Signer) || seg.ChainId != chain ||
			!bytes.Equal(seg.ManifestRoot, manifestRoot) || seg.ExecutionProfileVer != executionProfile ||
			seg.RequestId != receipt.RequestId || seg.Frame >= numFrames ||
			len(seg.InputHash) != 32 || len(seg.OutputHash) != 32 || len(seg.ParamsHash) != 32 {
			return nil, errors.New("invalid segment receipt binding")
		}
		binding, ok := profiles[seg.NodeId]
		if !ok || binding.UnitKind != seg.Kind {
			return nil, fmt.Errorf("segment signer %s is not the registered %s holder", seg.NodeId, seg.Kind)
		}
		if seg.Kind == "attention" && !bytes.Equal(binding.ParamsHash, seg.ParamsHash) {
			return nil, fmt.Errorf("attention segment params differ from registration for %s", seg.NodeId)
		}
		if seg.Kind == "expert" {
			committed, held := binding.ExpertUnits[[2]uint32{seg.Layer, seg.Expert}]
			if !held || !bytes.Equal(committed, seg.ParamsHash) {
				return nil, fmt.Errorf("expert segment params differ from registration for %s", seg.NodeId)
			}
		}
		segHash, err := MoeSegmentReceiptHash(seg)
		if err != nil {
			return nil, err
		}
		addSegHash(seg.NodeId, seg.Frame, segHash)
		if work.FrameOwners[seg.Frame] == nil {
			work.FrameOwners[seg.Frame] = map[string]bool{}
		}
		work.FrameOwners[seg.Frame][seg.NodeId] = true
		switch seg.Kind {
		case "attention":
			if seg.StartLayer != seg.Layer || seg.EndLayer <= seg.StartLayer || seg.EndLayer > layout.NumLayers ||
				binding.StartLayer != seg.StartLayer || binding.EndLayer != seg.EndLayer ||
				seg.TokenRows != frames[seg.Frame] {
				return nil, errors.New("invalid attention segment span")
			}
			key := segKey{kind: "attention", frame: seg.Frame, start: seg.StartLayer, end: seg.EndLayer}
			if seen[key] {
				return nil, errors.New("duplicate attention segment")
			}
			seen[key] = true
			// The receipt must bind exactly this span's routing records.
			spanRecords := make([]*pb.RoutingRecord, 0, seg.EndLayer-seg.StartLayer)
			for layer := seg.StartLayer; layer < seg.EndLayer; layer++ {
				spanRecords = append(spanRecords, records[[2]uint32{seg.Frame, layer}])
			}
			setHash, err := MoeRoutingSetHash(spanRecords)
			if err != nil || !bytes.Equal(setHash, seg.RoutingRecordHash) {
				return nil, errors.New("attention segment routing set mismatch")
			}
			attentionByFrame[seg.Frame] = append(attentionByFrame[seg.Frame], seg)
			flops := attentionFlops(layout, seg.StartLayer, seg.EndLayer, frames[seg.Frame], offsets[seg.Frame])
			if binding.HasHead {
				flops += headFlops(layout) * uint64(headTokensInFrame(frames, seg.Frame, receipt.OutputTokens))
				work.HeadNode = seg.NodeId
			}
			// Rows for an attention instance counts the cumulative positions
			// a segment auditor must process to reach this frame (KV rebuild
			// from the prefix), times the span's layers.
			sw := &MoeSegmentWork{NodeID: seg.NodeId, Kind: "attention", Layer: seg.StartLayer, End: seg.EndLayer,
				Frame: seg.Frame, Frames: 1, Rows: uint64(offsets[seg.Frame]+frames[seg.Frame]) * uint64(seg.EndLayer-seg.StartLayer), Flops: flops}
			segWorkIndex[key] = sw
		case "expert":
			if seg.Layer >= layout.NumLayers || seg.Expert >= layout.ExpertsPerLayer ||
				seg.StartLayer != seg.Layer || seg.EndLayer != seg.Layer+1 {
				return nil, errors.New("invalid expert segment binding")
			}
			record := records[[2]uint32{seg.Frame, seg.Layer}]
			if !bytes.Equal(seg.RoutingRecordHash, recordHash[[2]uint32{seg.Frame, seg.Layer}]) ||
				!bytes.Equal(seg.InputHash, record.InputHash) {
				return nil, errors.New("expert segment does not consume the committed routing record")
			}
			want := counts[[2]uint32{seg.Frame, seg.Layer}][seg.Expert]
			if want == 0 || seg.TokenRows != want {
				return nil, errors.New("expert segment row count differs from routing record")
			}
			ekey := [3]uint32{seg.Frame, seg.Layer, seg.Expert}
			if _, dup := expertSeen[ekey]; dup {
				return nil, errors.New("duplicate expert segment")
			}
			expertSeen[ekey] = seg
			key := segKey{kind: "expert", frame: seg.Frame, start: seg.Layer, expert: seg.Expert}
			flops := uint64(want) * 2 * MoeExpertParamCount(layout, seg.Layer, seg.Expert)
			segWorkIndex[key] = &MoeSegmentWork{NodeID: seg.NodeId, Kind: "expert", Layer: seg.Layer, End: seg.Layer + 1,
				Expert: seg.Expert, Frame: seg.Frame, Frames: 1, Rows: uint64(want), Flops: flops}
		default:
			return nil, errors.New("unknown segment kind")
		}
	}
	// Backbone tiling per frame + activation continuity along the backbone.
	for frame := uint32(0); frame < numFrames; frame++ {
		spans := attentionByFrame[frame]
		sort.Slice(spans, func(i, j int) bool { return spans[i].StartLayer < spans[j].StartLayer })
		var cursor uint32
		for i, seg := range spans {
			if seg.StartLayer != cursor {
				return nil, fmt.Errorf("frame %d backbone gap or overlap at layer %d", frame, cursor)
			}
			if i > 0 && !bytes.Equal(seg.InputHash, spans[i-1].OutputHash) {
				return nil, fmt.Errorf("frame %d backbone activation discontinuity", frame)
			}
			cursor = seg.EndLayer
		}
		if cursor != layout.NumLayers {
			return nil, fmt.Errorf("frame %d incomplete backbone coverage", frame)
		}
		// Expert branches: exactly the routed experts of every layer.
		for layer := uint32(0); layer < layout.NumLayers; layer++ {
			for expert := range counts[[2]uint32{frame, layer}] {
				if expertSeen[[3]uint32{frame, layer, expert}] == nil {
					return nil, fmt.Errorf("frame %d layer %d expert %d routed but not executed", frame, layer, expert)
				}
			}
		}
	}
	if len(expertSeen) != totalExpertSegments(counts) {
		return nil, errors.New("expert segment set differs from routed set")
	}
	// Frame receipts: every node's frame root must match its segment set.
	frameReceipts := make(map[string]map[uint32]*pb.NodeFrameReceipt)
	for _, fr := range data.FrameReceipts {
		if Verify(fr) != nil || fr.NodeId != NodeID(fr.Signer) || fr.RequestId != receipt.RequestId || fr.Frame >= numFrames {
			return nil, errors.New("invalid frame receipt")
		}
		if frameReceipts[fr.NodeId] == nil {
			frameReceipts[fr.NodeId] = map[uint32]*pb.NodeFrameReceipt{}
		}
		if _, dup := frameReceipts[fr.NodeId][fr.Frame]; dup {
			return nil, errors.New("duplicate frame receipt")
		}
		frameReceipts[fr.NodeId][fr.Frame] = fr
	}
	for node, byFrame := range nodeFrameSegs {
		for frame, hashes := range byFrame {
			fr := frameReceipts[node][frame]
			if fr == nil || fr.SegmentCount != uint32(len(hashes)) ||
				!bytes.Equal(MoeFrameRoot(hashes), fr.SegmentsRoot) {
				return nil, fmt.Errorf("frame receipt mismatch for node %s frame %d", node, frame)
			}
		}
	}
	for node, byFrame := range frameReceipts {
		for frame := range byFrame {
			if len(nodeFrameSegs[node][frame]) == 0 {
				return nil, errors.New("frame receipt without segments")
			}
		}
	}
	// Assemble work table (sorted deterministically).
	keys := make([]segKey, 0, len(segWorkIndex))
	for key := range segWorkIndex {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.kind != b.kind {
			return a.kind < b.kind
		}
		if a.start != b.start {
			return a.start < b.start
		}
		return a.expert < b.expert
	})
	merged := make(map[segKey]*MoeSegmentWork)
	for _, key := range keys {
		sw := segWorkIndex[key]
		mkey := segKey{kind: sw.Kind, start: sw.Layer, end: sw.End, expert: sw.Expert}
		_ = key
		if prev, ok := merged[mkey]; ok {
			prev.Frames++
			prev.Rows += sw.Rows
			prev.Flops += sw.Flops
		} else {
			cp := *sw
			merged[mkey] = &cp
		}
	}
	for _, sw := range merged {
		work.Segments = append(work.Segments, *sw)
		work.PerNode[sw.NodeID] += sw.Flops
		work.TotalFlops += sw.Flops
	}
	for _, key := range keys {
		work.Instances = append(work.Instances, *segWorkIndex[key])
	}
	sort.Slice(work.Instances, func(i, j int) bool {
		a, b := work.Instances[i], work.Instances[j]
		if a.Frame != b.Frame {
			return a.Frame < b.Frame
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Layer != b.Layer {
			return a.Layer < b.Layer
		}
		if a.Expert != b.Expert {
			return a.Expert < b.Expert
		}
		return a.NodeID < b.NodeID
	})
	sort.Slice(work.Segments, func(i, j int) bool {
		a, b := work.Segments[i], work.Segments[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Layer != b.Layer {
			return a.Layer < b.Layer
		}
		if a.Expert != b.Expert {
			return a.Expert < b.Expert
		}
		return a.NodeID < b.NodeID
	})
	return work, nil
}

func totalExpertSegments(counts map[[2]uint32]map[uint32]uint32) int {
	total := 0
	for _, per := range counts {
		total += len(per)
	}
	return total
}

// headTokensInFrame: how many output tokens are produced by frames up to and
// including frameIdx — exactly one per frame from the last prefill frame
// onward; earlier prefill chunks produce none.
func headTokensInFrame(frames []uint32, frameIdx uint32, outputTokens uint32) uint32 {
	// Number of prefill frames = ceil over the partition: all frames except
	// the trailing (outputTokens-1) one-position decode frames.
	prefillFrames := uint32(len(frames)) - (outputTokens - 1)
	if frameIdx < prefillFrames-1 {
		return 0
	}
	return 1
}

// AttentionFlops is the exported form of attentionFlops for chain-side
// worst-case reservation math; identical closed form.
func AttentionFlops(layout *pb.MoeLayout, start, end, size, offset uint32) uint64 {
	return attentionFlops(layout, start, end, size, offset)
}

// HeadFlopsPerToken is the per-output-token lm_head work.
func HeadFlopsPerToken(layout *pb.MoeLayout) uint64 { return headFlops(layout) }

func headFlops(layout *pb.MoeLayout) uint64 {
	if layout.HeadParamCount > 0 {
		return 2 * layout.HeadParamCount
	}
	return 2 * uint64(layout.HiddenSize) * uint64(layout.VocabSize)
}

// attentionFlops: closed form per design §4.1 for one stage span at one
// frame. Per position t (global offset o, index i): 2*P_att (projections)
// + 4*d*s_t (QK^T and attn*V, s_t = o+i+1) + 2*d*E (router) + 2*k*d (top-k
// selection). Embedding lookup is ~0 and the head is accounted separately.
func attentionFlops(layout *pb.MoeLayout, start, end, size, offset uint32) uint64 {
	layers := uint64(end - start)
	d := uint64(layout.HiddenSize)
	s := uint64(size)
	perPosition := 2*layout.AttentionParamCount + 2*d*uint64(layout.ExpertsPerLayer) + 2*uint64(layout.TopK)*d
	contextSum := s*uint64(offset) + s*(s+1)/2
	return layers * (s*perPosition + 4*d*contextSum)
}

// ---------- scarcity weighting ----------

// ScarcityMultiplierPPM returns clamp(holdersRef/holders, 1, max) scaled by
// 1e6. holdersRef is the median live-holder count across the profile's
// expert units; units at or above the median earn the base rate.
func ScarcityMultiplierPPM(holdersRef, holders, max uint32) uint64 {
	if holders == 0 || max == 0 {
		return 1_000_000
	}
	ppm := uint64(holdersRef) * 1_000_000 / uint64(holders)
	if ppm < 1_000_000 {
		ppm = 1_000_000
	}
	if capPPM := uint64(max) * 1_000_000; ppm > capPPM {
		ppm = capPPM
	}
	return ppm
}

// MedianHolders is the deterministic median of per-unit live holder counts.
func MedianHolders(counts []uint32) uint32 {
	if len(counts) == 0 {
		return 0
	}
	sorted := append([]uint32(nil), counts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

// ---------- segment audit draw ----------

type MoeSegmentCandidate struct {
	Ref  *pb.SegmentRef
	Work uint64
}

// MoeSegmentCandidates lists every auditable segment instance of a
// verified request: each executed expert branch (frame, layer, expert) and
// each backbone stage span (frame, span), weighted by exact segment FLOPs.
func MoeSegmentCandidates(data *pb.MoeExecutionData, work *MoeWork) []MoeSegmentCandidate {
	cands := []MoeSegmentCandidate{}
	for _, inst := range work.Instances {
		ref := &pb.SegmentRef{Kind: inst.Kind, Layer: inst.Layer, Expert: inst.Expert,
			Frame: inst.Frame, EndLayer: inst.End}
		cands = append(cands, MoeSegmentCandidate{Ref: ref, Work: inst.Flops})
	}
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i].Ref, cands[j].Ref
		if a.Frame != b.Frame {
			return a.Frame < b.Frame
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Layer != b.Layer {
			return a.Layer < b.Layer
		}
		return a.Expert < b.Expert
	})
	return cands
}

// DrawMoeSegments draws n segments without replacement, weighted by work,
// from a chain-supplied seed. Deterministic for identical inputs.
func DrawMoeSegments(seed []byte, candidates []MoeSegmentCandidate, n int) []*pb.SegmentRef {
	pool := append([]MoeSegmentCandidate(nil), candidates...)
	out := []*pb.SegmentRef{}
	for len(out) < n && len(pool) > 0 {
		var total uint64
		for _, cand := range pool {
			total += cand.Work
		}
		h := sha256.New()
		h.Write([]byte("ember/moe-segment-draw/v1\x00"))
		h.Write(seed)
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(len(out)))
		h.Write(b[:])
		digest := h.Sum(nil)
		var pick uint64
		if total > 0 {
			pick = binary.BigEndian.Uint64(digest[:8]) % total
		} else {
			pick = uint64(binary.BigEndian.Uint32(digest[:4])) % uint64(len(pool))
		}
		idx := 0
		if total > 0 {
			var acc uint64
			for i, cand := range pool {
				acc += cand.Work
				if pick < acc {
					idx = i
					break
				}
			}
		} else {
			idx = int(pick)
		}
		out = append(out, pool[idx].Ref)
		pool = append(pool[:idx], pool[idx+1:]...)
	}
	return out
}
