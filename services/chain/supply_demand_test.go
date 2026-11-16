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
	"strings"
	"testing"

	core "ember/core-go"
	pb "ember/proto"
	abci "github.com/cometbft/cometbft/abci/types"
	"google.golang.org/protobuf/proto"
)

type priceChain struct {
	t      *testing.T
	app    *Application
	actors map[string]actor
	nodes  []actor
	base   *pb.ChainGenesis
}

func newPriceChain(t *testing.T, policy PriceParameters, supply int) *priceChain {
	t.Helper()
	s, a, n, _, _ := pricedFixture(t)
	s.Parameters.SupplyDemand = policy
	s.Parameters.RegisteredAuditors = true
	s.Parameters.RegistrationTTL = 1000
	s.Parameters.HoldTimeout = 100
	s.Parameters.Assignment.CooldownRequests = 100
	s.Parameters.AuditWork.MaxPerExecutor = 10000
	m := s.Models["ember-unit-test"]
	g := &pb.ChainGenesis{}
	mustPrice(t, proto.Unmarshal(m.Genesis, g))
	// Dedicated signed test profile, using the published output rate 513.
	g.PriceInput, g.PriceOutput, g.PriceContext = 1, 513, 0
	mustPrice(t, core.Sign(g, a["governor"].key))
	m.Genesis, _ = proto.Marshal(g)
	hash := sha256.Sum256(m.Genesis)
	m.GenesisHash = hex.EncodeToString(hash[:])
	m.Pricing = &RolePricing{TargetExecutors: 4, TargetAuditors: 3, ExecutorWeight: 80, AuditorWeight: 15, ConsumerWeight: 5}
	m.AuditComp = auditCompFixture(t)
	gen := GenesisState{Parameters: s.Parameters, Models: []Model{m, {ID: "protected", GenesisHash: strings.Repeat("1", 64), ProfileHash: strings.Repeat("2", 64)}}, Coordinators: []string{a["coordinator"].id}, Governors: []string{a["governor"].id}}
	for id, acc := range s.Accounts {
		gen.Accounts = append(gen.Accounts, GenesisAccount{ID: id, Balance: 10000000, Stake: max(acc.Stake, 10000000)})
	}
	nodes := append(n, a["auditor"])
	for i := 4; i < supply; i++ {
		who := newActor(byte(40 + i))
		nodes = append(nodes, who)
		gen.Accounts = append(gen.Accounts, GenesisAccount{ID: who.id, Balance: 10000000, Stake: 10000000})
	}
	for _, who := range nodes {
		gen.Auditors = append(gen.Auditors, who.id)
	}
	app := NewMemoryApplication()
	raw, _ := json.Marshal(gen)
	_, err := app.InitChain(context.Background(), &abci.InitChainRequest{ChainId: s.Parameters.ChainID, AppStateBytes: raw})
	mustPrice(t, err)
	c := &priceChain{t: t, app: app, actors: a, nodes: nodes, base: g}
	for i, who := range nodes {
		expiry := int64(1000)
		if supply == 4 && (i == 1 || i == 2) {
			expiry = max(int64(12), policy.EpochBlocks)
		}
		c.register(who, expiry)
	}
	return c
}
func mustPrice(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func (c *priceChain) state() *State { s, err := c.app.State(); mustPrice(c.t, err); return s }
func (c *priceChain) block(txs ...[]byte) {
	c.t.Helper()
	height := c.state().Height + 1
	hash := sha256.Sum256([]byte(fmt.Sprint("price-block-", height)))
	r, err := c.app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: height, Hash: hash[:], Txs: txs})
	mustPrice(c.t, err)
	for _, r := range r.TxResults {
		if r.Code != 0 {
			c.t.Fatal(r.Log)
		}
	}
	_, err = c.app.Commit(context.Background(), &abci.CommitRequest{})
	mustPrice(c.t, err)
}
func (c *priceChain) to(height int64) {
	for c.state().Height < height {
		c.block()
	}
}
func (c *priceChain) tx(who actor, a Action) {
	s := c.state()
	raw, err := Sign(s.Parameters.ChainID, who.key, s.Accounts[who.id].Nonce+1, a)
	mustPrice(c.t, err)
	c.block(raw)
}
func (c *priceChain) register(who actor, expiry int64) {
	p := &pb.ResourceProfile{NodeId: who.id, ChainId: c.base.ChainId, ManifestRoot: c.base.ModelManifestRoot, ExecutionProfileVer: c.base.ExecutionProfileVer, ActualDevice: "cpu", TorchVersion: "2.5.1", EngineCapabilities: &pb.EngineCapabilities{Roles: []string{"model", "auditor"}, EngineName: "transformers", EngineVersion: "4.45.2", ForcedPrefixReplay: true, ScoreKind: "raw-logits", MaxTopK: 16, EvidenceLevels: []string{"full"}}}
	mustPrice(c.t, core.Sign(p, who.key))
	raw, _ := proto.Marshal(p)
	c.tx(who, Action{Type: "register_node", Profile: raw, ExpiresHeight: expiry})
}
func (c *priceChain) freeze(id string) map[string]any {
	s := c.state()
	p := s.PriceForProfile(c.base.ChainId)
	base, _ := core.TokenFee(c.base, 33, 32)
	fee, err := core.ScaleFee(base.Total, p.MultiplierPPM)
	mustPrice(c.t, err)
	commitment := &pb.ExecutionCommitment{TokenizerVersion: "unit-tokenizer-v1", RequestId: id, ChainId: c.base.ChainId, ProfileVer: c.base.ExecutionProfileVer, InputTokens: 33, MaxOutputTokens: 32, InputCommitment: make([]byte, 32)}
	mustPrice(c.t, core.Sign(commitment, c.nodes[0].key))
	raw, _ := proto.Marshal(commitment)
	c.tx(c.actors["payer"], Action{Type: "freeze", HoldID: id, Payer: c.actors["payer"].id, Node: c.nodes[0].id, Amount: fee, PricingCommitment: raw, ExpectedPrice: &PriceStamp{MultiplierPPM: p.MultiplierPPM, EffectiveHeight: p.EffectiveHeight}})
	h := c.state().Holds[id]
	if h.Amount != fee || h.TokenFee.MultiplierPPM != p.MultiplierPPM {
		c.t.Fatal("quote/freeze mismatch")
	}
	return map[string]any{"id": id, "base_fee": base.Total, "quote": fee, "freeze": h.Amount, "multiplier_ppm": p.MultiplierPPM, "effective_height": p.EffectiveHeight, "freeze_height": h.FreezeHeight}
}
func (c *priceChain) complete(row map[string]any) {
	id := row["id"].(string)
	s := c.state()
	h := s.Holds[id]
	ref := sha256.Sum256(h.TokenFee.Commitment)
	chunk := &pb.ChunkRoot{RequestId: id, UptoStep: 16, MerkleRoot: make([]byte, 32)}
	mustPrice(c.t, core.Sign(chunk, c.nodes[0].key))
	previous, _ := core.Digest(chunk)
	chunk2 := &pb.ChunkRoot{RequestId: id, Seq: 1, UptoStep: 32, PrevRoot: previous, MerkleRoot: make([]byte, 32)}
	mustPrice(c.t, core.Sign(chunk2, c.nodes[0].key))
	r := &pb.ExecutionReceipt{RequestId: id, NodeId: c.nodes[0].id, InputTokens: 33, OutputTokens: 32, CommitmentRef: ref[:], ChunkRoots: []*pb.ChunkRoot{chunk, chunk2}}
	mustPrice(c.t, core.Sign(r, c.nodes[0].key))
	raw, _ := proto.Marshal(r)
	c.tx(c.nodes[0], Action{Type: "submit", HoldID: id, Actual: row["quote"].(int64), Receipt: raw})
	if c.state().PriceForProfile(c.base.ChainId).CumulativeSamples != s.PriceForProfile(c.base.ChainId).CumulativeSamples {
		c.t.Fatal("unsettled submit counted as demand")
	}
	c.to(max(h.FreezeHeight+3, c.state().Height))
	c.tx(c.actors["coordinator"], Action{Type: "assign_audit", HoldID: id})
	h = c.state().Holds[id]
	var auditor actor
	for _, a := range c.nodes {
		if a.id == h.Auditor {
			auditor = a
		}
	}
	proof, _ := hex.DecodeString(h.AuditProof)
	challenge, verdict, _ := signedAudit(c.t, raw, auditor, "accept", proof)
	v := &pb.AuditVerdict{}
	proto.Unmarshal(verdict, v)
	v.NOutBilled = 1
	mustPrice(c.t, core.Sign(v, auditor.key))
	verdict, _ = proto.Marshal(v)
	c.tx(auditor, Action{Type: "audit", HoldID: id, Challenge: challenge, Verdict: verdict})
	h = c.state().Holds[id]
	receipts, err := ExpectedAuditReceipts(h, "accept")
	mustPrice(c.t, err)
	c.tx(c.actors["payer"], Action{Type: "settle", HoldID: id, Outcome: "accept", AuditReceipts: receipts})
	h = c.state().Holds[id]
	parts := allocateFee(h.Actual, [3]int64{80, 15, 5})
	if h.State != "settled" || h.Actual != row["quote"] || h.Shares.Executor != parts[0] || h.Shares.Auditor != parts[1] || h.Shares.Consumer != parts[2] {
		c.t.Fatal("settlement split/fee mismatch")
	}
	row["settle"] = h.Actual
	row["settle_height"] = c.state().Height
	row["shares"] = h.Shares
	row["audit_escrow"] = h.AuditEscrow
	row["audit_receipts"] = h.AuditReceipts
}
func priceTotal(s *State) int64 {
	var total int64
	for _, a := range s.Accounts {
		total += a.Balance + a.Stake
		for _, u := range a.Unbondings {
			total += u.Amount
		}
	}
	for _, h := range s.Holds {
		if h.State == "frozen" || h.State == "dispute" {
			total += h.Amount
		}
		total += h.AuditEscrow
	}
	return total
}

// TestSupplyDemandRehearsal drives a local ABCI chain with signed transactions,
// finalized block hashes and actual audit/settle transitions. No RPC is used.
func TestSupplyDemandRehearsal(t *testing.T) {
	policy := DefaultPriceParameters()
	policy.EpochBlocks = 20
	policy.MinSettledSamples = 1
	c := newPriceChain(t, policy, 4)
	before := priceTotal(c.state())
	issued := c.state().SubsidyIssued
	first := c.freeze("first")
	c.complete(first)
	c.to(20)
	half := c.state().PriceForProfile(c.base.ChainId)
	if half.MultiplierPPM != 1050000 || half.Supply != 2 || half.Previous.Requests != 1 || half.Previous.Tokens != 65 {
		t.Fatal("half supply", half)
	}
	c.register(c.nodes[1], 1000)
	c.register(c.nodes[2], 1000)
	second := c.freeze("second")
	c.complete(second)
	c.to(38)
	cross := c.freeze("cross-epoch")
	c.to(40)
	double := c.state().PriceForProfile(c.base.ChainId)
	if double.MultiplierPPM != 1000000 || double.Supply != 4 {
		t.Fatal("double supply", double)
	}
	c.complete(cross)
	c.to(60)
	// Queries must be pure reads: compare identical finalized states and future
	// epoch transitions against a no-query control chain.
	control, err := c.state().Clone()
	mustPrice(t, err)
	spamBefore := c.state().PriceForProfile(c.base.ChainId).MultiplierPPM
	for i := 0; i < 10000; i++ {
		r, err := c.app.Query(context.Background(), &abci.QueryRequest{Path: "/price/" + c.base.ChainId})
		mustPrice(t, err)
		if r.Code != 0 {
			t.Fatal(r.Log)
		}
		p := ProfilePrice{}
		mustPrice(t, json.Unmarshal(r.Value, &p))
		base, _ := core.TokenFee(c.base, 33, 32)
		_, err = core.ScaleFee(base.Total, p.MultiplierPPM)
		mustPrice(t, err)
	}
	got, _ := c.state().Hash()
	want, _ := control.Hash()
	if !reflect.DeepEqual(got, want) {
		t.Fatal("quote query changed consensus state")
	}
	other := &priceChain{t: t, app: &Application{committed: control}}
	c.to(80)
	other.to(80)
	got, _ = c.state().Hash()
	want, _ = other.state().Hash()
	if !reflect.DeepEqual(got, want) || c.state().PriceForProfile(c.base.ChainId).MultiplierPPM != spamBefore {
		t.Fatal("spam changed next price")
	}
	protected := c.state().PriceForProfile("protected")
	if protected.MultiplierPPM != PriceScale || protected.CumulativeSamples != 0 {
		t.Fatal(protected)
	}
	after := priceTotal(c.state())
	if after != before || c.state().SubsidyIssued != issued {
		t.Fatal("issuance", before, after)
	}
	// Independent chains force each clamp using only governance/genesis settings,
	// actual accepted settlements and natural registration expiry.
	runClamp := func(p PriceParameters, n int) ProfilePrice {
		q := newPriceChain(t, p, n)
		r := q.freeze("clamp")
		q.complete(r)
		q.to(max(int64(12), p.EpochBlocks))
		if priceTotal(q.state()) != priceTotalFromGenesis(before, n) {
			t.Fatal("clamp chain issuance changed")
		}
		return q.state().PriceForProfile(q.base.ChainId)
	}
	stepPolicy := policy
	stepPolicy.EpochBlocks = 6
	step := runClamp(stepPolicy, 4)
	if step.MultiplierPPM != 1100000 || step.RawPPM != 1166666 {
		t.Fatal("step clamp", step)
	}
	upperPolicy := policy
	upperPolicy.MaxPPM = 1020000
	upper := runClamp(upperPolicy, 4)
	if upper.MultiplierPPM != 1020000 || upper.RawPPM != 1050000 {
		t.Fatal("upper corridor", upper)
	}
	lowerPolicy := policy
	lowerPolicy.MinPPM = 980000
	lower := runClamp(lowerPolicy, 8)
	if lower.MultiplierPPM != 980000 || lower.RawPPM != 950000 {
		t.Fatal("lower corridor", lower)
	}
	protectionPolicy := policy
	protectionPolicy.MinSettledSamples = 2
	sampledProtection := runClamp(protectionPolicy, 4)
	if sampledProtection.MultiplierPPM != PriceScale || sampledProtection.CumulativeSamples != 1 {
		t.Fatal("sampled protection", sampledProtection)
	}
	// Explicit authorized work-output issuance is independent of all fee changes.
	c.tx(c.nodes[0], Action{Type: "claim_subsidy", EvidenceRef: c.state().Holds["first"].EvidenceRef, Amount: 30})
	if priceTotal(c.state()) != after+30 || c.state().SubsidyIssued != issued+30 {
		t.Fatal("work-output issuance audit")
	}
	// Persist and restart the consensus price state without changing its hash.
	path := filepath.Join(t.TempDir(), "price-state.json")
	mustPrice(t, SaveState(path, c.state()))
	restarted, err := OpenApplication(path)
	mustPrice(t, err)
	restored, err := restarted.State()
	mustPrice(t, err)
	got, _ = c.state().Hash()
	want, _ = restored.Hash()
	if !reflect.DeepEqual(got, want) {
		t.Fatal("price state changed on restart")
	}
	evidence := map[string]any{"chain": "local-deterministic-abci", "app_version": AppVersion, "epoch_blocks": 20, "formula": "P=min(1000000,(Q+ceil(T/1024))*1000000/L); raw=1000000+trunc(P*(K-S)/max(K,S,1)); corridor then relative step clamp", "half_supply": map[string]any{"before_supply": 4, "after_supply": 2, "expected_ppm": 1050000, "observed": half}, "double_supply": map[string]any{"before_supply": 2, "after_supply": 4, "expected_ppm": 1000000, "observed": double}, "requests": []any{first, second, cross}, "quote_spam": map[string]any{"queries": 10000, "before_ppm": spamBefore, "after_ppm": c.state().PriceForProfile(c.base.ChainId).MultiplierPPM, "state_hash_matches_control": true, "settled_requests": 0}, "step_clamp": step, "upper_corridor": upper, "lower_corridor": lower, "protected": protected, "sampled_protection": sampledProtection, "default_step_policy": stepPolicy, "restart_hash_preserved": true, "issuance": map[string]any{"total_before": before, "total_after": after, "subsidy_before": issued, "subsidy_after": issued, "authorized_work_output": 30, "total_after_work_output": priceTotal(c.state()), "subsidy_after_work_output": c.state().SubsidyIssued, "genesis_pool_used": false, "split": "80/15/5, largest remainder", "audit_formula_unchanged": true}}
	raw, err := json.MarshalIndent(evidence, "", "  ")
	mustPrice(t, err)
	t.Log(string(raw))
	if dir := os.Getenv("PRICING_EVIDENCE_DIR"); dir != "" {
		mustPrice(t, os.MkdirAll(dir, 0755))
		mustPrice(t, os.WriteFile(filepath.Join(dir, "summary.json"), append(raw, '\n'), 0644))
	}
}
func priceTotalFromGenesis(before int64, n int) int64 {
	return before + int64(max(0, n-4))*20000000
}

func TestPriceStepIntegerAndProtectionBoundaries(t *testing.T) {
	p := DefaultPriceParameters()
	for _, tc := range []struct {
		old     int64
		d       SettledDemand
		s, k, l int64
		want    int64
	}{{1000000, SettledDemand{100, 102400}, 0, 4, 20, 1100000}, {1000000, SettledDemand{100, 102400}, 8, 4, 20, 900000}, {800000, SettledDemand{100, 102400}, 100, 4, 20, 800000}, {2000000, SettledDemand{100, 102400}, 0, 4, 20, 2000000}, {1500000, SettledDemand{}, 0, 4, 20, 1500000}, {1000000, SettledDemand{math.MaxUint64, math.MaxUint64}, 0, 4, 20, 1100000}} {
		got, _, _ := priceStep(tc.old, tc.d, tc.s, tc.k, tc.l, p)
		if got != tc.want {
			t.Fatal(tc, got)
		}
	}
	for _, ppm := range []int64{800000, 1000000, 2000000} {
		s, a, n, _, _ := pricedFixture(t)
		s.priceRecord("ember-unit-test").MultiplierPPM = ppm
		c := tokenCommitment(t, n[0], 33, 100)
		base, _ := core.TokenFee(&pb.ChainGenesis{PriceInput: 2, PriceOutput: 3, PriceContext: 1}, 33, 100)
		amount, _ := core.ScaleFee(base.Total, ppm)
		apply(t, s, a["payer"], 1, Action{Type: "freeze", HoldID: "units", Payer: a["payer"].id, Node: n[0].id, Amount: amount, PricingCommitment: c}, 2)
		f := s.Holds["units"].TokenFee
		actual, _ := core.ScaleFee(69, ppm)
		if err := verifyTokenFee(f, 33, 1, actual, nil); err != nil {
			t.Fatal(err)
		}
		s.priceRecord("ember-unit-test").MultiplierPPM = 2000000
		if err := verifyTokenFee(f, 33, 1, actual, nil); err != nil {
			t.Fatal("frozen price changed", err)
		}
	}
}
func TestPriceGovernanceAndStaleQuote(t *testing.T) {
	p := DefaultPriceParameters()
	p.EpochBlocks = 20
	p.MinSettledSamples = 1
	c := newPriceChain(t, p, 4)
	next := p
	next.EpochBlocks = 30
	c.tx(c.actors["governor"], Action{Type: "set_supply_demand", SupplyDemand: &next})
	if c.state().Parameters.SupplyDemand.EpochBlocks != 20 {
		t.Fatal("mid-epoch governance")
	}
	c.to(20)
	if c.state().Parameters.SupplyDemand.EpochBlocks != 30 || c.state().PendingPriceParameters != nil {
		t.Fatal("governance not applied")
	}
	s, a, n, _, _ := pricedFixture(t)
	commitment := tokenCommitment(t, n[0], 33, 100)
	for _, stamp := range []*PriceStamp{{MultiplierPPM: 1100000, EffectiveHeight: 1}, {MultiplierPPM: 1000000, EffectiveHeight: 0}} {
		rejected(t, s, a["payer"], Action{Type: "freeze", HoldID: "units", Payer: a["payer"].id, Node: n[0].id, Amount: 369, PricingCommitment: commitment, ExpectedPrice: stamp}, 2)
	}
	for _, bad := range []PriceParameters{{}, {EpochBlocks: 1, MinPPM: 1100000, MaxPPM: 2000000, StepPPM: 100000, MinSettledSamples: 1}, {EpochBlocks: 1, MinPPM: 800000, MaxPPM: 2000000, StepPPM: 1000001, MinSettledSamples: 1}} {
		if bad.valid() {
			t.Fatal("invalid governance accepted", bad)
		}
	}
}
