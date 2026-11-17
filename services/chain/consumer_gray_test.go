package chain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	core "ember/core-go"
	pb "ember/proto"
	abci "github.com/cometbft/cometbft/abci/types"
	"google.golang.org/protobuf/proto"
)

type consumerEvidenceRow struct {
	Request   string    `json:"request"`
	Receipt   string    `json:"receipt"`
	Scores    []float64 `json:"scores"`
	Output    string    `json:"output"`
	Delivered uint32    `json:"delivered_tokens"`
}

func consumerEvidence(t *testing.T) []consumerEvidenceRow {
	t.Helper()
	path := os.Getenv("FAIRNESS_GRAY_FIXTURE")
	if path == "" {
		t.Skip("set FAIRNESS_GRAY_FIXTURE to ops/fairness_gray_probe.py output")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Rows []consumerEvidenceRow `json:"rows"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Rows) < 21 {
		t.Fatal("need at least 21 unique executions")
	}
	seen := map[string]bool{}
	for _, r := range f.Rows {
		if seen[r.Request] || r.Output == "" || r.Delivered != 4 || len(r.Scores) != 2 {
			t.Fatal("invalid execution evidence")
		}
		seen[r.Request] = true
	}
	return f.Rows
}

type consumerChain struct {
	t      *testing.T
	app    *Application
	a      map[string]actor
	height int64
}

func newConsumerChain(t *testing.T, comp bool) *consumerChain {
	t.Helper()
	s, a := pricingState(t)
	s.Height, s.BlockHashes = 0, nil
	s.Accounts[a["node"].id].Stake = 5000
	s.Parameters.Misjudge.Thresholds = []ThresholdPair{misjudgeFixture(t)}
	for _, n := range s.Nodes {
		n.ExpiresHeight = 10000
	}
	if comp {
		m := s.Models["ember-test-model"]
		m.AuditComp = &AuditCompParameters{FormulaVer: 2, OFixed: 1, MarginBPS: 10000, AuditShareBPS: 1500, BenchmarkRef: fmt.Sprintf("%064d", 1)}
		s.Models[m.ID] = m
	}
	return &consumerChain{t: t, app: &Application{committed: s}, a: a}
}

func (c *consumerChain) state() *State {
	c.t.Helper()
	s, err := c.app.State()
	if err != nil {
		c.t.Fatal(err)
	}
	return s
}

func (c *consumerChain) block(who string, action Action) {
	c.t.Helper()
	c.height++
	var txs [][]byte
	if who != "" {
		s := c.state()
		a := c.a[who]
		tx, err := Sign(s.Parameters.ChainID, a.key, s.Accounts[a.id].Nonce+1, action)
		if err != nil {
			c.t.Fatal(err)
		}
		txs = [][]byte{tx}
	}
	hash := sha256.Sum256([]byte(fmt.Sprintf("consumer-gray-%d", c.height)))
	r, err := c.app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: c.height, Hash: hash[:], Txs: txs})
	if err != nil {
		c.t.Fatal(err)
	}
	for _, tx := range r.TxResults {
		if tx.Code != 0 {
			c.t.Fatalf("height=%d action=%s: %s", c.height, action.Type, tx.Log)
		}
	}
	if _, err := c.app.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
		c.t.Fatal(err)
	}
}

func (c *consumerChain) request(row consumerEvidenceRow, gray, charge bool, scope string) {
	c.t.Helper()
	before := c.state()
	payer, node := c.a["payer"].id, c.a["node"].id
	receipt, err := hex.DecodeString(row.Receipt)
	if err != nil {
		c.t.Fatal(err)
	}
	c.block("payer", Action{Type: "freeze", HoldID: row.Request, Payer: payer, Node: node, Amount: 40})
	snap := c.state().Holds[row.Request].ConsumerGray
	if snap == nil || snap.Charge != charge || snap.Scope != scope {
		c.t.Fatalf("snapshot %+v, want charge=%v scope=%s", snap, charge, scope)
	}
	c.block("coordinator", Action{Type: "submit", HoldID: row.Request, Actual: 20, Receipt: receipt})
	c.block("", Action{})
	c.block("", Action{})
	c.block("coordinator", Action{Type: "assign_audit", HoldID: row.Request})
	outcome, rounds := "accept", 1
	if gray {
		outcome, rounds = "reject", 2
	}
	for round := 0; round < rounds; round++ {
		s := c.state()
		auditor, proof := assignedActor(c.t, s, c.a, row.Request)
		challenge, raw, _ := signedAudit(c.t, receipt, auditor, outcome, proof)
		v := &pb.AuditVerdict{}
		if err := proto.Unmarshal(raw, v); err != nil {
			c.t.Fatal(err)
		}
		v.NOutBilled = 1
		if gray {
			v.Offense, v.Distances = numericGrade(row.Scores[round]), []float64{row.Scores[round]}
		}
		if err := core.Sign(v, auditor.key); err != nil {
			c.t.Fatal(err)
		}
		raw, err = proto.Marshal(v)
		if err != nil {
			c.t.Fatal(err)
		}
		who := ""
		for name, a := range c.a {
			if a.id == auditor.id {
				who = name
			}
		}
		c.block(who, Action{Type: "audit", HoldID: row.Request, Challenge: challenge, Verdict: raw})
	}
	h := c.state().Holds[row.Request]
	payment := outcome
	if gray && charge {
		payment = "accept"
	}
	receipts, err := c.state().ExpectedSettlementAuditReceipts(h, outcome, c.height+1)
	normal, normalErr := ExpectedAuditReceipts(h, payment)
	if normalErr != nil || !reflect.DeepEqual(receipts, normal) {
		c.t.Fatal("tooling receipts differ from normal payment", normalErr)
	}
	if err != nil {
		c.t.Fatal(err)
	}
	if gray && charge && h.AuditCompPolicy != nil {
		// A zero-budget reject receipt must fail atomically before the paid retry.
		bad, err := ExpectedAuditReceipts(h, "reject")
		if err != nil {
			c.t.Fatal(err)
		}
		s := c.state()
		old, _ := s.Bytes()
		rejected(c.t, s, c.a["coordinator"], Action{Type: "settle", HoldID: row.Request, Outcome: outcome, AuditReceipts: bad}, c.height+1)
		after, _ := s.Bytes()
		if !equal(old, after) {
			c.t.Fatal("invalid receipts changed state")
		}
	}
	c.block("coordinator", Action{Type: "settle", HoldID: row.Request, Outcome: outcome, AuditReceipts: receipts})
	after := c.state()
	h = after.Holds[row.Request]
	spent := before.Accounts[payer].Balance - after.Accounts[payer].Balance
	earned := after.Accounts[node].Balance - before.Accounts[node].Balance
	var auditPaid int64
	for id, a := range after.Accounts {
		if id != payer && id != node {
			auditPaid += a.Balance - before.Accounts[id].Balance
		}
	}
	if len(h.Slashes) != 0 || after.Accounts[node].Stake != before.Accounts[node].Stake {
		c.t.Fatal("unexpected slash")
	}
	if gray {
		r := after.OffenseRecords[len(after.OffenseRecords)-1]
		if r.HoldID != h.ID || (r.Status != "gray" && r.Status != "pending-review") || h.AuditVerdict != "reject" {
			c.t.Fatal("lost gray rejection", r)
		}
	}
	if gray && !charge {
		if spent != 0 || earned != 0 || auditPaid != 0 || h.Shares != nil {
			c.t.Fatal("honest gray was charged")
		}
	} else {
		if h.State != "settled" || h.Shares == nil || h.Shares.Executor != 16 || h.Shares.Auditor != 3 || h.Shares.Consumer != 1 || h.Shares.Executor+h.Shares.Auditor+h.Shares.Consumer != h.Actual || spent != h.Actual-h.Shares.Consumer || earned != h.Shares.Executor || auditPaid+h.AuditEscrow != h.Shares.Auditor {
			c.t.Fatalf("bad shares: %+v spent=%d executor=%d audit=%d", h.Shares, spent, earned, auditPaid)
		}
		if h.AuditCompPolicy == nil && auditPaid != 3 {
			c.t.Fatal("auditor not paid")
		}
		if h.AuditCompPolicy != nil && (auditPaid != int64(rounds) || h.AuditEscrow != 3-int64(rounds)) {
			c.t.Fatal("compensation/escrow differs from accept")
		}
	}
	sample := after.ConsumerSamples[h.ID]
	if sample.HoldID != h.ID || sample.Consumer != payer || sample.Node != node || sample.Height != c.height || sample.Gray != gray {
		c.t.Fatal("sample mismatch", sample)
	}
	old, _ := after.Bytes()
	rejected(c.t, after, c.a["coordinator"], Action{Type: "settle", HoldID: h.ID, Outcome: outcome, AuditReceipts: receipts}, c.height+1)
	unchanged, _ := after.Bytes()
	if !equal(old, unchanged) {
		c.t.Fatal("duplicate settlement mutated state")
	}
	c.t.Logf("request=%s height=%d gray=%v charge=%v scope=%s frozen=40 fee=20 returned=%d net_spend=%d executor=%d audit_paid=%d escrow=%d rebate=%d state=%s", h.ID, c.height, gray, charge, scope, 40-spent, spent, earned, auditPaid, h.AuditEscrow, func() int64 {
		if h.Shares != nil {
			return h.Shares.Consumer
		}
		return 0
	}(), h.State)
}

func TestConsumerGrayABCI(t *testing.T) {
	rows := consumerEvidence(t)
	for _, comp := range []bool{false, true} {
		t.Run(fmt.Sprintf("comp=%v", comp), func(t *testing.T) {
			c := newConsumerChain(t, comp)
			for i, row := range rows {
				scope := ""
				if i >= 20 {
					scope = "global"
				} else if i >= 10 {
					scope = "pair"
				}
				c.request(row, true, i >= 10, scope)
			}
			if len(c.state().ConsumerSamples) != len(rows) || c.state().GrayObservations[c.a["node"].id] != uint64(len(rows)) {
				t.Fatal("counts changed")
			}
		})
	}
}

func TestConsumerGrayHonestControlsABCI(t *testing.T) {
	rows := consumerEvidence(t)
	t.Run("insufficient-samples", func(t *testing.T) {
		c := newConsumerChain(t, false)
		for _, row := range rows[:9] {
			c.request(row, true, false, "")
		}
	})
	t.Run("low-gray-rate", func(t *testing.T) {
		c := newConsumerChain(t, false)
		for i, row := range rows {
			c.request(row, i < 4 || i >= 20, false, "")
		}
	})
	t.Run("low-frequency", func(t *testing.T) {
		c := newConsumerChain(t, false)
		for _, row := range rows[:12] {
			c.height += 1000
			// Keep registrations live; the skipped heights are local empty history.
			for _, n := range c.app.committed.Nodes {
				n.ExpiresHeight = 100000
			}
			c.request(row, true, false, "")
		}
	})
}

func TestConsumerGrayGovernanceSnapshot(t *testing.T) {
	s, a := setupMisjudge(t)
	freeze := func(id string, height int64) {
		apply(t, s, a["payer"], s.Accounts[a["payer"].id].Nonce+1, Action{Type: "freeze", HoldID: id, Payer: a["payer"].id, Node: a["node"].id, Amount: 10}, height)
	}
	freeze("before", 1)
	p := s.Parameters.Misjudge
	p.ConsumerGray.GlobalMinSamples, p.ConsumerGray.PairMinSamples = 2, 100
	apply(t, s, a["governor"], 1, Action{Type: "set_misjudge", Misjudge: &p}, 2)
	gradedRequest(t, s, a, "sample1", 3, numericGrade(.45))
	gradedRequest(t, s, a, "sample2", 9, numericGrade(.45))
	freeze("global", 15)
	h := s.Holds["global"]
	if !h.ConsumerGray.Charge || h.ConsumerGray.Scope != "global" || h.ConsumerGray.Parameters != p.ConsumerGray {
		t.Fatal(h.ConsumerGray)
	}
	// A consumer's global history applies to a different target as well.
	other := &Hold{ID: "other-target", Payer: a["payer"].id, Node: "different-executor"}
	s.freezeConsumerGray(other, 15)
	if !other.ConsumerGray.Charge || other.ConsumerGray.Scope != "global" {
		t.Fatal("global did not cover another executor")
	}
	// Settle the uncharged snapshot while the current account is globally triggered.
	// Then remove history and raise thresholds before settling the charged snapshot.
	for _, id := range []string{"before", "global"} {
		if id == "global" {
			p.ConsumerGray.GlobalMinSamples = 100
			apply(t, s, a["governor"], 2, Action{Type: "set_misjudge", Misjudge: &p}, 16)
			s.ConsumerSamples = nil
		}
		h = s.Holds[id]
		h.Actual, h.Auditor, h.AuditOutputTokens = 4, a["auditor"].id, 1
		v, _ := proto.Marshal(&pb.AuditVerdict{Offense: numericGrade(.45)})
		h.VerdictBytes = v
		settleHeight := int64(15)
		if id == "global" {
			settleHeight = 17
		}
		if err := s.settleAt(h, "reject", settleHeight); err != nil {
			t.Fatal(err)
		}
		if (h.State == "settled") != (id == "global") {
			t.Fatal("snapshot recomputed", id, h.State)
		}
		t.Logf("hold=%s frozen_charge=%v scope=%s terminal=%s payer=%d", id, h.ConsumerGray.Charge, h.ConsumerGray.Scope, h.State, s.Accounts[a["payer"].id].Balance)
	}
	clone, err := s.Clone()
	if err != nil {
		t.Fatal(err)
	}
	clone.Holds["global"].ConsumerGray.Parameters.WindowBlocks++
	if reflect.DeepEqual(clone.Holds["global"].ConsumerGray, s.Holds["global"].ConsumerGray) {
		t.Fatal("snapshot aliases clone")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(path, s); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := s.Bytes()
	restored, _ := reloaded.Bytes()
	if !equal(raw, restored) {
		t.Fatal("persistence lost samples or snapshots")
	}
}

func TestConsumerGrayWindowAndThresholds(t *testing.T) {
	p := ConsumerGrayParameters{}
	p.defaults()
	if p != (ConsumerGrayParameters{1000, 20, 5000, 10, 5, 5000}) {
		t.Fatal("defaults", p)
	}
	for _, change := range []func(*ConsumerGrayParameters){func(p *ConsumerGrayParameters) { p.WindowBlocks = -1 }, func(p *ConsumerGrayParameters) { p.GlobalMinSamples = 0 }, func(p *ConsumerGrayParameters) { p.GlobalGrayRateBPS = 10001 }, func(p *ConsumerGrayParameters) { p.PairMinSamples = 0 }, func(p *ConsumerGrayParameters) { p.PairMinGray = 0 }, func(p *ConsumerGrayParameters) { p.PairGrayRateBPS = 10001 }} {
		bad := p
		change(&bad)
		if bad.valid() {
			t.Fatal("invalid parameters accepted", bad)
		}
	}
	for _, tc := range []struct {
		total, gray uint64
		bps         uint32
		want        bool
	}{{20, 10, 5000, true}, {20, 9, 5000, false}, {10, 5, 5000, true}, {11, 5, 5000, false}, {math.MaxUint64, math.MaxUint64, 10000, true}, {math.MaxUint64, math.MaxUint64 / 2, 5000, false}} {
		if grayRateAtLeast(tc.gray, tc.total, tc.bps) != tc.want {
			t.Fatal("rate boundary", tc)
		}
	}
	s, a := setupMisjudge(t)
	s.Parameters.Misjudge.ConsumerGray = ConsumerGrayParameters{10, 2, 5000, 2, 1, 5000}
	s.ConsumerSamples = map[string]ConsumerSample{"gray": {Consumer: a["payer"].id, Node: a["node"].id, Height: 10, Gray: true, HoldID: "gray"}, "accept": {Consumer: a["payer"].id, Node: a["node"].id, Height: 10, HoldID: "accept"}}
	for _, height := range []int64{9, 19, 20} {
		h := &Hold{Payer: a["payer"].id, Node: a["node"].id}
		s.freezeConsumerGray(h, height)
		if h.ConsumerGray.Charge != (height == 19) {
			t.Fatal("age must be nonnegative and strictly less than window", height, h.ConsumerGray)
		}
	}
	// Pair requires both the minimum gray count and rate; other consumers cannot dilute it.
	s.Parameters.Misjudge.ConsumerGray = p
	s.ConsumerSamples = map[string]ConsumerSample{}
	for i := 0; i < 10; i++ {
		id := fmt.Sprint(i)
		s.ConsumerSamples[id] = ConsumerSample{Consumer: a["payer"].id, Node: a["node"].id, Height: 1, Gray: i < 5, HoldID: id}
	}
	h := &Hold{Payer: a["payer"].id, Node: a["node"].id}
	s.freezeConsumerGray(h, 2)
	if !h.ConsumerGray.Charge || h.ConsumerGray.Scope != "pair" {
		t.Fatal("pair equality boundary", h.ConsumerGray)
	}
	h.Node = "other"
	s.freezeConsumerGray(h, 2)
	if h.ConsumerGray.Charge {
		t.Fatal("pair leaked to another executor")
	}
	v := s.ConsumerSamples["0"]
	v.Gray = false
	s.ConsumerSamples["0"] = v
	h.Node = a["node"].id
	s.freezeConsumerGray(h, 2)
	if h.ConsumerGray.Charge {
		t.Fatal("pair below threshold charged")
	}
}

func TestConsumerGraySampleEligibility(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		score      float64
		fault      bool
		wantGray   bool
	}{
		{"gray", "gray_gate", .45, false, true}, {"below-soft", "gray_gate", .1, false, false}, {"hard", "gray_gate", .6, false, false}, {"objective", "gray_gate", 0, true, false}, {"full", "full", .45, false, false}, {"refund-only", "refund_only", .45, false, false}, {"off", "off", .45, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, a := setupMisjudge(t)
			s.Parameters.Misjudge.SlashMode = tc.mode
			g := numericGrade(tc.score)
			if tc.fault {
				g = &pb.OffenseGrade{Class: "objective", Fault: "hash"}
			}
			h := gradedRequest(t, s, a, "eligible", 1, g)
			if len(s.ConsumerSamples) != 1 || s.ConsumerSamples[h.ID].Gray != tc.wantGray || s.ConsumerSamples[h.ID].Height != 6 {
				t.Fatal("wrong sample", s.ConsumerSamples)
			}
			s.recordConsumerSample(h, "reject", 7)
			if s.ConsumerSamples[h.ID].Height != 6 {
				t.Fatal("sample counted twice")
			}
		})
	}
	for _, outcome := range []string{"service_failure", "expire_hold", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			s, a := setupMisjudge(t)
			apply(t, s, a["payer"], 1, Action{Type: "freeze", HoldID: outcome, Payer: a["payer"].id, Node: a["node"].id, Amount: 5}, 1)
			action, height := Action{Type: outcome, HoldID: outcome}, int64(2)
			if outcome == "expire_hold" {
				height = 21
			}
			if outcome == "service_failure" {
				s.Holds[outcome].State, s.Holds[outcome].AuditVerdict = "dispute", "service_failure"
				action.Type, action.Outcome = "settle", outcome
			}
			apply(t, s, a["coordinator"], 1, action, height)
			if len(s.ConsumerSamples) != 0 || s.Accounts[a["payer"].id].Balance != 1000 {
				t.Fatal("excluded terminal request counted or charged")
			}
		})
	}
	s, a := setupMisjudge(t)
	s.recordConsumerSample(&Hold{ID: "pipeline", Pipeline: &PipelineHold{}}, "accept", 1)
	if len(s.ConsumerSamples) != 0 {
		t.Fatal("pipeline counted")
	}
	// A persisted legacy hold has no snapshot and retains the full gray refund.
	apply(t, s, a["payer"], 1, Action{Type: "freeze", HoldID: "legacy", Payer: a["payer"].id, Node: a["node"].id, Amount: 5}, 1)
	h := s.Holds["legacy"]
	h.ConsumerGray, h.State, h.AuditVerdict = nil, "dispute", "reject"
	h.Actual, h.AuditOutputTokens = 1, 1
	h.VerdictBytes, _ = proto.Marshal(&pb.AuditVerdict{Offense: numericGrade(.45)})
	path := filepath.Join(t.TempDir(), "legacy.json")
	if err := SaveState(path, s); err != nil {
		t.Fatal(err)
	}
	s, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	apply(t, s, a["coordinator"], 1, Action{Type: "settle", HoldID: "legacy", Outcome: "reject"}, 2)
	if s.Accounts[a["payer"].id].Balance != 1000 || s.Holds["legacy"].State == "settled" || s.Holds["legacy"].ConsumerGray != nil {
		t.Fatal("legacy hold charged")
	}
}

func TestConsumerGrayChargeDoesNotOverrideNonGray(t *testing.T) {
	for _, tc := range []struct {
		mode  string
		grade *pb.OffenseGrade
		slash bool
	}{
		{"gray_gate", numericGrade(.1), false},
		{"gray_gate", numericGrade(.6), true},
		{"gray_gate", &pb.OffenseGrade{Class: "objective", Fault: "hash"}, true},
		{"refund_only", numericGrade(.45), false},
		{"off", numericGrade(.45), false},
		{"full", numericGrade(.45), true},
	} {
		s, a := setupMisjudge(t)
		s.Parameters.Misjudge.SlashMode = tc.mode
		s.ConsumerSamples = map[string]ConsumerSample{}
		for i := 0; i < 10; i++ {
			id := fmt.Sprint(i)
			s.ConsumerSamples[id] = ConsumerSample{Consumer: a["payer"].id, Node: a["node"].id, Height: 0, Gray: true, HoldID: id}
		}
		h := gradedRequest(t, s, a, "non-gray", 1, tc.grade)
		if !h.ConsumerGray.Charge || h.Shares != nil || h.State == "settled" || s.ConsumerSamples[h.ID].Gray || (len(h.Slashes) > 0) != tc.slash {
			t.Fatalf("charged snapshot changed non-gray treatment: mode=%s hold=%+v", tc.mode, h)
		}
	}
}
