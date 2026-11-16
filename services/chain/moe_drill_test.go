package chain

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

// MOE-8 ingest: settle the REAL execution artifacts produced by the Python
// drill fleet (node/test_moe_drill.py) on a fresh chain state. Skipped
// unless MOE_DRILL_DIR points at the drill artifacts.
type moeDrillArtifacts struct {
	dir      string
	manifest *pb.ModelManifest
	genesis  *pb.ChainGenesis
	profiles map[string]*pb.ResourceProfile
	actors   map[string]actor
}

func loadDrill(t *testing.T) *moeDrillArtifacts {
	t.Helper()
	dir := os.Getenv("MOE_DRILL_DIR")
	if dir == "" {
		t.Skip("MOE_DRILL_DIR not set")
	}
	read := func(name string) []byte {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	d := &moeDrillArtifacts{dir: dir, profiles: map[string]*pb.ResourceProfile{},
		actors: map[string]actor{}}
	d.manifest = &pb.ModelManifest{}
	if err := proto.Unmarshal(read("manifest.pb"), d.manifest); err != nil {
		t.Fatal(err)
	}
	d.genesis = &pb.ChainGenesis{}
	if err := proto.Unmarshal(read("genesis.pb"), d.genesis); err != nil {
		t.Fatal(err)
	}
	var profilesJSON map[string]string
	if err := json.Unmarshal(read("profiles.json"), &profilesJSON); err != nil {
		t.Fatal(err)
	}
	var keysJSON map[string]string
	if err := json.Unmarshal(read("keys.json"), &keysJSON); err != nil {
		t.Fatal(err)
	}
	for id, hexProfile := range profilesJSON {
		raw, err := hex.DecodeString(hexProfile)
		if err != nil {
			t.Fatal(err)
		}
		p := &pb.ResourceProfile{}
		if err := proto.Unmarshal(raw, p); err != nil {
			t.Fatal(err)
		}
		d.profiles[id] = p
		seed, err := hex.DecodeString(keysJSON[id])
		if err != nil || len(seed) != ed25519.SeedSize {
			t.Fatalf("missing drill key for %s", id)
		}
		key := ed25519.NewKeyFromSeed(seed)
		d.actors[id] = actor{key: key, id: id}
	}
	return d
}

func (d *moeDrillArtifacts) state(t *testing.T) (*State, map[string]actor) {
	t.Helper()
	return d.stateSlash(t, "on")
}

// stateSlash builds the drill state with the global misjudge mode forced
// to slash ("" leaves the whole-model default in place).
func (d *moeDrillArtifacts) stateSlash(t *testing.T, slash string) (*State, map[string]actor) {
	t.Helper()
	s, a := testState(t)
	s.Parameters.MoeEnabled = true
	s.Parameters.Misjudge.CapBlockBPS = 10000
	// The drill profile runs with misjudge slashing ON for numerical
	// segment offenses: under the whole-model default (gray_gate),
	// numerical faults are refund-only and the Plan B honest-payment fund
	// can fall short — measured in this drill as a 2.88M subunit shortfall
	// before this setting was applied. Since the 2026-11-15 governance
	// change the granular MoE profile itself defaults to slash-on, so
	// TestMoeDrillIngestPlanBDefaultGovernance reaches the same outcome
	// with slash == "" (no global override).
	if slash != "" {
		s.Parameters.Misjudge.SlashMode = slash
	}
	s.Parameters.AuditWork = AuditWorkParameters{EpochBlocks: 10, MaxPerExecutor: 1000}
	graw, err := proto.Marshal(d.genesis)
	if err != nil {
		t.Fatal(err)
	}
	mraw, err := proto.Marshal(d.manifest)
	if err != nil {
		t.Fatal(err)
	}
	gh := sha256.Sum256(graw)
	mh := sha256.Sum256(mraw)
	model := Model{ID: d.manifest.ChainId, GenesisHash: hex.EncodeToString(gh[:]),
		ProfileHash: hex.EncodeToString(mh[:]), Genesis: graw, Manifest: mraw}
	if err := validateModelArtifacts(model); err != nil {
		t.Fatal(err)
	}
	s.Models[d.manifest.ChainId] = model
	comp := &AuditCompParameters{FormulaVer: 3, OFixed: 10, RIn: 100, ROut: 100,
		RSegPerKRow: 5000, MarginBPS: 20000, AuditShareBPS: 1500,
		BenchmarkRef: hex.EncodeToString(bytes.Repeat([]byte{0xab}, 32))}
	apply(t, s, a["governor"], 1, Action{Type: "set_audit_comp", ModelID: d.manifest.ChainId, AuditComp: comp}, 1)
	s.Accounts[a["payer"].id].Balance = 100_000_000
	for id, p := range d.profiles {
		s.Accounts[id] = &Account{Stake: 50_000_000}
		raw, err := proto.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		apply(t, s, d.actors[id], 1, Action{Type: "register_node", Profile: raw, ExpiresHeight: 90}, 1)
	}
	return s, a
}

func (d *moeDrillArtifacts) freezeSubmit(t *testing.T, s *State, a map[string]actor,
	receiptRaw []byte, holdID string) *pb.PipelineExecutionReceipt {
	t.Helper()
	receipt := &pb.PipelineExecutionReceipt{}
	if err := proto.Unmarshal(receiptRaw, receipt); err != nil {
		t.Fatal(err)
	}
	// Authorizations: attention stages from profiles; expert primaries =
	// the serving nodes (the receipt's expert segment signers), authorized
	// for every unit in their registered profiles — the plan covers all
	// units whether or not this request routed to them.
	auths := []MoeAuthorization{}
	seenAttn := map[string]bool{}
	serving := map[string]bool{}
	for _, seg := range receipt.Moe.SegmentReceipts {
		if seg.Kind == "expert" {
			serving[seg.NodeId] = true
		}
	}
	for id, p := range d.profiles {
		if p.MoeUnitKind == "attention" && !seenAttn[id] {
			seenAttn[id] = true
			auths = append(auths, MoeAuthorization{NodeID: id, Kind: "attention",
				StartLayer: p.ShardStartLayer, EndLayer: p.ShardEndLayer})
		}
	}
	for node := range serving {
		p := d.profiles[node]
		units := []MoeUnitRef{}
		for _, unit := range p.MoeUnits {
			units = append(units, MoeUnitRef{Layer: unit.Layer, Expert: unit.Expert})
		}
		auths = append(auths, MoeAuthorization{NodeID: node, Kind: "expert", Units: units})
	}
	apply(t, s, a["payer"], 1, Action{Type: "freeze_moe", HoldID: holdID,
		Payer: a["payer"].id, Amount: 10_000_000, MoeInputTokens: receipt.InputTokens,
		MoeMaxTokens: receipt.OutputTokens, MoeAuthorizations: auths}, 2)
	apply(t, s, a["coordinator"], 1, Action{Type: "submit_moe", HoldID: holdID,
		Actual: 6_000_000, Receipt: receiptRaw}, 3)
	apply(t, s, a["coordinator"], 2, Action{Type: "assign_audit", HoldID: holdID}, 4)
	return receipt
}

type drillAudit struct {
	Verdict string `json:"verdict"`
	Dist    []struct {
		Kind     string  `json:"kind"`
		Frame    uint32  `json:"frame"`
		Layer    uint32  `json:"layer"`
		Expert   uint32  `json:"expert"`
		End      uint32  `json:"end"`
		Distance float64 `json:"distance"`
		BandKey  string  `json:"band_key"`
	} `json:"distances"`
	Faults  []string `json:"fault_node_ids"`
	Offense *struct {
		Class string `json:"class"`
		Band  string `json:"band"`
	} `json:"offense"`
}

func (d *moeDrillArtifacts) verdict(t *testing.T, s *State, holdID string,
	auditor actor, nonce uint64, height int64, audit *drillAudit,
	cheatOwners map[string]bool, receipt *pb.PipelineExecutionReceipt) {
	t.Helper()
	h := s.Holds[holdID]
	drawn, err := h.moeExpectedDraw()
	if err != nil {
		t.Fatal(err)
	}
	owners := map[string]string{}
	for _, seg := range receipt.Moe.SegmentReceipts {
		owners[moeSegmentKey(seg.Kind, seg.Frame, seg.Layer, seg.Expert, seg.EndLayer)] = seg.NodeId
	}
	measured := map[string]float64{}
	bandKeys := map[string]string{}
	for _, dist := range audit.Dist {
		key := moeSegmentKey(dist.Kind, dist.Frame, dist.Layer, dist.Expert, dist.End)
		measured[key] = dist.Distance
		bandKeys[key] = dist.BandKey
	}
	ref, _ := hex.DecodeString(h.ReceiptRef)
	proof, _ := hex.DecodeString(h.AuditProof)
	c := &pb.AuditChallenge{ReceiptRef: ref, VrfProof: proof, SegmentRefs: drawn}
	if err := core.Sign(c, auditor.key); err != nil {
		t.Fatal(err)
	}
	cr, _ := core.Digest(c)
	v := &pb.AuditVerdict{ChallengeRef: cr, AuditorId: auditor.id,
		Verdict: audit.Verdict, EvidenceRef: bytes.Repeat([]byte{7}, 32)}
	for _, dref := range drawn {
		key := moeRefKey(dref)
		distance, ok := measured[key]
		if !ok {
			distance = 0.001
			if dref.Kind == "expert" && cheatOwners[owners[key]] {
				distance = 0.5 // every unit of the cheating nodes was tampered
			}
		}
		bandKey, ok := bandKeys[key]
		if !ok {
			bandKey = "moe/" + dref.Kind + "/cpu->cpu"
		}
		v.SegmentDistances = append(v.SegmentDistances, &pb.SegmentDistance{
			Ref: dref, Distance: distance, BandKey: bandKey})
	}
	if audit.Verdict == "reject" {
		_, nodes, err := s.moeFaultSegments(h, v)
		if err != nil {
			t.Fatal(err)
		}
		v.FaultNodeIds = nodes
		if audit.Offense != nil && audit.Offense.Class == "objective" {
			v.Offense = &pb.OffenseGrade{Class: "objective", Fault: "hash"}
		} else {
			score := 0.0
			for _, dist := range v.SegmentDistances {
				if !math.IsInf(dist.Distance, 0) && !math.IsNaN(dist.Distance) && dist.Distance > score {
					score = dist.Distance
				}
			}
			v.Offense = &pb.OffenseGrade{Class: "numerical", Score: score, Band: "moe-segment-v1"}
		}
	}
	if err := core.Sign(v, auditor.key); err != nil {
		t.Fatal(err)
	}
	cb, _ := proto.Marshal(c)
	vb, _ := proto.Marshal(v)
	apply(t, s, auditor, nonce, Action{Type: "audit", HoldID: holdID, Challenge: cb, Verdict: vb}, height)
}

func drillAuditor(a map[string]actor, s *State, holdID string) actor {
	h := s.Holds[holdID]
	if h.Auditor == a["auditor"].id {
		return a["auditor"]
	}
	return a["auditor2"]
}

func TestMoeDrillIngestAccept(t *testing.T) {
	d := loadDrill(t)
	s, a := d.state(t)
	receiptRaw, err := os.ReadFile(filepath.Join(d.dir, "receipt-honest.pb"))
	if err != nil {
		t.Fatal(err)
	}
	auditRaw, err := os.ReadFile(filepath.Join(d.dir, "audit-honest.json"))
	if err != nil {
		t.Fatal(err)
	}
	audit := &drillAudit{}
	if err := json.Unmarshal(auditRaw, audit); err != nil {
		t.Fatal(err)
	}
	receipt := d.freezeSubmit(t, s, a, receiptRaw, "drill-honest")
	h := s.Holds["drill-honest"]
	if h.State != "dispute" || h.Moe == nil || len(h.Moe.Credits) == 0 {
		t.Fatalf("post-submit state wrong: %s", h.State)
	}
	// Deterministic FLOPs split: recompute and compare against stored dues.
	totalDues := int64(0)
	for _, due := range h.Moe.SegmentDues {
		totalDues += due.Amount
	}
	if totalDues != h.Actual {
		t.Fatalf("segment dues sum %d != actual %d", totalDues, h.Actual)
	}
	d.verdict(t, s, "drill-honest", drillAuditor(a, s, "drill-honest"), 1, 5, audit, nil, receipt)
	if s.Holds["drill-honest"].AuditVerdict != "accept" {
		t.Fatal("honest drill receipt not accepted")
	}
	expected, err := ExpectedAuditReceipts(s.Holds["drill-honest"], "accept")
	if err != nil {
		t.Fatal(err)
	}
	apply(t, s, a["coordinator"], 3, Action{Type: "settle", HoldID: "drill-honest",
		Outcome: "accept", AuditReceipts: expected}, 6)
	h = s.Holds["drill-honest"]
	if h.State != "settled" {
		t.Fatalf("state = %s", h.State)
	}
	pool := int64(0)
	for _, share := range h.Moe.Credits {
		pool += share.Amount
		if got := s.Accounts[share.NodeID].Balance; got != share.Amount {
			t.Fatalf("node %s balance %d != credit %d", share.NodeID, got, share.Amount)
		}
	}
	if pool != 4_800_000 {
		t.Fatalf("executor pool = %d, want 4800000", pool)
	}
}

func TestMoeDrillIngestPlanB(t *testing.T) {
	d := loadDrill(t)
	s, a := d.state(t)
	receiptRaw, err := os.ReadFile(filepath.Join(d.dir, "receipt-cheat.pb"))
	if err != nil {
		t.Fatal(err)
	}
	auditRaw, err := os.ReadFile(filepath.Join(d.dir, "audit-cheat.json"))
	if err != nil {
		t.Fatal(err)
	}
	audit := &drillAudit{}
	if err := json.Unmarshal(auditRaw, audit); err != nil {
		t.Fatal(err)
	}
	if audit.Verdict != "reject" || len(audit.Faults) == 0 {
		t.Fatal("cheat audit artifact is not a rejection")
	}
	cheatOwners := map[string]bool{}
	for _, id := range audit.Faults {
		cheatOwners[id] = true
	}
	receipt := d.freezeSubmit(t, s, a, receiptRaw, "drill-cheat")
	auditor1 := drillAuditor(a, s, "drill-cheat")
	d.verdict(t, s, "drill-cheat", auditor1, 1, 5, audit, cheatOwners, receipt)
	h := s.Holds["drill-cheat"]
	if h.AuditVerdict != "" || h.AuditRound != 2 {
		t.Fatalf("round 1 did not escalate: %q round %d", h.AuditVerdict, h.AuditRound)
	}
	auditor2 := drillAuditor(a, s, "drill-cheat")
	if auditor2.id == auditor1.id {
		t.Fatal("round 2 reuses the first auditor")
	}
	d.verdict(t, s, "drill-cheat", auditor2, 1, 6, audit, cheatOwners, receipt)
	h = s.Holds["drill-cheat"]
	if h.AuditVerdict != "reject" {
		t.Fatalf("verdict = %q, want reject", h.AuditVerdict)
	}
	expected, err := ExpectedAuditReceipts(h, "reject")
	if err != nil {
		t.Fatal(err)
	}
	apply(t, s, a["coordinator"], 3, Action{Type: "settle", HoldID: "drill-cheat",
		Outcome: "reject", AuditReceipts: expected}, 7)
	h = s.Holds["drill-cheat"]
	if h.Moe.PlanB == nil {
		t.Fatal("no Plan B record")
	}
	plan := h.Moe.PlanB
	if plan.HonestPaid != plan.HonestDue || plan.Shortfall != 0 {
		t.Fatalf("honest segments not made whole: %+v", plan)
	}
	if plan.SlashFund == 0 || len(plan.FaultSegments) == 0 {
		t.Fatalf("no slash recorded: %+v", plan)
	}
	for _, id := range audit.Faults {
		if s.Accounts[id].Stake >= 50_000_000 {
			t.Fatalf("cheating node %s stake not slashed", id)
		}
	}
	// Honest attention nodes were paid their dues.
	paidAttention := false
	for _, due := range h.Moe.SegmentDues {
		if due.Kind == "attention" && s.Accounts[due.NodeID].Balance > 0 {
			paidAttention = true
		}
	}
	if !paidAttention {
		t.Fatal("honest attention segments were not paid")
	}
}

// TestMoeDrillIngestPlanBDefaultGovernance is TestMoeDrillIngestPlanB
// without the global slash override: the global misjudge mode stays at
// its whole-model default (gray_gate) and the drill profile's manifest
// predates the profile-level field, so the slash must come from the
// granular-MoE default alone (2026-11-15 governance decision). Before
// that change this exact setup measured a 2,882,644 subunit Plan B
// shortfall on these artifacts.
func TestMoeDrillIngestPlanBDefaultGovernance(t *testing.T) {
	d := loadDrill(t)
	s, a := d.stateSlash(t, "")
	if got := s.Parameters.Misjudge.SlashMode; got != "gray_gate" {
		t.Fatalf("premise broken: global misjudge mode = %q, want the gray_gate default", got)
	}
	if got := d.manifest.MoeLayout.MisjudgeSlashMode; got != "" {
		t.Fatalf("premise broken: drill manifest carries misjudge_slash_mode %q, want unset", got)
	}
	receiptRaw, err := os.ReadFile(filepath.Join(d.dir, "receipt-cheat.pb"))
	if err != nil {
		t.Fatal(err)
	}
	auditRaw, err := os.ReadFile(filepath.Join(d.dir, "audit-cheat.json"))
	if err != nil {
		t.Fatal(err)
	}
	audit := &drillAudit{}
	if err := json.Unmarshal(auditRaw, audit); err != nil {
		t.Fatal(err)
	}
	cheatOwners := map[string]bool{}
	for _, id := range audit.Faults {
		cheatOwners[id] = true
	}
	receipt := d.freezeSubmit(t, s, a, receiptRaw, "drill-cheat")
	auditor1 := drillAuditor(a, s, "drill-cheat")
	d.verdict(t, s, "drill-cheat", auditor1, 1, 5, audit, cheatOwners, receipt)
	auditor2 := drillAuditor(a, s, "drill-cheat")
	if auditor2.id == auditor1.id {
		t.Fatal("round 2 reuses the first auditor")
	}
	d.verdict(t, s, "drill-cheat", auditor2, 1, 6, audit, cheatOwners, receipt)
	h := s.Holds["drill-cheat"]
	if h.AuditVerdict != "reject" {
		t.Fatalf("verdict = %q, want reject", h.AuditVerdict)
	}
	expected, err := ExpectedAuditReceipts(h, "reject")
	if err != nil {
		t.Fatal(err)
	}
	apply(t, s, a["coordinator"], 3, Action{Type: "settle", HoldID: "drill-cheat",
		Outcome: "reject", AuditReceipts: expected}, 7)
	h = s.Holds["drill-cheat"]
	if h.Moe.PlanB == nil {
		t.Fatal("no Plan B record")
	}
	plan := h.Moe.PlanB
	if plan.HonestPaid != plan.HonestDue || plan.Shortfall != 0 {
		t.Fatalf("honest segments not made whole under default governance: %+v", plan)
	}
	if plan.SlashFund == 0 || len(plan.FaultSegments) == 0 {
		t.Fatalf("no slash recorded under default governance: %+v", plan)
	}
	for _, id := range audit.Faults {
		if s.Accounts[id].Stake >= 50_000_000 {
			t.Fatalf("cheating node %s stake not slashed", id)
		}
	}
}
