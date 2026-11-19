package chain

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	abci "github.com/cometbft/cometbft/abci/types"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

func spotFixture(t *testing.T) (*State, map[string]actor, []actor, *Hold) {
	t.Helper()
	s, a, nodes, auth, r := pipelineState(t)
	s.Version = 14
	s.Parameters.DisputeBlocks = 5000
	// Six generated tokens, with three signed stage invocations per token.
	r.OutputTokens = 6
	base := append([]*pb.ShardStepReceipt(nil), r.StageReceipts...)
	for step := uint32(1); step < r.OutputTokens; step++ {
		for i, old := range base {
			stage := proto.Clone(old).(*pb.ShardStepReceipt)
			stage.Step = step
			core.Sign(stage, nodes[i].key)
			r.StageReceipts = append(r.StageReceipts, stage)
		}
	}
	apply(t, s, a["payer"], 1, Action{Type: "freeze_pipeline", HoldID: "units", Payer: a["payer"].id, Amount: 6, TotalLayers: 4, PipelineAuthorizations: auth}, 2)
	raw, _ := proto.Marshal(r)
	apply(t, s, a["coordinator"], 1, Action{Type: "submit_pipeline", HoldID: "units", Actual: 6, Receipt: raw, SpotStepCount: 6, CommitmentRoot: bytes.Repeat([]byte{1}, 32)}, 3)
	apply(t, s, a["coordinator"], 2, Action{Type: "assign_audit", HoldID: "units"}, 4)
	return s, a, nodes, s.Holds["units"]
}
func gatewayFixture(t *testing.T, s *State) actor {
	g := newActor(90)
	p := &pb.ResourceProfile{NodeId: g.id, EngineCapabilities: &pb.EngineCapabilities{Roles: []string{"gateway"}}}
	core.Sign(p, g.key)
	raw, _ := proto.Marshal(p)
	s.Accounts[g.id] = &Account{Stake: 100000}
	s.Nodes[g.id] = &NodeRegistration{Profile: raw, ExpiresHeight: 1000000}
	return g
}
func spoolFor(h *Hold, g actor, received bool, height int64) *SpoolAttestation {
	c := h.Pipeline.Spot
	a := &SpoolAttestation{RequestID: c.RequestID, ChallengeHeight: c.ChallengeHeight, StepSetHash: SpotStepSetHash(c.StepSet), Gateway: g.id, Received: received, Height: height}
	a.Signature = ed25519.Sign(g.key, SpoolSigningBytes(*a))
	return a
}
func TestSpotCommitmentAndLegacyBytes(t *testing.T) {
	for _, version := range []uint64{13, 14} {
		s, a, _, auth, r := pipelineState(t)
		s.Version = version
		apply(t, s, a["payer"], 1, Action{Type: "freeze_pipeline", HoldID: "units", Payer: a["payer"].id, Amount: 6, TotalLayers: 4, PipelineAuthorizations: auth}, 2)
		raw, _ := proto.Marshal(r)
		act := Action{Type: "submit_pipeline", HoldID: "units", Actual: 6, Receipt: raw}
		if version == 14 {
			for _, root := range [][]byte{nil, make([]byte, 32), make([]byte, 31)} {
				act.CommitmentRoot = root
				if err := s.submitPipeline(a["coordinator"].id, act, 3); err == nil {
					t.Fatal("missing commitment accepted")
				}
			}
		} else {
			if err := s.submitPipeline(a["coordinator"].id, act, 3); err != nil {
				t.Fatal(err)
			}
			b, _ := s.Bytes()
			if bytes.Contains(b, []byte("commitment_root")) || bytes.Contains(b, []byte("availability")) || bytes.Contains(b, []byte("\"spot\"")) {
				t.Fatal("legacy serialization changed")
			}
		}
	}
}
func TestSpotDrawAndEntropy(t *testing.T) {
	seed := sha256.Sum256([]byte("seed"))
	for tier, k := range []uint64{1, 6, 15} {
		if SpotK(uint32(tier), 62) != k || SpotK(uint32(tier), 1) != 1 {
			t.Fatal("k")
		}
		a, e := DeriveSpotSteps(seed[:], 62, k)
		b, _ := DeriveSpotSteps(seed[:], 62, k)
		if e != nil || !reflect.DeepEqual(a, b) || len(a) != int(k) {
			t.Fatal("draw")
		}
		for i, v := range a {
			if v < 1 || v > 62 || (i > 0 && v <= a[i-1]) {
				t.Fatal("invalid set")
			}
		}
	}
	s, _, _, h := spotFixture(t)
	before := append([]uint64(nil), h.Pipeline.Spot.StepSet...)
	if s.startSpot(h, 5) == nil || !reflect.DeepEqual(before, h.Pipeline.Spot.StepSet) {
		t.Fatal("redraw")
	}
	delete(s.BlockHashes, h.SubmitHeight+1)
	if _, _, err := s.assignmentSeed(h); err == nil {
		t.Fatal("precommit entropy used")
	}
}
func TestSpotAccusationCounterproof(t *testing.T) {
	s, _, _, h := spotFixture(t)
	g := gatewayFixture(t, s)
	c := h.Pipeline.Spot
	deadline := c.Deadline
	a := Action{Type: "spot_unopened", HoldID: h.ID}
	if s.spotAction(h.Auditor, a, deadline+1) == nil {
		t.Fatal("missing spool")
	}
	a.Spool = spoolFor(h, g, false, deadline)
	if s.spotAction(h.Payer, a, deadline+1) == nil || s.spotAction(h.Auditor, a, deadline) == nil {
		t.Fatal("missing signer/deadline")
	}
	a.Spool.Signature[0] ^= 1
	if s.spotAction(h.Auditor, a, deadline+1) == nil {
		t.Fatal("bad gateway signature")
	}
	a.Spool = spoolFor(h, g, false, deadline)
	if err := s.spotAction(h.Auditor, a, deadline+1); err != nil {
		t.Fatal(err)
	}
	auditor := h.Auditor
	receipt := spoolFor(h, g, true, deadline-1)
	if err := s.spotAction(h.Pipeline.Authorizations[0].NodeID, Action{Type: "spot_counterproof", HoldID: h.ID, Spool: receipt}, deadline+2); err != nil {
		t.Fatal(err)
	}
	if !c.Received || c.Accusation != nil || s.Availability[auditor].Major != 1 {
		t.Fatal("counterproof")
	}
	for _, id := range holdExecutors(h) {
		if s.Availability[id].Major != 0 || s.Availability[id].Minor != 0 {
			t.Fatal("executor penalized")
		}
	}
}
func accuseAndAdvance(t *testing.T, s *State, h *Hold, g actor) {
	t.Helper()
	c := h.Pipeline.Spot
	at := c.Deadline + 1
	if err := s.spotAction(h.Auditor, Action{Type: "spot_unopened", HoldID: h.ID, Spool: spoolFor(h, g, false, c.Deadline)}, at); err != nil {
		t.Fatal(err)
	}
	if err := s.advanceSpot(at + SPOT_FIRST_VERDICT_BLOCKS + 1); err != nil {
		t.Fatal(err)
	}
}
func TestSpotTwoRoundsAndLateOpening(t *testing.T) {
	for _, late := range []bool{false, true} {
		s, _, nodes, h := spotFixture(t)
		g := gatewayFixture(t, s)
		set := append([]uint64(nil), h.Pipeline.Spot.StepSet...)
		stakes := []int64{}
		for _, n := range nodes {
			stakes = append(stakes, s.Accounts[n.id].Stake)
		}
		accuseAndAdvance(t, s, h, g)
		c := h.Pipeline.Spot
		if c.Round != "2" || !reflect.DeepEqual(set, c.StepSet) || len(c.History) != 1 {
			t.Fatal("resend changed set")
		}
		if late {
			if err := s.spotAction(h.Auditor, Action{Type: "spot_received", HoldID: h.ID}, c.Deadline); err != nil {
				t.Fatal(err)
			}
			for _, n := range nodes {
				if s.Availability[n.id].Minor != 1 || s.Availability[n.id].Major != 0 {
					t.Fatal("late opening count")
				}
			}
		} else {
			accuseAndAdvance(t, s, h, g)
			if h.State != "cancelled" || h.AuditVerdict != "reject_unopened" || s.Accounts[h.Payer].Balance != 1000 {
				t.Fatal("no refund")
			}
			for i, n := range nodes {
				if s.Accounts[n.id].Stake != stakes[i] || s.Availability[n.id].Major != 1 || s.Availability[n.id].Executor.UnopenedTotal != 2 {
					t.Fatal("silence slash/count")
				}
			}
		}
	}
}
func TestSpotEscalationUnopened(t *testing.T) {
	s, _, _, h := spotFixture(t)
	g := gatewayFixture(t, s)
	h.Pipeline.Spot.Round = "esc"
	h.Pipeline.Spot.Deadline = 400
	accuseAndAdvance(t, s, h, g)
	if h.State != "cancelled" || h.Pipeline.Spot.Round != "esc" {
		t.Fatal("escalation got free resend")
	}
}
func TestSpotAvailabilityWindowsAndFine(t *testing.T) {
	s, _, nodes, h := spotFixture(t)
	id := nodes[0].id
	stake := s.Accounts[id].Stake
	for i := 0; i < 6; i++ {
		s.availabilityEvent(id, true, 10+int64(i))
		r := s.Availability[id]
		if i == 1 && r.Level != 1 || i == 3 && (r.Level != 2 || r.PauseUntil != 30013) {
			t.Fatal("threshold")
		}
	}
	if s.Accounts[id].Stake != stake-1000 || s.Availability[id].Level != 3 {
		t.Fatal("T3 fine")
	}
	s.availabilityEvent(id, false, 16)
	if s.Accounts[id].Stake != stake-1000 {
		t.Fatal("minor charged")
	}
	s.availabilityEvent(id, true, 17)
	if s.Accounts[id].Stake != stake-2000 {
		t.Fatal("repeated major not charged")
	}
	h.State = "frozen"
	raw := append([]byte(nil), h.Pipeline.Receipt...)
	signer := ""
	for candidate, ok := range s.Coordinators {
		if ok {
			signer = candidate
			break
		}
	}
	if err := s.submitPipeline(signer, Action{HoldID: h.ID, Actual: 6, Receipt: raw, CommitmentRoot: bytes.Repeat([]byte{1}, 32)}, 18); err == nil || err.Error() != "executor availability paused" {
		t.Fatal("paused submit")
	}
	r := s.Availability[id]
	r.refresh(200017)
	if r.Level != 0 || r.Major != 0 || r.Minor != 0 {
		t.Fatal("window failed")
	}
	r = &AvailabilityRecord{}
	for i := 0; i < 12; i++ {
		r.Events = append(r.Events, AvailabilityEvent{Height: 1})
	}
	r.refresh(50000)
	if r.Level != 3 {
		t.Fatal("minor thresholds")
	}
	r.refresh(50001)
	if r.Level != 0 {
		t.Fatal("minor boundary")
	}
}
func spotVerdict(h *Hold, bad bool) []byte {
	c := h.Pipeline.Spot
	v := &pb.AuditVerdict{Verdict: "accept", VerdictKind: "accept"}
	if c.Round == "esc" {
		v.VerdictKind = "escalation_full_accept"
	}
	for _, step := range c.StepSet {
		v.PerStep = append(v.PerStep, &pb.SpotStepResult{Step: step, Outcome: "match"})
	}
	if bad {
		v.Verdict = "reject"
		v.VerdictKind = "reject_step"
		if c.Round == "esc" {
			v.VerdictKind = "escalation_full_reject"
		}
		v.PerStep[0].Outcome = "mismatch"
		v.PerStep[0].FirstDivergenceDistance = 1
		v.VerdictDistance = 1
	}
	raw, _ := proto.Marshal(v)
	return raw
}
func TestSpotEscalationGoodFaithAndCorroboration(t *testing.T) {
	for _, bad := range []bool{false, true} {
		s, _, _, h := spotFixture(t)
		first := h.Auditor
		h.VerdictBytes = spotVerdict(h, true)
		if err := s.validateSpotVerdict(h, h.VerdictBytes, 5); err != nil {
			t.Fatal(err)
		}
		if err := s.spotOutcome(h, first, "reject", "e", 5); err != nil {
			t.Fatal(err)
		}
		if h.Auditor == first || h.Pipeline.Spot.Round != "esc" {
			t.Fatal("no second auditor")
		}
		// Match the initially challenged step in the full-review response.
		h.VerdictBytes = spotVerdict(h, bad)
		if bad {
			v := &pb.AuditVerdict{}
			proto.Unmarshal(h.VerdictBytes, v)
			v.PerStep[0].Outcome = "match"
			v.PerStep[0].FirstDivergenceDistance = 0
			fv := &pb.AuditVerdict{}
			proto.Unmarshal(h.Pipeline.Spot.FirstVerdict, fv)
			i := fv.PerStep[0].Step - 1
			v.PerStep[i].Outcome = "mismatch"
			v.PerStep[i].FirstDivergenceDistance = 1
			h.VerdictBytes, _ = proto.Marshal(v)
		}
		if err := s.validateSpotVerdict(h, h.VerdictBytes, 6); err != nil {
			t.Fatal(err)
		}
		out := "accept"
		if bad {
			out = "reject"
		}
		if err := s.spotOutcome(h, h.Auditor, out, "e", 6); err != nil {
			t.Fatal(err)
		}
		if h.AuditVerdict != out {
			t.Fatal("wrong outcome")
		}
		if !bad && (h.Pipeline.Spot.GoodFaithAuditor != first || s.Availability[first].Major != 0 || s.NodeAuditStats[first].AuditsMisjudged != 0) {
			t.Fatal("good faith penalized")
		}
	}
}
func TestSpotSubstituteUsesNextFinalizedEntropy(t *testing.T) {
	s, _, _, h := spotFixture(t)
	s.spotReceived(h, 5)
	first := h.Auditor
	deadline := h.Pipeline.Spot.VerdictDeadline
	if err := s.advanceSpot(deadline + 1); err != nil {
		t.Fatal(err)
	}
	if s.Availability[first].Minor != 1 {
		t.Fatal("auditor timeout")
	}
	if err := s.advanceSpot(deadline + 2); err != nil {
		t.Fatal(err)
	}
	if h.Auditor != first {
		t.Fatal("unfinalized replacement")
	}
	s.BlockHashes[deadline+2] = bytes.Repeat([]byte{42}, 32)
	if err := s.advanceSpot(deadline + 3); err != nil {
		t.Fatal(err)
	}
	if h.Auditor == first || h.Pipeline.Spot.VerdictDeadline != deadline+3+SPOT_SUBSTITUTE_BLOCKS {
		t.Fatal("replacement failed")
	}
	last := h.Assignments[len(h.Assignments)-1]
	if len(last.SeedBlocks) != 1 || !bytes.Equal(last.SeedBlocks[0], s.BlockHashes[deadline+2]) {
		t.Fatal("wrong replacement entropy")
	}
}
func TestSpotMerkleGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/spot-merkle-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	json.Unmarshal(raw, &obj)
	var rows []map[string]any
	json.Unmarshal(obj["leaves"], &rows)
	leaves := [][32]byte{}
	decode := func(s string) [32]byte { b, _ := hex.DecodeString(s); var a [32]byte; copy(a[:], b); return a }
	for _, r := range rows {
		l := SpotLeaf{Step: uint64(r["step"].(float64)), InputHash: decode(r["input_hash"].(string)), OutputHash: decode(r["output_hash"].(string)), FormatHash: decode(r["format_hash"].(string)), ProfileIDHash: decode(r["profile_id_hash"].(string)), RequestID: decode(r["request_id"].(string))}
		h := SpotLeafHash(l)
		if h != decode(r["leaf_hash"].(string)) {
			t.Fatal("leaf vector")
		}
		tensor, _ := hex.DecodeString(r["tensor"].(string))
		got, err := SpotTensorBytes("f32", []uint64{1}, tensor[len(tensor)-4:])
		if err != nil || !bytes.Equal(got, tensor) || sha256.Sum256(got) != l.InputHash || SpotFormatHash("f32", []uint64{1}) != l.FormatHash {
			t.Fatal("tensor vector")
		}
		leaves = append(leaves, h)
	}
	root, _ := SpotMerkleRoot(leaves)
	var expected string
	json.Unmarshal(obj["root"], &expected)
	if root != decode(expected) {
		t.Fatal("root vector")
	}
	for i, l := range leaves {
		p, _ := SpotMerkleProof(leaves, i)
		if !VerifySpotProof(l, root, p) {
			t.Fatal("proof")
		}
		p[0].Hash[0] ^= 1
		if VerifySpotProof(l, root, p) {
			t.Fatal("forged proof")
		}
	}
}

func TestSpotSignedAuditsAndSettlement(t *testing.T) {
	for _, mode := range []string{"accept", "reject", "good_faith"} {
		reject := mode != "accept"
		s, a, nodes, h := spotFixture(t)
		firstAuditor := h.Auditor
		firstSet := append([]uint64(nil), h.Pipeline.Spot.StepSet...)
		for round := 1; round <= 2; round++ {
			aud := a["auditor"]
			if aud.id != h.Auditor {
				aud = a["auditor2"]
			}
			if aud.id != h.Auditor {
				t.Fatal("missing auditor fixture")
			}
			v := &pb.AuditVerdict{}
			proto.Unmarshal(spotVerdict(h, reject && !(mode == "good_faith" && round == 2)), v)
			if reject && !(mode == "good_faith" && round == 2) {
				for _, p := range v.PerStep {
					p.Outcome = "match"
					p.FirstDivergenceDistance = 0
				}
				for _, p := range v.PerStep {
					if p.Step == firstSet[0] {
						p.Outcome = "mismatch"
						p.FirstDivergenceDistance = 1
					}
				}
			}
			ref, _ := hex.DecodeString(h.ReceiptRef)
			proof, _ := hex.DecodeString(h.AuditProof)
			c := &pb.AuditChallenge{ReceiptRef: ref, VrfProof: proof}
			core.Sign(c, aud.key)
			cr, _ := core.Digest(c)
			v.ChallengeRef = cr
			v.AuditorId = aud.id
			v.EvidenceRef = bytes.Repeat([]byte{7}, 32)
			core.Sign(v, aud.key)
			cb, _ := proto.Marshal(c)
			vb, _ := proto.Marshal(v)
			nonce := s.Accounts[aud.id].Nonce + 1
			raw, err := Sign(s.Parameters.ChainID, aud.key, nonce, Action{Type: "audit", HoldID: h.ID, Challenge: cb, Verdict: vb})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Apply(raw, int64(4+round)); err != nil {
				t.Fatal(err)
			}
			h = s.Holds[h.ID]
			if !reject {
				break
			}
			if round == 1 && len(h.Slashes) != 0 {
				t.Fatal("single reject slashed")
			}
			if round == 1 {
				rejected(t, s, a["coordinator"], Action{Type: "settle", HoldID: h.ID, Outcome: "reject"}, 5)
				h = s.Holds[h.ID]
			}
		}
		outcome := "accept"
		if mode == "reject" {
			outcome = "reject"
		}
		apply(t, s, a["coordinator"], s.Accounts[a["coordinator"].id].Nonce+1,
			Action{Type: "settle", HoldID: h.ID, Outcome: outcome}, h.SettleHeight)
		h = s.Holds[h.ID]
		if mode == "reject" {
			if h.State != "slashed" || len(h.Slashes) != 3 || h.Slashes[0].Amount == 0 || s.Accounts[h.Payer].Balance <= 1000 {
				t.Fatalf("two-reject conviction missing: state=%s slashes=%+v balance=%d", h.State, h.Slashes, s.Accounts[h.Payer].Balance)
			}
			penalty := int64(0)
			for i, share := range h.Pipeline.Credits {
				want := max(share.Amount*20, int64(50)) // existing 1% stake floor
				penalty += want
				if h.Slashes[i].NodeID != share.NodeID || h.Slashes[i].Amount != want || s.Accounts[share.NodeID].Stake != 5000-want || s.Accounts[share.NodeID].Balance != 0 {
					t.Fatal("executor slash or balance mismatch", h.Slashes)
				}
			}
			if penalty != 180 || s.Accounts[h.Payer].Balance != 1180 || s.Accounts[a["auditor"].id].Balance != 1000 || s.Accounts[a["auditor2"].id].Balance != 1000 {
				t.Fatal("payer/auditor accounting mismatch")
			}
		} else {
			if h.State != "settled" {
				t.Fatal("accept not settled")
			}
			if s.Accounts[h.Payer].Balance != 994 || len(h.Slashes) != 0 {
				t.Fatal("accept payer accounting")
			}
			for i, n := range nodes {
				if s.Accounts[n.id].Stake != 5000 || s.Accounts[n.id].Balance != []int64{1, 4, 1}[i] {
					t.Fatal("accept executor accounting")
				}
			}
			if mode == "good_faith" && (h.Pipeline.Spot.GoodFaithAuditor != firstAuditor || s.Availability[firstAuditor].Major != 0 || s.NodeAuditStats[firstAuditor].AuditsMisjudged != 0) {
				t.Fatal("good faith auditor penalized")
			}
		}
	}
}
func TestSpotDisputeExpiry(t *testing.T) {
	s, _, _, h := spotFixture(t)
	if err := s.advanceSpot(h.SettleHeight); err != nil {
		t.Fatal(err)
	}
	if h.State != "cancelled" || s.Accounts[h.Payer].Balance != 1000 {
		t.Fatal("unresolved expiry did not refund")
	}
}

func TestSpotManifestLadderAndLoad(t *testing.T) {
	s, _ := testState(t)
	s.Version = 13
	s.Parameters.HoldTimeout = 5000
	app := &Application{committed: s}
	if err := app.ConfigureMembershipUpgrade(MembershipUpgrade{ChainID: s.Parameters.ChainID, ActivationHeight: 2}); err != nil {
		t.Fatal(err)
	}
	for height := int64(1); height <= 3; height++ {
		res, err := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: height})
		if err != nil {
			t.Fatal(err)
		}
		want := uint64(13)
		if height >= 2 {
			want = 14
		}
		dispute, timeout := int64(1), int64(5000)
		if height >= 2 {
			dispute, timeout = 5000, 7000
		}
		if app.staged.Parameters.DisputeBlocks != dispute || app.staged.Parameters.HoldTimeout != timeout {
			t.Fatal("wrong version-gated hold parameters", app.staged.Parameters)
		}
		if app.staged.Version != want || res.ConsensusParamUpdates.Version.App != want {
			t.Fatal("wrong rung")
		}
		app.Commit(context.Background(), &abci.CommitRequest{})
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(path, app.committed); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadState(path)
	if err != nil || loaded.Version != 14 {
		t.Fatal("v14 reload", err)
	}
}

func TestSpotProfileCountAndGates(t *testing.T) {
	s, _, _, h := spotFixture(t)
	s.Version = 13
	if s.spotAction(h.Auditor, Action{Type: "spot_received", HoldID: h.ID}, 5) == nil {
		t.Fatal("legacy accepted spot tx")
	}
	before, _ := s.Bytes()
	if err := s.advanceSpot(1000); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Bytes()
	if !bytes.Equal(before, after) {
		t.Fatal("legacy block state changed")
	}
}
func TestSpotSignedReceiptAndRejectFormat(t *testing.T) {
	s, a, _, h := spotFixture(t)
	aud := a["auditor"]
	if aud.id != h.Auditor {
		aud = a["auditor2"]
	}
	raw, err := Sign(s.Parameters.ChainID, aud.key, s.Accounts[aud.id].Nonce+1, Action{Type: "spot_received", HoldID: h.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Apply(raw, 5); err != nil {
		t.Fatal(err)
	}
	h = s.Holds[h.ID]
	if !h.Pipeline.Spot.Received {
		t.Fatal("signed receipt missing")
	}
	for _, outcome := range []string{"binding_fail", "format_fail"} {
		v := &pb.AuditVerdict{}
		proto.Unmarshal(spotVerdict(h, true), v)
		v.PerStep[0].Outcome = outcome
		b, _ := proto.Marshal(v)
		if err := s.validateSpotVerdict(h, b, 6); err != nil {
			t.Fatal(err)
		}
		v.VerdictDistance = 2
		b, _ = proto.Marshal(v)
		if s.validateSpotVerdict(h, b, 6) == nil {
			t.Fatal("incorrect max accepted")
		}
	}
}

// Captured by the unmodified e07e653 baseline, not recomputed expectations.
func TestSpotLegacyByteEquivalence(t *testing.T) {
	raw, err := os.ReadFile("testdata/spot-legacy-v11-v13.json")
	if err != nil {
		t.Fatal(err)
	}
	golden := map[string]string{}
	if err = json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	for _, version := range []uint64{11, 12, 13} {
		s, a, _, auth, r := pipelineState(t)
		s.Version = version
		check := func(phase string) {
			b, _ := s.Bytes()
			sum := sha256.Sum256(b)
			key := fmt.Sprintf("%d/%s", version, phase)
			if hex.EncodeToString(sum[:]) != golden[key] {
				t.Fatalf("legacy bytes changed: %s", key)
			}
		}
		apply(t, s, a["payer"], 1, Action{Type: "freeze_pipeline", HoldID: "units", Payer: a["payer"].id, Amount: 6, TotalLayers: 4, PipelineAuthorizations: auth}, 2)
		check("freeze")
		receipt, _ := proto.Marshal(r)
		apply(t, s, a["coordinator"], 1, Action{Type: "submit_pipeline", HoldID: "units", Actual: 6, Receipt: receipt}, 3)
		check("submit")
		apply(t, s, a["coordinator"], 2, Action{Type: "assign_audit", HoldID: "units"}, 4)
		check("assign")
	}
}

func TestSpotUndeliveredDoesNotBlameAuditor(t *testing.T) {
	s, _, _, h := spotFixture(t)
	c := h.Pipeline.Spot
	first := h.Auditor
	at := c.Deadline + SPOT_FIRST_VERDICT_BLOCKS + 1
	if err := s.advanceSpot(at); err != nil {
		t.Fatal(err)
	}
	if s.Availability[first].Minor != 0 || c.ReplaceAfter != 0 || c.Round != "2" {
		t.Fatal("undelivered challenge blamed auditor or did not advance")
	}
	for _, id := range spotExecutors(h) {
		if s.Availability[id].Minor != 0 || s.Availability[id].Major != 0 {
			t.Fatal("executor blamed without accusation")
		}
	}
}

func TestSpotLegacyIgnoresUnknownActionFields(t *testing.T) {
	for _, version := range []uint64{11, 12, 13} {
		s, a := testState(t)
		s.Version = version
		raw, err := Sign(s.Parameters.ChainID, a["payer"].key, 1, Action{Type: "transfer", To: a["governor"].id, Amount: 1})
		if err != nil {
			t.Fatal(err)
		}
		var obj map[string]json.RawMessage
		json.Unmarshal(raw, &obj)
		var act map[string]json.RawMessage
		json.Unmarshal(obj["action"], &act)
		act["commitment_root"] = json.RawMessage(`{"unknown":true}`)
		act["spool"] = json.RawMessage(`42`)
		obj["action"], _ = json.Marshal(act)
		raw, _ = json.Marshal(obj)
		if _, err = s.Apply(raw, 1); err != nil {
			t.Fatalf("v%d changed legacy unknown-field semantics: %v", version, err)
		}
	}
}

func TestSpotPureDeadlineProgression(t *testing.T) {
	s, _, nodes, h := spotFixture(t)
	c := h.Pipeline.Spot
	old := *c
	at := c.Deadline + 1
	if err := s.advanceSpot(at); err != nil {
		t.Fatal(err)
	}
	if c.Round != "2" || c.ChallengeHeight != at || c.Deadline != at+120 || c.VerdictDeadline != at+180 || !reflect.DeepEqual(c.StepSet, old.StepSet) || len(c.History) != 1 || c.History[0].ChallengeHeight != old.ChallengeHeight {
		t.Fatal("bad resend", c)
	}
	if err := s.advanceSpot(c.Deadline + 1); err != nil {
		t.Fatal(err)
	}
	if h.State != "cancelled" || h.AuditVerdict != "unconfirmed_delivery_timeout" || s.Accounts[h.Payer].Balance != 1000 {
		t.Fatal("bad refund", h)
	}
	for _, n := range nodes {
		r := s.Availability[n.id]
		if r.Minor != 0 || r.Major != 0 || r.Executor.UnopenedTotal != 0 || s.Accounts[n.id].Balance != 0 || s.Accounts[n.id].Stake != 5000 {
			t.Fatal("timeout attributed or paid", r)
		}
	}
	if s.Availability[h.Auditor].Minor != 0 || c.ReplaceAfter != 0 {
		t.Fatal("auditor blamed")
	}
}
func TestSpotPendingAccusationOwnsDeadline(t *testing.T) {
	s, _, _, h := spotFixture(t)
	g := gatewayFixture(t, s)
	c := h.Pipeline.Spot
	at := c.Deadline + 1
	if err := s.spotAction(h.Auditor, Action{Type: "spot_unopened", HoldID: h.ID, Spool: spoolFor(h, g, false, at)}, at); err != nil {
		t.Fatal(err)
	}
	if err := s.advanceSpot(at + 60); err != nil {
		t.Fatal(err)
	}
	if c.Round != "1" || c.Accusation == nil || c.ReplaceAfter != 0 {
		t.Fatal("pending accusation lost")
	}
	if err := s.advanceSpot(at + 61); err != nil {
		t.Fatal(err)
	}
	if c.Round != "2" || c.Accusation != nil {
		t.Fatal("accusation did not advance")
	}
}
func TestSpotEscPureDeadlineAndTotalPriority(t *testing.T) {
	for _, total := range []bool{false, true} {
		s, _, _, h := spotFixture(t)
		c := h.Pipeline.Spot
		c.Round = "esc"
		at := c.Deadline + 1
		if total {
			at = h.SettleHeight
		}
		if err := s.advanceSpot(at); err != nil {
			t.Fatal(err)
		}
		want := "unconfirmed_delivery_timeout"
		if total {
			want = "service_failure"
		}
		if h.State != "cancelled" || h.AuditVerdict != want || len(c.History) != 0 {
			t.Fatal("wrong priority", h)
		}
		for _, id := range spotExecutors(h) {
			if s.Availability[id].Major != 0 || s.Availability[id].Executor.UnopenedTotal != 0 {
				t.Fatal("blamed executor")
			}
		}
	}
}

func TestSpotHistoricalEvidenceAfterAutomaticAdvance(t *testing.T) {
	for _, counter := range []bool{false, true} {
		s, _, _, h := spotFixture(t)
		g := gatewayFixture(t, s)
		c := h.Pipeline.Spot
		absence := spoolFor(h, g, false, c.Deadline+1)
		receipt := spoolFor(h, g, true, c.Deadline)
		at := c.Deadline + 1
		if err := s.advanceSpot(at); err != nil {
			t.Fatal(err)
		}
		if err := s.spotAction(h.Auditor, Action{Type: "spot_unopened", HoldID: h.ID, Spool: absence}, at+1); err != nil {
			t.Fatal(err)
		}
		if counter {
			if err := s.spotAction(spotExecutors(h)[0], Action{Type: "spot_counterproof", HoldID: h.ID, Spool: receipt}, at+2); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.advanceSpot(at + 62); err != nil {
			t.Fatal(err)
		}
		old := &c.History[0]
		if !old.EvidenceResolved || (counter && s.Availability[old.Auditor].Major != 1) {
			t.Fatal("historical evidence not resolved")
		}
		for _, id := range spotExecutors(h) {
			want := uint64(1)
			if counter {
				want = 0
			}
			if s.Availability[id].Executor.UnopenedTotal != want || s.Availability[id].Major != 0 {
				t.Fatal("wrong historical attribution")
			}
		}
		if err := s.spotAction(h.Auditor, Action{Type: "spot_unopened", HoldID: h.ID, Spool: absence}, at+63); err == nil {
			t.Fatal("duplicate evidence")
		}
	}
}
func TestSpotTerminalEvidenceNeverRefundsTwice(t *testing.T) {
	s, _, nodes, h := spotFixture(t)
	g := gatewayFixture(t, s)
	c := h.Pipeline.Spot
	if err := s.advanceSpot(c.Deadline + 1); err != nil {
		t.Fatal(err)
	}
	at := c.Deadline + 1
	absence := spoolFor(h, g, false, at)
	if err := s.advanceSpot(at); err != nil {
		t.Fatal(err)
	}
	if err := s.spotAction(h.Auditor, Action{Type: "spot_unopened", HoldID: h.ID, Spool: absence}, at+1); err != nil {
		t.Fatal(err)
	}
	if err := s.advanceSpot(at + 62); err != nil {
		t.Fatal(err)
	}
	if err := s.advanceSpot(at + 63); err != nil {
		t.Fatal(err)
	}
	if h.State != "cancelled" || h.AuditVerdict != "reject_unopened" || s.Accounts[h.Payer].Balance != 1000 {
		t.Fatal("terminal evidence changed funds", h)
	}
	for _, n := range nodes {
		r := s.Availability[n.id]
		if r.Major != 1 || r.Executor.UnopenedTotal != 1 || s.Accounts[n.id].Balance != 0 {
			t.Fatal("bad attribution", r)
		}
	}
}
func TestSpotSubstitutePreservesEscalationBudget(t *testing.T) {
	s, _, _, h := spotFixture(t)
	c := h.Pipeline.Spot
	c.Round = "esc"
	c.Deadline = 100
	c.Received = true
	c.VerdictDeadline = 10
	if err := s.advanceSpot(11); err != nil {
		t.Fatal(err)
	}
	s.BlockHashes[12] = bytes.Repeat([]byte{42}, 32)
	if err := s.advanceSpot(13); err != nil {
		t.Fatal(err)
	}
	if c.Round != "esc" || c.Deadline != 100 || c.VerdictDeadline != 100 || c.ChallengeHeight != 4 {
		t.Fatal("substitute reset budget", c)
	}
}

func TestSpotSignedHistoricalClaimAndAssignmentQuery(t *testing.T) {
	s, a, _, h := spotFixture(t)
	g := gatewayFixture(t, s)
	c := h.Pipeline.Spot
	at := c.Deadline + 1
	absence := spoolFor(h, g, false, at)
	if err := s.advanceSpot(at); err != nil {
		t.Fatal(err)
	}
	aud := a["auditor"]
	if aud.id != h.Auditor {
		aud = a["auditor2"]
	}
	raw, err := Sign(s.Parameters.ChainID, aud.key, s.Accounts[aud.id].Nonce+1, Action{Type: "spot_unopened", HoldID: h.ID, Spool: absence})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Apply(raw, at+1); err != nil {
		t.Fatal(err)
	}
	h = s.Holds[h.ID]
	if h.Pipeline.Spot.History[0].Accusation == nil {
		t.Fatal("signed claim not stored")
	}
	s.Height = at + 1
	app := &Application{committed: s}
	for _, id := range []string{h.Auditor, "unassigned"} {
		res, err := app.Query(context.Background(), &abci.QueryRequest{Path: "/spot-assignments/" + id})
		if err != nil || res.Code != 0 {
			t.Fatal(err, res)
		}
		var snapshot struct {
			Height int64            `json:"height"`
			Holds  map[string]*Hold `json:"holds"`
		}
		if err = json.Unmarshal(res.Value, &snapshot); err != nil {
			t.Fatal(err)
		}
		want := 0
		if id == h.Auditor {
			want = 1
		}
		if len(snapshot.Holds) != want || snapshot.Height != at+1 {
			t.Fatal("bad assignment filter", snapshot)
		}
	}
}
func TestSpotHistoricalEvidenceBindingAndCutoff(t *testing.T) {
	s, _, _, h := spotFixture(t)
	g := gatewayFixture(t, s)
	c := h.Pipeline.Spot
	at := c.Deadline + 1
	absence := spoolFor(h, g, false, at)
	if err := s.advanceSpot(at); err != nil {
		t.Fatal(err)
	}
	claim := Action{Type: "spot_unopened", HoldID: h.ID, Spool: absence}
	if s.spotAction(h.Payer, claim, at+1) == nil {
		t.Fatal("wrong historical signer")
	}
	absence.Signature[0] ^= 1
	if s.spotAction(h.Auditor, claim, at+1) == nil {
		t.Fatal("bad historical gateway signature")
	}
	absence.Signature[0] ^= 1
	if s.spotAction(h.Auditor, claim, h.SettleHeight-60) == nil {
		t.Fatal("unbounded historical evidence")
	}
}

func TestSpotExpiredEscalationDoesNotBlameFreshSubstitute(t *testing.T) {
	s, _, _, h := spotFixture(t)
	c := h.Pipeline.Spot
	c.Round = "esc"
	c.Received = true
	c.Deadline = 10
	c.VerdictDeadline = 10
	first := h.Auditor
	if err := s.advanceSpot(11); err != nil {
		t.Fatal(err)
	}
	s.BlockHashes[12] = bytes.Repeat([]byte{42}, 32)
	if err := s.advanceSpot(13); err != nil {
		t.Fatal(err)
	}
	if h.Auditor != first || h.State != "cancelled" || s.Availability[first].Minor != 1 {
		t.Fatal("expired budget reassigned", h)
	}
}
