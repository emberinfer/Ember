package chain

import (
	"bytes"
	"context"
	"crypto/sha256"
	core "ember/core-go"
	pb "ember/proto"
	"encoding/json"
	"fmt"
	abci "github.com/cometbft/cometbft/abci/types"
	"google.golang.org/protobuf/proto"
	"testing"
	"time"
)

// Starts a fresh application from actual genesis parameters, then runs signed
// transactions through Apply (the same atomic transition used by ABCI).
func TestGenesisSignedTransactionDrill(t *testing.T) {
	fixture, actors, nodes, _, _ := pricedFixture(t)
	params := fixture.Parameters
	params.SubsidyCap = 0
	params.Misjudge.SlashMode = "refund_only"
	gen := GenesisState{Parameters: params, GenesisSubsidy: &GenesisSubsidy{OrganicYearEquivalents: SubsidyTotalEquivalents * 2, Entities: map[string]string{}}, Coordinators: []string{actors["coordinator"].id}, Auditors: []string{actors["auditor"].id, actors["auditor2"].id}}
	governors := []actor{actors["governor"]}
	for i := 1; i < SubsidyGovernors; i++ {
		who := newActor(byte(80 + i))
		governors = append(governors, who)
		fixture.Accounts[who.id] = &Account{}
	}
	for _, who := range governors {
		gen.Governors = append(gen.Governors, who.id)
	}
	bob := newActor(90)
	contributor := newActor(91)
	developer := newActor(92)
	for _, who := range []actor{bob, contributor, developer} {
		fixture.Accounts[who.id] = &Account{}
	}
	for _, id := range SortedAccountIDs(fixture) {
		a := fixture.Accounts[id]
		gen.Accounts = append(gen.Accounts, GenesisAccount{ID: id, Balance: a.Balance, Stake: a.Stake, Nonce: a.Nonce})
	}
	for _, m := range fixture.Models {
		gen.Models = append(gen.Models, m)
	}
	gen.GenesisSubsidy.Entities[contributor.id] = "supplier"
	gen.GenesisSubsidy.Entities[actors["payer"].id] = "consumer"
	for i := 0; i < 30; i++ {
		gen.GenesisSubsidy.TestnetRecords = append(gen.GenesisSubsidy.TestnetRecords, TestnetContribution{RequestID: fmt.Sprint(i), RecordRef: fmt.Sprint("source-", i), Payer: actors["payer"].id, Executor: contributor.id, Settled: true})
	}
	app := NewMemoryApplication()
	raw, err := json.Marshal(gen)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.InitChain(context.Background(), &abci.InitChainRequest{ChainId: params.ChainID, AppStateBytes: raw}); err != nil {
		t.Fatal(err)
	}
	s := app.committed
	for _, node := range nodes {
		registration := fixture.Nodes[node.id]
		apply(t, s, node, s.Accounts[node.id].Nonce+1, Action{Type: "register_node", Profile: registration.Profile, ExpiresHeight: registration.ExpiresHeight}, 1)
	}
	s.BlockHashes = fixture.BlockHashes
	s.Entropy = fixture.Entropy
	if err = s.advanceSubsidyTime(SubsidyDaySeconds); err != nil {
		t.Fatal(err)
	}
	send := func(who actor, a Action) { t.Helper(); apply(t, s, who, s.Accounts[who.id].Nonce+1, a, s.Height+1) }
	reject := func(who actor, a Action) { t.Helper(); rejected(t, s, who, a, s.Height+1) }
	payer := actors["payer"]
	reject(payer, Action{Type: "subsidy_claim_a"})
	for _, who := range []actor{payer, bob, contributor, developer} {
		send(who, Action{Type: "subsidy_notice", Notice: SubsidyNotice})
	}
	send(payer, Action{Type: "subsidy_claim_a"})
	send(bob, Action{Type: "subsidy_claim_a"})
	reject(payer, Action{Type: "subsidy_claim_a"})
	amount := SubsidyHouseholdEquivalents * SubsidyEquivalentSubunits
	reject(bob, Action{Type: "transfer", To: payer.id, Amount: 1})
	reject(bob, Action{Type: "stake", Amount: 1})
	execute := func(id string, input uint32) {
		t.Helper()
		c := &pb.ExecutionCommitment{RequestId: id, ChainId: "ember-unit-test", ProfileVer: "units-test", InputTokens: input, MaxOutputTokens: 1, TokenizerVersion: "unit-tokenizer-v1", InputCommitment: bytes.Repeat([]byte{1}, 32)}
		core.Sign(c, nodes[0].key)
		wire, _ := proto.Marshal(c)
		g := &pb.ChainGenesis{}
		proto.Unmarshal(s.Models[c.ChainId].Genesis, g)
		fee, _ := core.TokenFee(g, input, 1)
		send(payer, Action{Type: "freeze", HoldID: id, Payer: payer.id, Node: nodes[0].id, Amount: fee.Total, PricingCommitment: wire})
		r := &pb.ExecutionReceipt{}
		proto.Unmarshal(signedReceipt(t, id, nodes[0]), r)
		r.InputTokens = input
		ref := sha256.Sum256(wire)
		r.CommitmentRef = ref[:]
		core.Sign(r, nodes[0].key)
		receipt, _ := proto.Marshal(r)
		send(actors["coordinator"], Action{Type: "submit", HoldID: id, Actual: fee.Total, Receipt: receipt})
		send(actors["coordinator"], Action{Type: "assign_audit", HoldID: id})
		auditor, proof := assignedActor(t, s, actors, id)
		challenge, verdict, _ := signedAudit(t, receipt, auditor, "accept", proof)
		send(auditor, Action{Type: "audit", HoldID: id, Challenge: challenge, Verdict: verdict})
		send(actors["coordinator"], Action{Type: "settle", HoldID: id, Outcome: "accept"})
	}
	execute("short", SubsidyActivationInput-1)
	if s.GenesisSubsidy.Grants[payer.id].Activated {
		t.Fatal("short real inference activated")
	}
	execute("qualifying", SubsidyActivationInput)
	if s.Accounts[payer.id].ConsumptionCredit != amount {
		t.Fatal("qualifying settled inference did not activate")
	}
	send(payer, Action{Type: "transfer", To: bob.id, Amount: s.Accounts[payer.id].Balance}) // Isolate credit-only assertions through a real transfer.
	reject(payer, Action{Type: "transfer", To: bob.id, Amount: 1})
	reject(payer, Action{Type: "stake", Amount: 1})
	before := s.Accounts[nodes[0].id].Balance
	execute("subsidy-funded", SubsidyActivationInput)
	h := s.Holds["subsidy-funded"]
	if h.SubsidyDebit != h.Amount || s.Accounts[nodes[0].id].Balance-before != h.Actual || s.Accounts[nodes[0].id].ConsumptionCredit != 0 {
		t.Fatal("subsidy settlement did not produce clean executor Spark")
	}
	send(contributor, Action{Type: "subsidy_claim_b"})
	reject(contributor, Action{Type: "subsidy_claim_a"})
	reject(contributor, Action{Type: "subsidy_claim_b"})
	grantAmount := ratio(s.GenesisSubsidy.Total, SubsidyCDailyBPS, 10000)
	send(governors[0], Action{Type: "subsidy_c", Subsidy: &SubsidyAction{Op: "propose", ID: "app", To: developer.id, Amount: grantAmount}})
	for i := 0; i < SubsidyCApprovals-1; i++ {
		send(governors[i], Action{Type: "subsidy_c", Subsidy: &SubsidyAction{Op: "approve", ID: "app"}})
	}
	reject(developer, Action{Type: "subsidy_c", Subsidy: &SubsidyAction{Op: "issue", ID: "app"}})
	send(governors[SubsidyCApprovals-1], Action{Type: "subsidy_c", Subsidy: &SubsidyAction{Op: "approve", ID: "app"}})
	reject(developer, Action{Type: "subsidy_c", Subsidy: &SubsidyAction{Op: "issue", ID: "app"}})
	// An empty consensus block must reclaim the inactive A grant automatically.
	now := s.BlockTime + SubsidyActivationSeconds
	if _, err = app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: s.Height + 1, Time: time.Unix(now, 0), Hash: bytes.Repeat([]byte{3}, 32)}); err != nil {
		t.Fatal(err)
	}
	if _, err = app.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
		t.Fatal(err)
	}
	s = app.committed
	if !s.GenesisSubsidy.Grants[bob.id].Reclaimed || s.GenesisSubsidy.Channels["A"].Recovered != amount {
		t.Fatal("empty block did not reclaim A")
	}
	send(developer, Action{Type: "subsidy_c", Subsidy: &SubsidyAction{Op: "issue", ID: "app"}})
	send(developer, Action{Type: "subsidy_c", Subsidy: &SubsidyAction{Op: "distribute", ID: "app", To: bob.id, Amount: 100}})
	payer = bob
	execute("developer-terminal-activation", SubsidyActivationInput)
	if s.Accounts[bob.id].ConsumptionCredit != 100 {
		t.Fatal("C terminal grant did not require and activate through real settlement")
	}
	reject(bob, Action{Type: "transfer", To: SubsidyPoolAccount, Amount: 1})
	reject(governors[0], Action{Type: "set_subsidy_rate", Amount: 1})
	// C is at its independent daily limit. A/B spare capacity cannot help it.
	send(governors[0], Action{Type: "subsidy_c", Subsidy: &SubsidyAction{Op: "propose", ID: "second", To: developer.id, Amount: 1}})
	acknowledge(t, s, payer.id)
	if s.issueSubsidy("C", payer.id, 1, "over-cap", true) == nil {
		t.Fatal("C bypassed daily cap")
	}
	p := s.GenesisSubsidy
	var issued, recovered int64
	for _, c := range p.Channels {
		if c.Balance != c.Initial-c.Issued+c.Recovered {
			t.Fatal("channel reconciliation")
		}
		issued += c.Issued
		recovered += c.Recovered
	}
	if s.Accounts[p.Account].Balance != p.Total-issued+recovered {
		t.Fatal("pool reconciliation")
	}
	q, err := app.Query(context.Background(), &abci.QueryRequest{Path: "/genesis-subsidy"})
	if err != nil || q.Code != 0 || !bytes.Contains(q.Value, []byte("daily_issued")) {
		t.Fatal("read-only query", err, q)
	}
	t.Logf("genesis drill PASS: initial=%d issued=%d recovered=%d closing=%d", p.Total, issued, recovered, s.Accounts[p.Account].Balance)
}
