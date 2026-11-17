package core

import (
	"encoding/json"
	"os"
	"testing"

	pb "ember/proto"
)

// The universal-expert exemption (2026-11-15 governance decision): the
// per-expert routing cap is C_e = max(ceil(beta*k*T/E), ceil(u_e*T)),
// where u_e is the expert's routing-share envelope pinned in the manifest
// (MoeLayout.routing_universal_ppm), calibrated pre-publication against
// the pinned weights. MOE-0 measured OLMoE routing concentration on 12
// real requests: at beta=4 with no envelope, all 12 breach the tight cap
// because every (request, layer) has some expert taking up to ~0.99 of
// positions. These tests replay the MOE-0 counts fixture
// (testdata/moe0_routing_counts.json, produced by the MOE-0 rerun of
// moe0_routing_beta.py's corpus with full per-(layer, expert) counts)
// against the old and new cap rules.

type moe0CountsDoc struct {
	Layers   int `json:"layers"`
	Experts  int `json:"experts"`
	TopK     int `json:"top_k"`
	Requests []struct {
		Request   int        `json:"request"`
		Positions int        `json:"positions"`
		Counts    [][]uint64 `json:"counts"`
	} `json:"requests"`
}

func loadMoe0Counts(t *testing.T) *moe0CountsDoc {
	t.Helper()
	raw, err := os.ReadFile("testdata/moe0_routing_counts.json")
	if err != nil {
		t.Fatal(err)
	}
	doc := &moe0CountsDoc{}
	if err := json.Unmarshal(raw, doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Requests) != 12 || doc.Layers != 16 || doc.Experts != 64 || doc.TopK != 8 {
		t.Fatalf("unexpected MOE-0 fixture shape: %d requests, L=%d E=%d k=%d",
			len(doc.Requests), doc.Layers, doc.Experts, doc.TopK)
	}
	return doc
}

// moe0EnvelopePPM derives the pinned envelope exactly as the calibration
// methodology prescribes: per (layer, expert), the upper envelope of the
// realized routing share count/T over the calibration corpus, in ppm.
func moe0EnvelopePPM(doc *moe0CountsDoc) []uint32 {
	table := make([]uint32, doc.Layers*doc.Experts)
	for _, req := range doc.Requests {
		for layer := 0; layer < doc.Layers; layer++ {
			for expert := 0; expert < doc.Experts; expert++ {
				ppm := (req.Counts[layer][expert]*1_000_000 + uint64(req.Positions) - 1) / uint64(req.Positions)
				if idx := layer*doc.Experts + expert; ppm > uint64(table[idx]) {
					table[idx] = uint32(ppm)
				}
			}
		}
	}
	return table
}

func moe0Layout(table []uint32) *pb.MoeLayout {
	return &pb.MoeLayout{
		NumLayers: 16, ExpertsPerLayer: 64, TopK: 8, RoutingCapBeta: 4,
		RoutingUniversalPpm: table,
	}
}

func moe0Rows(doc *moe0CountsDoc, req int) map[[2]uint32]uint64 {
	rows := map[[2]uint32]uint64{}
	r := doc.Requests[req]
	for layer := 0; layer < doc.Layers; layer++ {
		for expert := 0; expert < doc.Experts; expert++ {
			if n := r.Counts[layer][expert]; n > 0 {
				rows[[2]uint32{uint32(layer), uint32(expert)}] = n
			}
		}
	}
	return rows
}

// Acceptance: the 12 real MOE-0 requests, rejected 12/12 under the old
// tight cap at beta=4, all pass once the calibrated envelope is pinned —
// with beta unchanged at 4.
func TestMoeBetaExemptionRealRequests(t *testing.T) {
	doc := loadMoe0Counts(t)
	bare := moe0Layout(nil)
	envelope := moe0Layout(moe0EnvelopePPM(doc))
	oldBreaches, newBreaches := 0, 0
	for i, req := range doc.Requests {
		rows := moe0Rows(doc, i)
		total := uint32(req.Positions)
		var maxCount uint64
		for _, n := range rows {
			if n > maxCount {
				maxCount = n
			}
		}
		if maxCount <= RoutingCap(bare, total) {
			t.Fatalf("request %d: max count %d does not breach the old cap %d; fixture drift?",
				i, maxCount, RoutingCap(bare, total))
		}
		if err := CheckRoutingCaps(bare, rows, total); err != nil {
			oldBreaches++
		}
		if err := CheckRoutingCaps(envelope, rows, total); err != nil {
			newBreaches++
			t.Errorf("request %d: envelope cap still rejects: %v", i, err)
		}
		t.Logf("request %2d: T=%3d max_count=%3d old_cap=%3d old=reject new_cap_for_argmax<=%3d new=accept",
			i, total, maxCount, RoutingCap(bare, total), MaxExpertRoutingCap(envelope, total))
	}
	if oldBreaches != 12 {
		t.Fatalf("old rule breaches = %d, want 12", oldBreaches)
	}
	if newBreaches != 0 {
		t.Fatalf("new rule breaches = %d, want 0", newBreaches)
	}
}

func TestExpertRoutingCapExemptionMath(t *testing.T) {
	table := make([]uint32, 16*64)
	table[0*64+6] = 991_379  // layer 0 expert 6: envelope share 115/116
	table[0*64+21] = 250_000 // layer 0 expert 21: envelope below the beta bound
	layout := moe0Layout(table)
	// T=116: beta term = ceil(4*8*116/64) = 58.
	if got := ExpertRoutingCap(layout, 0, 6, 116); got != 115 {
		t.Fatalf("universal expert cap = %d, want 115 (ceil(0.991379*116))", got)
	}
	if got := ExpertRoutingCap(layout, 0, 21, 116); got != 58 {
		t.Fatalf("low-envelope expert cap = %d, want 58 (beta term dominates)", got)
	}
	if got := ExpertRoutingCap(layout, 0, 7, 116); got != 58 {
		t.Fatalf("undeclared expert cap = %d, want 58", got)
	}
	if got := MaxExpertRoutingCap(layout, 116); got != 115 {
		t.Fatalf("max cap = %d, want 115", got)
	}
	if got := UniversalSharePPM(layout, 99, 0); got != 0 {
		t.Fatalf("out-of-range share = %d, want 0", got)
	}
	// No table at all: the exemption term vanishes everywhere.
	bare := moe0Layout(nil)
	if got := MaxExpertRoutingCap(bare, 116); got != 58 {
		t.Fatalf("bare max cap = %d, want 58", got)
	}
}

// Gaming counterexample, realized-count side: a pinned envelope raises
// the cap for its own expert only. Concentration realized on any other
// expert is still judged by the tight beta cap, so a publisher cannot
// launder concentration through a universal declaration, and an executor
// cannot spend one expert's exemption on another.
func TestRoutingCapExemptionDoesNotTransfer(t *testing.T) {
	table := make([]uint32, 16*64)
	table[0*64+6] = 960_000 // layer 0 expert 6 declared near-universal
	layout := moe0Layout(table)
	// T=100: beta cap = ceil(4*8*100/64) = 50; expert 6's cap = 96.
	if err := CheckRoutingCaps(layout, map[[2]uint32]uint64{{0, 6}: 96}, 100); err != nil {
		t.Fatalf("declared expert at its envelope rejected: %v", err)
	}
	if err := CheckRoutingCaps(layout, map[[2]uint32]uint64{{0, 6}: 97}, 100); err == nil {
		t.Fatal("declared expert above its envelope accepted")
	}
	if err := CheckRoutingCaps(layout, map[[2]uint32]uint64{{0, 21}: 80}, 100); err == nil {
		t.Fatal("concentration on an undeclared expert accepted under another expert's exemption")
	}
	if err := CheckRoutingCaps(layout, map[[2]uint32]uint64{{0, 21}: 50}, 100); err != nil {
		t.Fatalf("undeclared expert at the beta cap rejected: %v", err)
	}
	// Synthetic over-concentration on a non-universal expert, shaped like
	// a real MOE-0 layer (T=58, count 32 vs beta cap 29): still rejected
	// even with a universal declaration elsewhere in the layer.
	table2 := make([]uint32, 16*64)
	table2[3*64+5] = 985_500
	layout2 := moe0Layout(table2)
	if err := CheckRoutingCaps(layout2, map[[2]uint32]uint64{{3, 45}: 32}, 58); err == nil {
		t.Fatal("over-concentration on non-universal expert accepted")
	}
}

func moe0Manifest(table []uint32) *pb.ModelManifest {
	layout := moe0Layout(table)
	layout.HiddenSize = 2048
	layout.AttentionParamCount = 1_000_000
	layout.ExpertParamCount = 100_000
	layout.VocabSize = 50_000
	layout.MaxFramePositions = 512
	layout.ScarcityMax = 8
	layout.SegmentDtype = "bf16"
	layout.ExpertWeightsRoot = make([]byte, 32)
	return &pb.ModelManifest{ChainId: "olmoe-test", TotalLayers: 16, MoeLayout: layout}
}

func TestValidateMoeLayoutUniversalTable(t *testing.T) {
	doc := loadMoe0Counts(t)
	if err := ValidateMoeLayout(moe0Manifest(nil)); err != nil {
		t.Fatalf("no table: %v", err)
	}
	if err := ValidateMoeLayout(moe0Manifest(moe0EnvelopePPM(doc))); err != nil {
		t.Fatalf("honest MOE-0 envelope table rejected: %v", err)
	}
	short := make([]uint32, 100)
	if err := ValidateMoeLayout(moe0Manifest(short)); err == nil {
		t.Fatal("short table accepted")
	}
	over := moe0EnvelopePPM(doc)
	over[0] = 1_000_001
	if err := ValidateMoeLayout(moe0Manifest(over)); err == nil {
		t.Fatal("share above 1 accepted")
	}
	// Blanket near-1.0 declarations: per-layer sum 64*0.9 = 57.6e6 ppm,
	// over the envelope budget top_k*(ceil(beta)+1) = 8*5 = 40e6 ppm.
	blanket := make([]uint32, 16*64)
	for i := range blanket {
		blanket[i] = 900_000
	}
	if err := ValidateMoeLayout(moe0Manifest(blanket)); err == nil {
		t.Fatal("blanket universal declarations accepted")
	}
}
