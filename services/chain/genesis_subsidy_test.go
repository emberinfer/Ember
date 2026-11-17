package chain

import (
	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
	"os"
	"testing"
)

func TestGenesisSubsidyPool(t *testing.T) {
	s := &State{Accounts: map[string]*Account{}}
	if err := s.initGenesisSubsidy(&GenesisSubsidy{OrganicYearEquivalents: SubsidyTotalEquivalents * 2}); err != nil {
		t.Fatal(err)
	}
	p := s.GenesisSubsidy
	for ch, equiv := range map[string]int64{"A": SubsidyAEquivalents, "B": SubsidyBEquivalents, "C": SubsidyCEquivalents} {
		if p.Channels[ch].Balance != equiv*SubsidyEquivalentSubunits {
			t.Fatalf("channel %s: %+v", ch, p.Channels[ch])
		}
	}
	if p.Total != SubsidyTotalEquivalents*SubsidyEquivalentSubunits || s.Accounts[p.Account].Balance != p.Total {
		t.Fatal("pool funding")
	}
	small := &State{Accounts: map[string]*Account{}}
	if err := small.initGenesisSubsidy(&GenesisSubsidy{OrganicYearEquivalents: 100}); err != nil {
		t.Fatal(err)
	}
	if small.GenesisSubsidy.Total != 50*SubsidyEquivalentSubunits {
		t.Fatal("guardrail did not scale")
	}
	for ch, c := range small.GenesisSubsidy.Channels {
		if c.Initial > p.Channels[ch].Initial {
			t.Fatal("scaled upward")
		}
	}
}

func TestPermanentConsumptionCredit(t *testing.T) {
	s := &State{Accounts: map[string]*Account{"payer": {Balance: 10, ConsumptionCredit: 100}, "node": {}}, Holds: map[string]*Hold{}}
	if s.debitBalance("payer", 11) == nil {
		t.Fatal("transfer/stake/bond can spend credit")
	}
	if err := s.debitInference("payer", 105); err != nil {
		t.Fatal(err)
	}
	h := &Hold{Payer: "payer", Node: "node", Amount: 105, SubsidyDebit: s.takeInferenceDebit(), Actual: 60}
	if h.SubsidyDebit != 100 {
		t.Fatal("credit not frozen")
	}
	if err := s.refundInference(h, 45); err != nil {
		t.Fatal(err)
	}
	if s.Accounts["payer"].Balance != 10 || s.Accounts["payer"].ConsumptionCredit != 40 {
		t.Fatal("refund laundered credit")
	}
	if err := s.creditBalance("node", 60); err != nil {
		t.Fatal(err)
	}
	if s.Accounts["node"].Balance != 60 || s.Accounts["node"].ConsumptionCredit != 0 {
		t.Fatal("executor did not receive clean Spark")
	}
	if err := s.rebateInference(h, 3); err != nil {
		t.Fatal(err)
	}
	if s.Accounts["payer"].ConsumptionCredit != 43 {
		t.Fatal("rebate laundered credit")
	}
	if s.Accounts["payer"].Balance != 10 {
		t.Fatal("rebate minted clean Spark")
	}
	clone, err := s.Clone()
	if err != nil {
		t.Fatal(err)
	}
	if clone.Accounts["payer"].ConsumptionCredit != 43 {
		t.Fatal("credit lost on persistence")
	}
}

func subsidyFixture(t *testing.T) *State {
	t.Helper()
	s := &State{Accounts: map[string]*Account{"alice": {}, "bob": {}}, Holds: map[string]*Hold{}}
	if err := s.initGenesisSubsidy(&GenesisSubsidy{OrganicYearEquivalents: SubsidyTotalEquivalents * 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.advanceSubsidyTime(SubsidyDaySeconds); err != nil {
		t.Fatal(err)
	}
	return s
}
func acknowledge(t *testing.T, s *State, id string) {
	t.Helper()
	if err := s.subsidyA(id, Action{Type: "subsidy_notice", Notice: SubsidyNotice}); err != nil {
		t.Fatal(err)
	}
}
func TestSubsidyAActivationAndRecovery(t *testing.T) {
	s := subsidyFixture(t)
	if s.subsidyA("alice", Action{Type: "subsidy_claim_a"}) == nil {
		t.Fatal("issued without notice")
	}
	if s.subsidyA("alice", Action{Type: "subsidy_notice", Notice: ""}) == nil {
		t.Fatal("empty notice accepted")
	}
	acknowledge(t, s, "alice")
	acknowledge(t, s, "bob")
	before := s.GenesisSubsidy.Channels["A"].Balance
	for _, id := range []string{"alice", "bob"} {
		if err := s.subsidyA(id, Action{Type: "subsidy_claim_a"}); err != nil {
			t.Fatal(err)
		}
	}
	if s.Accounts["alice"].ConsumptionCredit != 0 {
		t.Fatal("pending credit spendable")
	}
	if s.subsidyA("alice", Action{Type: "subsidy_claim_a"}) == nil {
		t.Fatal("duplicate lifetime claim")
	}
	h := &Hold{Payer: "alice", State: "settled", Actual: 1, AuditInputTokens: SubsidyActivationInput - 1, TokenFee: &FrozenFee{Commitment: []byte{1}}}
	s.activateSubsidy(h)
	if s.GenesisSubsidy.Grants["alice"].Activated {
		t.Fatal("short input activated")
	}
	h.AuditInputTokens = SubsidyActivationInput
	h.State = "dispute"
	s.activateSubsidy(h)
	if s.GenesisSubsidy.Grants["alice"].Activated {
		t.Fatal("unsettled activated")
	}
	h.State = "settled"
	s.activateSubsidy(h)
	s.activateSubsidy(h)
	amount := SubsidyHouseholdEquivalents * SubsidyEquivalentSubunits
	if s.Accounts["alice"].ConsumptionCredit != amount {
		t.Fatal("activation must be once")
	}
	deadline := s.GenesisSubsidy.Grants["bob"].Deadline
	if err := s.advanceSubsidyTime(deadline); err != nil {
		t.Fatal(err)
	}
	if !s.GenesisSubsidy.Grants["bob"].Reclaimed || s.GenesisSubsidy.Channels["A"].Balance != before-amount {
		t.Fatal("expiry did not return pending funds")
	}
	if err := s.advanceSubsidyTime(deadline + 100*SubsidyDaySeconds); err != nil {
		t.Fatal(err)
	}
	if s.Accounts["alice"].ConsumptionCredit != amount {
		t.Fatal("permanent credit expired")
	}
	if s.subsidyA("bob", Action{Type: "subsidy_claim_a"}) == nil {
		t.Fatal("expired lifetime claim reused")
	}
	if s.advanceSubsidyTime(deadline) == nil {
		t.Fatal("time regression accepted")
	}
}

func TestSubsidyNewAccountRequiresNoCleanSpark(t *testing.T) {
	s := subsidyFixture(t)
	s.Parameters.ChainID = "subsidy-test"
	fresh := newActor(100)
	if s.Accounts[fresh.id] != nil {
		t.Fatal("not a fresh account")
	}
	for _, action := range []Action{{Type: "subsidy_claim_a"}, {Type: "subsidy_notice", Notice: "not the notice"}} {
		before, _ := s.Hash()
		raw, err := Sign(s.Parameters.ChainID, fresh.key, 1, action)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.Apply(raw, 1); err == nil {
			t.Fatal("unregistered action accepted")
		}
		after, _ := s.Hash()
		if string(before) != string(after) {
			t.Fatal("rejected action mutated state")
		}
	}
	if s.Accounts[fresh.id] != nil {
		t.Fatal("failed notice created account")
	}
	apply(t, s, fresh, 1, Action{Type: "subsidy_notice", Notice: SubsidyNotice}, 1)
	apply(t, s, fresh, 2, Action{Type: "subsidy_claim_a"}, 2)
	if s.Accounts[fresh.id].Balance != 0 || s.Accounts[fresh.id].ConsumptionCredit != 0 || s.GenesisSubsidy.Grants[fresh.id] == nil {
		t.Fatal("new account needed clean funds or skipped activation")
	}
}

func TestSubsidyConversionMatchesGenesisPriceTable(t *testing.T) {
	// The signed testnet table stays pinned as the historical placeholder:
	// one reference standard request under it costs exactly 768 subunits.
	raw, err := os.ReadFile("../../chain-genesis/ember-smollm2-360m-gpu-v1/genesis.pb")
	if err != nil {
		t.Fatal(err)
	}
	profile := &pb.ChainGenesis{}
	if err = proto.Unmarshal(raw, profile); err != nil {
		t.Fatal(err)
	}
	if err = core.Verify(profile); err != nil {
		t.Fatal(err)
	}
	placeholderFee, err := core.TokenFee(profile, uint32(SubsidyReferenceInput), uint32(SubsidyReferenceOutput))
	if err != nil {
		t.Fatal(err)
	}
	if placeholderFee.Total != 768 {
		t.Fatal("signed testnet placeholder drifted", placeholderFee.Total)
	}
	// The genesis conversion is generated from the calibrated genesis price
	// table (genesis_prices.json): the fee of one reference standard request
	// under the generated reference price must equal SubsidyEquivalentSubunits.
	genesisProfile := &pb.ChainGenesis{
		PriceInput:   GenesisReferencePriceInput,
		PriceOutput:  GenesisReferencePriceOutput,
		PriceContext: GenesisReferencePriceContext,
	}
	fee, err := core.TokenFee(genesisProfile, uint32(SubsidyReferenceInput), uint32(SubsidyReferenceOutput))
	if err != nil {
		t.Fatal(err)
	}
	if fee.Total != SubsidyEquivalentSubunits {
		t.Fatal("generated conversion drifted from genesis price table", fee.Total, SubsidyEquivalentSubunits)
	}
}

// End-to-end through settle(): a request paid from the subsidy sub-balance
// must not leak the 5% consumer rebate into clean, transferable Spark. The
// rebate returns to the marked consumption credit; executor and auditor are
// still paid clean Spark. A mixed payment restricts the whole rebate
// (conservative: the rebate contributes zero clean balance).
func TestSubsidyPricedSettlementRebateRestricted(t *testing.T) {
	s, a := pricingState(t)
	payer := s.Accounts[a["payer"].id]
	payer.Balance = 0
	payer.ConsumptionCredit = 12000
	h := pricingHold(s, a)
	if err := s.debitInference(a["payer"].id, h.Amount); err != nil {
		t.Fatal(err)
	}
	h.SubsidyDebit = s.takeInferenceDebit()
	if h.SubsidyDebit != h.Amount {
		t.Fatal("freeze not fully subsidy funded")
	}
	s.Holds[h.ID] = h
	if err := s.settle(h, "accept"); err != nil {
		t.Fatal(err)
	}
	if h.Shares == nil || h.Shares.Consumer <= 0 {
		t.Fatal("priced settle produced no consumer rebate")
	}
	if payer.Balance != 0 {
		t.Fatal("subsidy-funded settlement changed consumer clean balance")
	}
	if want := h.Amount - h.Actual + h.Shares.Consumer; payer.ConsumptionCredit != want {
		t.Fatal("rebate did not return as consumption credit", payer.ConsumptionCredit, want)
	}
	if s.Accounts[a["node"].id].ConsumptionCredit != 0 || s.Accounts[a["auditor"].id].ConsumptionCredit != 0 {
		t.Fatal("executor or auditor paid in restricted credit")
	}
	// Mixed funding: clean part is refunded clean, but the rebate still
	// contributes zero clean balance.
	s, a = pricingState(t)
	payer = s.Accounts[a["payer"].id]
	payer.Balance = 7000
	payer.ConsumptionCredit = 5000
	h = pricingHold(s, a)
	if err := s.debitInference(a["payer"].id, h.Amount); err != nil {
		t.Fatal(err)
	}
	h.SubsidyDebit = s.takeInferenceDebit()
	if h.SubsidyDebit != 5000 {
		t.Fatal("mixed freeze split")
	}
	s.Holds[h.ID] = h
	if err := s.settle(h, "accept"); err != nil {
		t.Fatal(err)
	}
	if want := h.Amount - h.Actual; payer.Balance != want {
		t.Fatal("mixed settlement leaked rebate into clean balance", payer.Balance, want)
	}
	if payer.ConsumptionCredit != h.Shares.Consumer {
		t.Fatal("mixed settlement rebate not fully restricted", payer.ConsumptionCredit, h.Shares.Consumer)
	}
}
