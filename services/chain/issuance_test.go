package chain

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "ember/core-go"
	pb "ember/proto"
	abci "github.com/cometbft/cometbft/abci/types"
	"google.golang.org/protobuf/proto"
)

func issuanceGenesis(t *testing.T) (GenesisState, map[string]actor) {
	t.Helper()
	base, a := testState(t)
	governors := []string{}
	for n := 0; n < 5; n++ {
		name := fmt.Sprint("g", n)
		a[name] = newActor(byte(20 + n))
		governors = append(governors, a[name].id)
	}
	a["developer"] = newActor(30)
	a["expire"] = newActor(31)
	a["enduser"] = newActor(32)
	p := &IssuanceParameters{StandardPrice: 1000000, PriceRef: "published-reference-512-256", OrganicForecast: 2 * PoolEquivalents, ForecastRef: "published-conservative-forecast", GenesisTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix(), GovernorThreshold: 3, GovernorSigners: governors, FoundingEntities: []string{"founder"}, Entities: map[string]string{}}
	g := GenesisState{Parameters: base.Parameters, Coordinators: []string{a["coordinator"].id}, Auditors: []string{a["auditor"].id, a["auditor2"].id}, Governors: governors}
	g.Parameters.Misjudge.SlashMode = "refund_only"
	g.Parameters.RegisteredAuditors = true
	g.Parameters.GenesisIssuance = p
	g.Parameters.SubsidyCap = 0
	for name, v := range a {
		balance := int64(0)
		stake := int64(0)
		if name == "payer" {
			balance = 1000000
		}
		if name == "auditor" || name == "auditor2" {
			stake = 10000
		}
		if name == "node" {
			stake = 10000000000
		}
		g.Accounts = append(g.Accounts, GenesisAccount{ID: v.id, Balance: balance, Stake: stake})
		p.Entities[v.id] = name
	}
	// 170 settled requests: supply=1020 points, capped to 1000;
	// audit=510, consumer=170. A self-dealing request excludes every role.
	for n := 0; n < 170; n++ {
		p.Retrospective = append(p.Retrospective, ContributionRecord{RequestID: fmt.Sprint(n), Payer: a["payer"].id, Executor: a["node"].id, Auditor: a["auditor"].id, Settled: true, Audited: true})
	}
	p.Entities[a["expire"].id] = "node"
	p.Retrospective = append(p.Retrospective, ContributionRecord{RequestID: "self", Payer: a["expire"].id, Executor: a["node"].id, Auditor: a["auditor2"].id, Settled: true, Audited: true}, ContributionRecord{RequestID: "unsettled", Payer: a["enduser"].id, Executor: a["node"].id})
	g.Models = []Model{{ID: "ember-test-model", GenesisHash: strings.Repeat("1", 64), ProfileHash: strings.Repeat("2", 64), Pricing: &RolePricing{TargetExecutors: 1, TargetAuditors: 1, ExecutorWeight: 80, AuditorWeight: 15, ConsumerWeight: 5}}}
	return g, a
}

type issuanceChain struct {
	t      *testing.T
	app    *Application
	now    int64
	height int64
	path   string
}

func newIssuanceChain(t *testing.T, g GenesisState) *issuanceChain {
	t.Helper()
	dir := t.TempDir()
	if root := os.Getenv("GENESIS_REHEARSAL_DIR"); root != "" {
		dir = root
	}
	path := filepath.Join(dir, "data", "ember-state.json")
	app, err := OpenApplication(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(g)
	if err = os.WriteFile(filepath.Join(dir, "genesis.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = app.InitChain(context.Background(), &abci.InitChainRequest{ChainId: g.Parameters.ChainID, Time: time.Unix(g.Parameters.GenesisIssuance.GenesisTime, 0), AppStateBytes: raw})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fresh ABCI chain initialized from zero: %s; initial=%d; slash_mode=refund_only", path, PoolEquivalents*g.Parameters.GenesisIssuance.StandardPrice)
	return &issuanceChain{t: t, app: app, now: g.Parameters.GenesisIssuance.GenesisTime, path: path}
}
func (c *issuanceChain) state() *State {
	c.t.Helper()
	s, e := c.app.State()
	if e != nil {
		c.t.Fatal(e)
	}
	return s
}
func (c *issuanceChain) block(txs ...[]byte) *abci.FinalizeBlockResponse {
	c.t.Helper()
	c.height++
	hash := sha256.Sum256([]byte(fmt.Sprint(c.height)))
	r, e := c.app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: c.height, Time: time.Unix(c.now, 0), Hash: hash[:], Txs: txs})
	if e != nil {
		c.t.Fatal(e)
	}
	if _, e = c.app.Commit(context.Background(), &abci.CommitRequest{}); e != nil {
		c.t.Fatal(e)
	}
	c.reconcile()
	return r
}
func (c *issuanceChain) tx(who actor, a Action, want bool) {
	c.t.Helper()
	s := c.state()
	nonce := uint64(1)
	if s.Accounts[who.id] != nil {
		nonce = s.Accounts[who.id].Nonce + 1
	}
	raw, e := Sign(s.Parameters.ChainID, who.key, nonce, a)
	if e != nil {
		c.t.Fatal(e)
	}
	// Valid transactions must also survive both proposal paths at block time.
	if want {
		prep, e := c.app.PrepareProposal(context.Background(), &abci.PrepareProposalRequest{Height: c.height + 1, Time: time.Unix(c.now, 0), MaxTxBytes: 1 << 20, Txs: [][]byte{raw}})
		if e != nil || len(prep.Txs) != 1 {
			c.t.Fatalf("prepare %v %+v", e, prep)
		}
		prop, e := c.app.ProcessProposal(context.Background(), &abci.ProcessProposalRequest{Height: c.height + 1, Time: time.Unix(c.now, 0), Txs: prep.Txs})
		if e != nil || prop.Status != abci.PROCESS_PROPOSAL_STATUS_ACCEPT {
			c.t.Fatalf("process %v %+v", e, prop)
		}
	}
	before := s.Accounts[who.id]
	r := c.block(raw)
	ok := r.TxResults[0].Code == 0
	if ok != want {
		c.t.Fatalf("%s/%+v: success=%v want=%v log=%s", a.Type, a.Subsidy, ok, want, r.TxResults[0].Log)
	}
	if !ok {
		after := c.state().Accounts[who.id]
		if before != nil && after.Nonce != before.Nonce {
			c.t.Fatal("rejected transaction consumed nonce")
		}
		c.t.Logf("rejected %s/%+v: %s", a.Type, a.Subsidy, r.TxResults[0].Log)
	}
}
func (c *issuanceChain) sub(who actor, a SubsidyAction, want bool) {
	c.t.Helper()
	c.tx(who, Action{Type: "subsidy", Subsidy: &a}, want)
}
func (c *issuanceChain) reconcile() {
	c.t.Helper()
	s := c.state()
	i := s.Issuance
	var issued, recycled, remaining int64
	for _, g := range i.Grants {
		if g.ID == "" || g.Batch == "" || g.Channel == "" {
			c.t.Fatal("untraceable grant")
		}
		if g.Parent == "" {
			issued += g.Amount
		}
		recycled += g.Recycled
	}
	for channel, v := range i.Remaining {
		remaining += v
		expected := s.Parameters.GenesisIssuance.Rules.ChannelEquivalents[channel] * s.Parameters.GenesisIssuance.StandardPrice
		for _, g := range i.Grants {
			if g.Channel == channel {
				if g.Parent == "" {
					expected -= g.Amount
				}
				expected += g.Recycled
			}
		}
		if v != expected {
			c.t.Fatal("channel reconciliation mismatch", channel, v, expected)
		}
	}
	for day, d := range i.Days {
		var total int64
		for _, g := range i.Grants {
			if g.Parent == "" && g.IssuedAt/86400 == day {
				total += g.Amount
			}
		}
		if d.Total != total {
			c.t.Fatal("daily issuance ledger mismatch")
		}
	}
	months := s.subsidyMonthly()
	if len(months) == 0 || months[len(months)-1].Closing != s.Accounts[PoolAccount].Balance {
		c.t.Fatal("monthly summary mismatch")
	}
	if issued != i.Issued || recycled != i.Recycled || remaining != i.Initial-i.Issued+i.Recycled || s.Accounts[PoolAccount].Balance != remaining {
		c.t.Fatalf("pool reconciliation failed: %+v", i)
	}
}
func (c *issuanceChain) register(node actor) {
	c.t.Helper()
	p := &pb.ResourceProfile{NodeId: node.id, ChainId: "ember-test-model", ManifestRoot: make([]byte, 32), ExecutionProfileVer: "v1", ActualDevice: "cpu", TorchVersion: "2.5.1", EngineCapabilities: &pb.EngineCapabilities{EngineName: "mock", EngineVersion: "1", Roles: []string{"model", "auditor"}, ForcedPrefixReplay: true, ScoreKind: "raw-logits", MaxTopK: 16, EvidenceLevels: []string{"full"}}}
	if e := core.Sign(p, node.key); e != nil {
		c.t.Fatal(e)
	}
	raw, _ := proto.Marshal(p)
	c.tx(node, Action{Type: "register_node", Profile: raw, ExpiresHeight: c.height + 90}, true)
}
func (c *issuanceChain) inference(a map[string]actor, payer actor, id string, input uint32) {
	c.t.Helper()
	node := a["node"]
	co := a["coordinator"]
	c.tx(payer, Action{Type: "freeze", HoldID: id, Payer: payer.id, Node: node.id, Amount: 10000}, true)
	raw := signedReceipt(c.t, id, node)
	r := &pb.ExecutionReceipt{}
	_ = proto.Unmarshal(raw, r)
	r.InputTokens = input
	if e := core.Sign(r, node.key); e != nil {
		c.t.Fatal(e)
	}
	raw, _ = proto.Marshal(r)
	c.tx(co, Action{Type: "submit", HoldID: id, Actual: 1000, Receipt: raw}, true)
	// MBlocks future entropy arrives on empty blocks.
	for n := 0; n < 4; n++ {
		c.block()
	}
	c.tx(co, Action{Type: "assign_audit", HoldID: id}, true)
	auditor, proof := assignedActor(c.t, c.state(), a, id)
	challenge, verdict, _ := signedAudit(c.t, raw, auditor, "accept", proof)
	c.tx(auditor, Action{Type: "audit", HoldID: id, Challenge: challenge, Verdict: verdict}, true)
	c.tx(co, Action{Type: "settle", HoldID: id, Outcome: "accept"}, true)
}

func TestGenesisRehearsal(t *testing.T) {
	g, a := issuanceGenesis(t)
	g.Parameters.ChainID = "ember-genesis-local-rehearsal"
	// Additional published records supply B daily-cap probes.
	for n := 0; n < 5; n++ {
		name := fmt.Sprint("b", n)
		a[name] = newActor(byte(50 + n))
		g.Parameters.GenesisIssuance.Entities[a[name].id] = name
	}
	for n := 0; n < 3; n++ {
		for k := 0; k < 170; k++ {
			g.Parameters.GenesisIssuance.Retrospective = append(g.Parameters.GenesisIssuance.Retrospective, ContributionRecord{RequestID: fmt.Sprintf("boundary-%d-%d", n, k), Payer: a["b4"].id, Executor: a[fmt.Sprint("b", n)].id, Settled: true})
		}
	}
	for n := 0; n < 285; n++ {
		g.Parameters.GenesisIssuance.Retrospective = append(g.Parameters.GenesisIssuance.Retrospective, ContributionRecord{RequestID: fmt.Sprint("boundary-consume-", n), Payer: a["b3"].id, Executor: a["node"].id, Settled: true})
	}
	c := newIssuanceChain(t, g)
	p := g.Parameters.GenesisIssuance
	// Disclosure must be part of the signed API request; no grant without it.
	newcomer := newActor(40)
	c.sub(newcomer, SubsidyAction{Op: "claim", ID: "missing-confirm", Batch: "A-0"}, false)
	c.sub(newcomer, SubsidyAction{Op: "claim", Confirm: true, ID: "A-active", Batch: "A-1"}, true)
	if c.state().Accounts[newcomer.id].Marked != 0 {
		t.Fatal("unactivated voucher spendable")
	}
	c.tx(newcomer, Action{Type: "transfer", To: a["node"].id, Amount: 1}, false)
	c.tx(newcomer, Action{Type: "freeze", HoldID: "unactivated", Payer: newcomer.id, Node: a["node"].id, Amount: 1}, false)
	// First inference is paid with clean Spark, as activation precedes spending.
	c.tx(a["payer"], Action{Type: "transfer", To: newcomer.id, Amount: 3000}, true)
	c.tx(a["payer"], Action{Type: "transfer", To: PoolAccount, Amount: 1}, false)
	c.register(a["node"])
	c.register(a["auditor"])
	c.register(a["auditor2"])
	// Deposit requires 10000; fund it with clean funds for the qualifying request.
	// Supply-account work rewards here are clean, independently of pool grants.
	c.tx(a["payer"], Action{Type: "transfer", To: newcomer.id, Amount: 17000}, true)
	c.inference(a, newcomer, "short", 127)
	if c.state().Accounts[newcomer.id].Marked != 0 {
		t.Fatal("short input activated")
	}
	c.inference(a, newcomer, "qualify", 128)
	if c.state().Accounts[newcomer.id].Marked != 100*p.StandardPrice {
		t.Fatal("qualifying settlement did not activate")
	}
	t.Log("PASS A claim -> 127 tokens rejected for activation -> settled 128-token inference activates")
	// Empty clean wallet, then show every non-inference debit fails.
	clean := c.state().Accounts[newcomer.id].Balance
	c.tx(newcomer, Action{Type: "transfer", To: a["payer"].id, Amount: clean}, true)
	c.tx(newcomer, Action{Type: "transfer", To: a["payer"].id, Amount: 1}, false)
	c.tx(newcomer, Action{Type: "stake", Amount: 1}, false)
	c.inference(a, newcomer, "marked-spend", 128)
	h := c.state().LookupHold("marked-spend")
	if h.Marked == 0 || h.Shares == nil || h.Shares.Executor <= 0 || h.Shares.Auditor <= 0 {
		t.Fatal("marked settlement missing split")
	}
	auditor := actor{}
	for _, v := range a {
		if v.id == h.Auditor {
			auditor = v
		}
	}
	for _, v := range []actor{a["node"], auditor} {
		if c.state().Accounts[v.id].Marked != 0 {
			t.Fatal("mark propagated")
		}
		c.tx(v, Action{Type: "transfer", To: a["payer"].id, Amount: 1}, true)
	}
	t.Log("PASS permanent marked transfer/stake rejection; inference executor and auditor receive clean transferable Spark; payer rebate retains mark")
	c.sub(a["expire"], SubsidyAction{Op: "claim", Confirm: true, ID: "A-expire", Batch: "A-2"}, true)
	before := c.state().Accounts[PoolAccount].Balance
	c.now += subsidyWeek
	c.block()
	if c.state().Accounts[PoolAccount].Balance != before+100*p.StandardPrice {
		t.Fatal("expiry did not recycle exactly")
	}
	c.sub(a["expire"], SubsidyAction{Op: "claim", Confirm: true, ID: "replay-expiry", Batch: "A-2"}, false)
	t.Log("PASS automatic empty-block expiry -> original A pool; expired lifetime claim cannot repeat")
	// Recompute each published award from records, including an identity alias.
	awards, e := RecomputeRetrospective(p)
	if e != nil {
		t.Fatal(e)
	}
	expected := map[string][4]int64{a["node"].id: {455, 0, 0, 1000}, a["auditor"].id: {0, 170, 0, 510}, a["payer"].id: {0, 0, 170, 170}}
	for n := 0; n < 3; n++ {
		expected[a[fmt.Sprint("b", n)].id] = [4]int64{170, 0, 0, 1000}
	}
	expected[a["b3"].id] = [4]int64{0, 0, 285, 285}
	expected[a["b4"].id] = [4]int64{0, 0, 510, 510}
	if len(awards) != len(expected) {
		t.Fatal("self-dealing/unsettled records included")
	}
	for id, x := range expected {
		v := awards[id]
		if v.Supply != x[0] || v.Audit != x[1] || v.Consume != x[2] || v.Amount != x[3]*p.StandardPrice {
			t.Fatal("award mismatch", id, v)
		}
		if v != c.state().Issuance.Awards[id] {
			t.Fatal("genesis recomputation differs")
		}
		t.Logf("PASS B item %s: supply=%d audit=%d consume=%d score=%d award=%d", id, v.Supply, v.Audit, v.Consume, v.Score, v.Amount)
	}
	c.sub(a["node"], SubsidyAction{Op: "claim", Confirm: true, ID: "B-node", Batch: "B-1"}, true)
	c.sub(a["node"], SubsidyAction{Op: "claim", Confirm: true, ID: "A-after-B", Batch: "A-3"}, false)
	for n := 0; n < 3; n++ {
		c.sub(a[fmt.Sprint("b", n)], SubsidyAction{Op: "claim", Confirm: true, ID: fmt.Sprint("B-rate-", n), Batch: "B-rate"}, true)
	}
	c.sub(a["b3"], SubsidyAction{Op: "claim", Confirm: true, ID: "B-rate-consume", Batch: "B-rate"}, true)
	c.sub(a["auditor"], SubsidyAction{Op: "claim", Confirm: true, ID: "B-over-day", Batch: "B-rate"}, false)
	c.now = (c.now/86400+1)*86400 - 1
	c.block()
	c.sub(a["auditor"], SubsidyAction{Op: "claim", Confirm: true, ID: "B-last-second", Batch: "B-rate"}, false)
	c.now++
	c.sub(a["auditor"], SubsidyAction{Op: "claim", Confirm: true, ID: "B-next-day", Batch: "B-rate"}, true)
	t.Log("PASS B daily issuance 4285 equivalents within 4285.715 cap; next 510-equivalent grant rejected despite idle A/C budgets; UTC midnight reset")
	if c.state().Issuance.Claims[a["node"].id] != "B" {
		t.Fatal("did not take higher B")
	}
	c.sub(a["enduser"], SubsidyAction{Op: "claim", Confirm: true, ID: "A-low-B", Batch: "A-3"}, true)
	t.Log("PASS B 6:3:1, 10x cap, self-dealing excludes supply/audit/consume, A/B higher-only lifetime choice")
	c.sub(a["g0"], SubsidyAction{Op: "propose_c", ID: "C-over", To: a["developer"].id, Amount: scaled(c.state().Issuance.Initial, 25, 10000) + 1, Notice: "over cap"}, false)
	grant := scaled(c.state().Issuance.Initial, 15, 10000)
	c.sub(a["g0"], SubsidyAction{Op: "propose_c", ID: "C-good", To: a["developer"].id, Amount: grant, Notice: "Public application, product and expected users"}, true)
	for n := 0; n < 4; n++ {
		c.sub(a[fmt.Sprint("g", n)], SubsidyAction{Op: "approve_c", ID: "C-good"}, true)
	}
	c.sub(a["developer"], SubsidyAction{Op: "confirm", Confirm: true}, true)
	c.sub(a["g0"], SubsidyAction{Op: "issue_c", ID: "C-good", Batch: "C-1"}, false)
	c.now += subsidyWeek - 1
	c.block()
	c.sub(a["g0"], SubsidyAction{Op: "issue_c", ID: "C-good", Batch: "C-1"}, false)
	c.now++
	c.sub(a["g0"], SubsidyAction{Op: "issue_c", ID: "C-good", Batch: "C-1"}, true)
	c.sub(a["g0"], SubsidyAction{Op: "propose_c", ID: "C-no-borrow", To: a["developer"].id, Amount: 1, Notice: "No channel borrowing"}, true)
	for n := 0; n < 4; n++ {
		c.sub(a[fmt.Sprint("g", n)], SubsidyAction{Op: "approve_c", ID: "C-no-borrow"}, true)
	}
	// Existing C cap is exact; use an already mature second notice for boundary.
	c.now += subsidyWeek
	c.block()
	c.sub(a["g0"], SubsidyAction{Op: "issue_c", ID: "C-no-borrow", Batch: "C-2"}, true)
	// Fill C again to its exact sub-cap, without touching A/B counters.
	c.sub(a["g0"], SubsidyAction{Op: "propose_c", ID: "C-fill", To: a["developer"].id, Amount: grant - 1, Notice: "Fill daily cap"}, true)
	for n := 0; n < 4; n++ {
		c.sub(a[fmt.Sprint("g", n)], SubsidyAction{Op: "approve_c", ID: "C-fill"}, true)
	}
	c.now += subsidyWeek
	c.block()
	c.sub(a["g0"], SubsidyAction{Op: "issue_c", ID: "C-fill", Batch: "C-3"}, true)
	// A new proposal can mature while retaining a grant ready to fill the day.
	c.sub(a["g0"], SubsidyAction{Op: "propose_c", ID: "C-extra", To: a["developer"].id, Amount: 2, Notice: "Boundary probe"}, true)
	for n := 0; n < 4; n++ {
		c.sub(a[fmt.Sprint("g", n)], SubsidyAction{Op: "approve_c", ID: "C-extra"}, true)
	}
	c.sub(a["g0"], SubsidyAction{Op: "propose_c", ID: "C-full", To: a["developer"].id, Amount: grant, Notice: "Exact boundary"}, true)
	for n := 0; n < 4; n++ {
		c.sub(a[fmt.Sprint("g", n)], SubsidyAction{Op: "approve_c", ID: "C-full"}, true)
	}
	c.now += subsidyWeek
	c.block()
	c.sub(a["g0"], SubsidyAction{Op: "issue_c", ID: "C-full", Batch: "C-4"}, true)
	c.sub(a["g0"], SubsidyAction{Op: "issue_c", ID: "C-extra", Batch: "C-4"}, false)
	c.now = (c.now/86400+1)*86400 - 1
	c.block()
	c.sub(a["g0"], SubsidyAction{Op: "issue_c", ID: "C-extra", Batch: "C-4"}, false)
	c.now++
	c.sub(a["g0"], SubsidyAction{Op: "issue_c", ID: "C-extra", Batch: "C-5"}, true)
	t.Log("PASS C >0.25% rejected, 7-day notice checked to the second, 4-of-5 approval, exact C daily cap, cross-channel borrowing rejected and UTC midnight reset")
	// Developer distribution is inventory, not a transferable balance; end users
	// confirm independently and must complete their own qualifying inference.
	c.sub(a["developer"], SubsidyAction{Op: "distribute_c", ID: "C-good", To: newActor(41).id, Amount: 10, Batch: "C-users"}, false)
	end := newActor(41)
	c.sub(end, SubsidyAction{Op: "confirm", Confirm: true}, true)
	c.sub(a["developer"], SubsidyAction{Op: "distribute_c", ID: "C-good", To: end.id, Amount: 10, Batch: "C-users"}, true)
	if c.state().Accounts[end.id].Marked != 0 {
		t.Fatal("developer bypassed activation")
	}
	c.now += subsidyWeek
	c.block()
	t.Log("PASS C recipient disclosure, pending activation and expired child recycle; quarterly inventory reconciliation tested below")
	// 102 A grants fit; grant 103 is refused despite idle B and C budgets.
	c.now = (c.now/86400 + 1) * 86400
	c.block()
	for n := 0; n < 103; n++ {
		who := newActor(byte(80 + n))
		c.sub(who, SubsidyAction{Op: "claim", Confirm: true, ID: fmt.Sprint("A-rate-", n), Batch: "A-rate"}, n < 102)
	}
	c.now = (c.now/86400+1)*86400 - 1
	c.block()
	c.sub(newActor(182), SubsidyAction{Op: "claim", Confirm: true, ID: "A-last-second", Batch: "A-rate"}, false)
	c.now++
	c.sub(newActor(182), SubsidyAction{Op: "claim", Confirm: true, ID: "A-next-day", Batch: "A-rate"}, true)
	t.Log("PASS A daily cap accepts 102 fixed-size grants and rejects next grant; no B/C borrowing; UTC boundary resets")
	// The approved 0.25% maximum is funded across days, respecting C's
	// smaller daily cap. Distributions can span its traceable tranches.
	maximum := scaled(c.state().Issuance.Initial, 25, 10000)
	c.sub(a["g0"], SubsidyAction{Op: "propose_c", ID: "C-maximum", To: a["developer"].id, Amount: maximum, Notice: "Maximum grant funded across days"}, true)
	for n := 0; n < 4; n++ {
		c.sub(a[fmt.Sprint("g", n)], SubsidyAction{Op: "approve_c", ID: "C-maximum"}, true)
	}
	c.now += subsidyWeek
	c.block()
	c.sub(a["g0"], SubsidyAction{Op: "issue_c", ID: "C-maximum", Amount: grant, Batch: "C-max-day-1"}, true)
	c.sub(a["g0"], SubsidyAction{Op: "issue_c", ID: "C-maximum", Batch: "C-max-too-early"}, false)
	c.now = (c.now/86400 + 1) * 86400
	c.sub(a["g0"], SubsidyAction{Op: "issue_c", ID: "C-maximum", Batch: "C-max-day-2"}, true)
	c.sub(a["g0"], SubsidyAction{Op: "issue_c", ID: "C-maximum", Amount: 1, Batch: "C-max-over"}, false)
	c.sub(a["developer"], SubsidyAction{Op: "distribute_c", ID: "C-maximum", To: end.id, Amount: 3000 * p.StandardPrice, Batch: "C-max-users"}, true)
	t.Log("PASS maximum 0.25% C proposal funded in two daily-compliant tranches; same-day remainder and over-funding rejected; end-user distribution spans traceable parents")
	c.now = time.Unix(p.GenesisTime, 0).AddDate(1, 0, 1).Unix()
	c.block()
	c.tx(newcomer, Action{Type: "transfer", To: a["payer"].id, Amount: 1}, false)
	c.tx(newcomer, Action{Type: "stake", Amount: 1}, false)
	if c.state().Accounts[newcomer.id].Marked <= 0 {
		t.Fatal("activated voucher disappeared with age")
	}
	t.Log("PASS activated mark remains consume-only after more than twelve months")
	s := c.state()
	if len(s.Issuance.Reminders) == 0 {
		t.Fatal("12-month governor reminder absent")
	}
	for _, q := range s.Issuance.Proposals {
		if q.Issued && q.Remaining != 0 {
			t.Fatal("quarterly C remainder not recycled")
		}
	}
	restarted, e := OpenApplication(c.path)
	if e != nil {
		t.Fatal(e)
	}
	beforeHash, _ := s.Hash()
	after, _ := restarted.State()
	afterHash, _ := after.Hash()
	if !equal(beforeHash, afterHash) {
		t.Fatal("restart changed consensus state")
	}
	for _, path := range []string{"/subsidy", "/subsidy-monthly", "/governor-metrics", "/account/" + PoolAccount} {
		q, e := restarted.Query(context.Background(), &abci.QueryRequest{Path: path})
		if e != nil || q.Code != 0 {
			t.Fatal("query unavailable", path, e)
		}
	}
	t.Logf("PASS restart and public queries; FINAL POOL initial=%d issued=%d recycled=%d balance=%d grants=%d", s.Issuance.Initial, s.Issuance.Issued, s.Issuance.Recycled, s.Accounts[PoolAccount].Balance, len(s.Issuance.Grants))
}

func TestGenesisIssuanceExactCaps(t *testing.T) {
	g, a := issuanceGenesis(t)
	s, e := NewState(g)
	if e != nil {
		t.Fatal(e)
	}
	s.Issuance.Confirmed[a["payer"].id] = true
	// Every exact integer cap, plus the total cap, is exercised in the same day.
	for _, channel := range []string{"A", "B", "C"} {
		amount := scaled(s.Issuance.Initial, map[string]int64{"A": 60, "B": 25, "C": 15}[channel], 10000)
		if e = s.issue(channel, "boundary", channel, a["payer"].id, amount, false); e != nil {
			t.Fatal(e)
		}
		if e = s.issue(channel, "boundary", channel+"-extra", a["payer"].id, 1, false); e == nil {
			t.Fatal("sub-cap exceeded", channel)
		}
		t.Logf("PASS %s cap exactly %d subunits, next subunit rejected", channel, amount)
	}
	d := s.Issuance.Days[s.Time/86400]
	if d.Total != scaled(s.Issuance.Initial, 100, 10000) {
		t.Fatal("daily total not exact")
	}
	if e = s.AdvanceTime((s.Time/86400+1)*86400 - 1); e != nil {
		t.Fatal(e)
	}
	if e = s.issue("A", "boundary", "last-second", a["payer"].id, 1, false); e == nil {
		t.Fatal("cap reset too early")
	}
	if e = s.AdvanceTime(s.Time + 1); e != nil {
		t.Fatal(e)
	}
	if e = s.issue("A", "boundary", "next-day", a["payer"].id, 1, false); e != nil {
		t.Fatal(e)
	}
	t.Log("PASS sum of non-fungible channel caps equals exact 1% daily total; reset at UTC midnight only")
}

func TestGenesisIssuanceRefundAndRestrictions(t *testing.T) {
	g, a := issuanceGenesis(t)
	s, e := NewState(g)
	if e != nil {
		t.Fatal(e)
	}
	id := a["payer"].id
	s.Accounts[id].Balance = 400
	s.Accounts[id].Marked = 600
	marked, e := s.debitAllInference(id, 1000)
	if e != nil {
		t.Fatal(e)
	}
	h := &Hold{Payer: id, Amount: 1000, Actual: 500, Marked: marked}
	if e = s.refundInference(h, 500); e != nil {
		t.Fatal(e)
	}
	if s.Accounts[id].Balance != 400 || s.Accounts[id].Marked != 100 || h.Marked != 500 {
		t.Fatal("inference did not spend marked first")
	}
	if e = s.consumerRebate(h, 25); e != nil {
		t.Fatal(e)
	}
	if s.Accounts[id].Marked != 125 || s.Accounts[id].Balance != 400 {
		t.Fatal("payer rebate laundered marked funds")
	}
	marked, e = s.debitAllInference(id, 500)
	if e != nil {
		t.Fatal(e)
	}
	h = &Hold{Payer: id, Amount: 500, Marked: marked}
	if e = s.refundInference(h, 500); e != nil {
		t.Fatal(e)
	}
	if s.Accounts[id].Marked != 125 || s.Accounts[id].Balance != 400 {
		t.Fatal("cancel/expiry refund laundered marks")
	}
	s.Accounts[id].Balance = 0
	if e = s.debitBalance(id, 1); e == nil {
		t.Fatal("clean-only bond/deposit debit spent voucher")
	}
	raw, _ := Sign(s.Parameters.ChainID, a["payer"].key, 1, Action{Type: "claim_subsidy", Amount: 1})
	if _, e = s.Apply(raw, 1); e == nil {
		t.Fatal("legacy mint enabled")
	}
	t.Log("PASS mixed funding consumes marked first; unused escrow and cancellation preserve marks; bond/deposit cannot debit marked; legacy issuance disabled")
}

func TestGenesisGovernorAndCPolicies(t *testing.T) {
	g, a := issuanceGenesis(t)
	s, e := NewState(g)
	if e != nil {
		t.Fatal(e)
	}
	run := func(who actor, action Action, want bool) {
		t.Helper()
		raw, _ := Sign(s.Parameters.ChainID, who.key, s.Accounts[who.id].Nonce+1, action)
		_, e := s.Apply(raw, s.Height+1)
		if (e == nil) != want {
			t.Fatalf("action %+v want=%v error=%v", action, want, e)
		}
	}
	sub := func(who actor, b SubsidyAction, want bool) {
		t.Helper()
		run(who, Action{Type: "subsidy", Subsidy: &b}, want)
	}
	target := Action{Type: "set_audit_work", AuditWork: &AuditWorkParameters{EpochBlocks: 200, MaxPerExecutor: 40}}
	run(a["g0"], target, false)
	for n := 0; n < 3; n++ {
		sub(a[fmt.Sprint("g", n)], SubsidyAction{Op: "approve_governance", GovernanceAction: &target}, true)
		run(a["g0"], target, n == 2)
	}
	run(a["g0"], target, false)
	q := SubsidyAction{Op: "propose_c", ID: "c-policy", To: a["developer"].id, Amount: 100, Notice: "Public developer product and user plan"}
	sub(a["g0"], q, true)
	for n := 0; n < 3; n++ {
		sub(a[fmt.Sprint("g", n)], SubsidyAction{Op: "approve_c", ID: q.ID}, true)
	}
	if e = s.AdvanceTime(s.Time + subsidyWeek); e != nil {
		t.Fatal(e)
	}
	sub(a["g0"], SubsidyAction{Op: "issue_c", ID: q.ID, Batch: "policy"}, false)
	sub(a["g3"], SubsidyAction{Op: "approve_c", ID: q.ID}, true)
	sub(a["g0"], SubsidyAction{Op: "issue_c", ID: q.ID, Batch: "policy"}, false) // No disclosure.
	sub(a["developer"], SubsidyAction{Op: "confirm", Confirm: true}, true)
	sub(a["g0"], SubsidyAction{Op: "issue_c", ID: q.ID, Batch: "policy"}, true)
	// Permanent disqualification requires the configured governor quorum.
	revoke := Action{Type: "subsidy", Subsidy: &SubsidyAction{Op: "revoke_c", ID: q.ID, Notice: "Public violation finding"}}
	run(a["g0"], revoke, false)
	for n := 0; n < 3; n++ {
		sub(a[fmt.Sprint("g", n)], SubsidyAction{Op: "approve_governance", GovernanceAction: &revoke}, true)
	}
	run(a["g0"], revoke, true)
	q.ID = "c-reapply"
	sub(a["g0"], q, false)
	if s.Issuance.Recycled != 100 {
		t.Fatal("revocation did not recycle developer inventory")
	}
	t.Log("PASS governor 3-of-5 with replay protection, C independently 4-of-5, recipient-signed disclosure required, governance revocation and permanent developer disqualification")
}

func TestGenesisGovernorTriggerMetrics(t *testing.T) {
	g, a := issuanceGenesis(t)
	s, e := NewState(g)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.AdvanceTime(s.Time + 30*86400); e != nil {
		t.Fatal(e)
	}
	for j := int64(1); j <= 30; j++ {
		s.Issuance.Settlements[s.Time/86400-j] = &SettlementDay{Total: 100, External: 20}
	}
	s.updateGovernorReminders()
	m := s.governorMetrics()
	if m.ConsecutiveDays != 30 || m.TrailingTotal != 3000 || m.TrailingExternal != 600 || m.TrailingExternalBPS != 2000 || len(m.Reminders) != 1 {
		t.Fatal(m)
	}
	s.Issuance.Issued = s.Issuance.Initial/2 - 1
	s.updateGovernorReminders()
	if len(s.Issuance.Reminders) != 1 {
		t.Fatal("50% trigger early")
	}
	s.Issuance.Issued++
	s.updateGovernorReminders()
	if len(s.Issuance.Reminders) != 2 {
		t.Fatal("50% trigger absent")
	}
	if e = s.AdvanceTime(time.Unix(g.Parameters.GenesisIssuance.GenesisTime, 0).AddDate(1, 0, 0).Unix()); e != nil {
		t.Fatal(e)
	}
	if len(s.Issuance.Reminders) != 3 {
		t.Fatal("12-month trigger absent")
	}
	for _, r := range s.Issuance.Reminders {
		if r.Due-r.At != 90*86400 || r.RequiredSigners != 7 || r.CrossOrganization != 2 || r.Threshold != 4 {
			t.Fatal(r)
		}
	}
	// Count pipeline external work proportionally, not all-or-nothing per request.
	s.Issuance.Settlements = map[int64]*SettlementDay{}
	s.Parameters.GenesisIssuance.Entities[a["node"].id] = "founder"
	h := &Hold{Payer: a["payer"].id, Pipeline: &PipelineHold{Credits: []core.StageShare{{NodeID: a["node"].id, Amount: 80}, {NodeID: a["auditor"].id, Amount: 20}}}}
	s.subsidySettlement(h)
	day := s.Issuance.Settlements[s.Time/86400]
	if day.Total != 100 || day.External != 20 {
		t.Fatal(day)
	}
	// Zero traffic must not qualify as 20% external share.
	s.Issuance.Settlements = map[int64]*SettlementDay{}
	if s.governorMetrics().ConsecutiveDays != 0 {
		t.Fatal("empty days qualified")
	}
	t.Log("PASS three governor triggers, 20% threshold for 30 complete UTC days, mixed pipeline amounts, zero-data exclusion, 90-day reminders and 4-of-7 / two outside signers")
}

func TestGenesisRetrospectiveScalingAndValidation(t *testing.T) {
	p := &IssuanceParameters{StandardPrice: 1, Entities: map[string]string{"consumer": "consumer"}}
	// 350 suppliers each earn 1002 points, capped at 1000, then the list
	// exceeds B's 342857 budget. Flooring leaves its remainder in B.
	for n := 0; n < 350; n++ {
		id := fmt.Sprint("supplier", n)
		p.Entities[id] = id
		for j := 0; j < 167; j++ {
			p.Retrospective = append(p.Retrospective, ContributionRecord{RequestID: fmt.Sprintf("%d-%d", n, j), Payer: "consumer", Executor: id, Settled: true})
		}
	}
	awards, e := RecomputeRetrospective(p)
	if e != nil {
		t.Fatal(e)
	}
	var sum int64
	for _, v := range awards {
		if v.Amount != scaled(1000, 342857, 351000) {
			t.Fatal("incorrect proportional scaling", v)
		}
		sum += v.Amount
	}
	if sum > 342857 {
		t.Fatal("B budget exceeded")
	}
	p.Retrospective = append(p.Retrospective, p.Retrospective[0])
	if _, e = RecomputeRetrospective(p); e == nil {
		t.Fatal("duplicate record accepted")
	}
	g, _ := issuanceGenesis(t)
	g.Parameters.GenesisIssuance.OrganicForecast = 2*PoolEquivalents - 1
	if _, e = NewState(g); e == nil {
		t.Fatal("organic guardrail bypass")
	}
	g, _ = issuanceGenesis(t)
	g.Parameters.GenesisIssuance.Rules = fixedIssuanceRules()
	g.Parameters.GenesisIssuance.Rules.SingleGrant = 101
	if _, e = NewState(g); e == nil {
		t.Fatal("decided number overridden")
	}
	g, _ = issuanceGenesis(t)
	g.Parameters.Misjudge.SlashMode = "full"
	if _, e = NewState(g); e == nil {
		t.Fatal("genesis slash mode override accepted")
	}
	g, _ = issuanceGenesis(t)
	g.Parameters.GenesisIssuance.GovernorSigners[4] = g.Parameters.GenesisIssuance.GovernorSigners[0]
	if _, e = NewState(g); e == nil {
		t.Fatal("duplicate governor")
	}
	t.Log("PASS B proportional scaling with deterministic rounding and unused budget retained; duplicate records, guardrail violations, changed fixed numbers and duplicate governors rejected")
}
