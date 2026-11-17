package chain

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
)

func TestMoeEightToNineConfigure(t *testing.T) {
	s, _ := testState(t)
	s.Version, s.Height = 8, 9
	on := true
	app := &Application{committed: s}
	u := MembershipUpgrade{ChainID: s.Parameters.ChainID, ActivationHeight: 10, MoeEnabled: &on}
	if err := app.ConfigureMembershipUpgrade(u); err != nil {
		t.Fatal(err)
	}
	if app.upgrade == nil || !*app.upgrade.MoeEnabled {
		t.Fatal("manifest not staged")
	}
	u.ActivationHeight = 8
	restarted := &Application{committed: s}
	if err := restarted.ConfigureMembershipUpgrade(u); err != nil || restarted.upgrade != nil {
		t.Fatal("consumed manifest was not ignored", err)
	}
	u.ActivationHeight = 10
	u.ChainID = "wrong-chain"
	if err := app.ConfigureMembershipUpgrade(u); err == nil {
		t.Fatal("accepted wrong chain")
	}
}

func TestMoeEightToNineActivation(t *testing.T) {
	s, _ := testState(t)
	s.Version, s.Height = 8, 8
	s.Parameters.MoeEnabled = false
	on := true
	p := s.Parameters.AuditSampling
	p.defaults()
	p.Enabled = true
	app := &Application{committed: s}
	if err := app.ConfigureMembershipUpgrade(MembershipUpgrade{ChainID: s.Parameters.ChainID, ActivationHeight: 10, MoeEnabled: &on, AuditSampling: &p}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Hash()
	if err := app.activateMembership(s, &abci.FinalizeBlockRequest{Height: 9}); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Hash()
	if !bytes.Equal(before, after) {
		t.Fatal("state changed before activation")
	}
	if err := app.activateMembership(s, &abci.FinalizeBlockRequest{Height: 11}); err == nil || !strings.Contains(err.Error(), "moe enablement activation height missed") {
		t.Fatalf("missed activation: %v", err)
	}
	res, err := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: 10})
	if err != nil {
		t.Fatal(err)
	}
	if app.staged.Version != 9 || !app.staged.Parameters.MoeEnabled || app.staged.Parameters.AuditSampling != p {
		t.Fatal("migration/manifest not applied")
	}
	if res.ConsensusParamUpdates == nil || res.ConsensusParamUpdates.Version.App != 9 {
		t.Fatal("missing consensus version update")
	}
}

func TestMoeEightStateRoundTripWithoutUpgrade(t *testing.T) {
	s, _ := testState(t)
	s.Version = 8
	path := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(path, s); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadStateWithUpgrade(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Version != 8 {
		t.Fatal("load migrated state")
	}
	want, err := s.Hash()
	if err != nil {
		t.Fatal(err)
	}
	got, err := loaded.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, got) {
		t.Fatal("round-trip hash changed")
	}
	app := &Application{}
	if err := app.activateMembership(loaded, &abci.FinalizeBlockRequest{Height: 10}); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(path, loaded); err != nil {
		t.Fatal(err)
	}
	roundTrip, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, roundTrip) {
		t.Fatal("round-trip bytes changed")
	}
}

func TestMoeEightToNineOptionalParameters(t *testing.T) {
	s, _ := testState(t)
	s.Version = 8
	s.Parameters.MoeEnabled = true
	p := s.Parameters.AuditSampling
	s.migrateMoeEnablement(MembershipUpgrade{})
	if s.Version != 9 || !s.Parameters.MoeEnabled || s.Parameters.AuditSampling != p {
		t.Fatal("absent parameters changed")
	}
	off := false
	s.migrateMoeEnablement(MembershipUpgrade{MoeEnabled: &off})
	if s.Parameters.MoeEnabled {
		t.Fatal("explicit false ignored")
	}
}

// Proposal validation must see the same enablement gate as FinalizeBlock.
func TestMoeEightToNineProposalBoundary(t *testing.T) {
	f := moeChainState(t)
	f.s.Version, f.s.Height = 8, 1
	f.s.Parameters.MoeEnabled = false
	on := true
	app := &Application{committed: f.s}
	if err := app.ConfigureMembershipUpgrade(MembershipUpgrade{ChainID: f.s.Parameters.ChainID, ActivationHeight: 2, MoeEnabled: &on}); err != nil {
		t.Fatal(err)
	}
	raw, err := Sign(f.s.Parameters.ChainID, f.a["payer"].key, 1, Action{Type: "freeze_moe", HoldID: "moe-req", Payer: f.a["payer"].id, Amount: 1_000_000, MoeInputTokens: 3, MoeMaxTokens: 2, MoeAuthorizations: f.authorizations()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	check, err := app.CheckTx(ctx, &abci.CheckTxRequest{Tx: raw})
	if err != nil || check.Code != 0 {
		t.Fatalf("CheckTx: %v %v", check, err)
	}
	prepared, err := app.PrepareProposal(ctx, &abci.PrepareProposalRequest{Height: 2, Txs: [][]byte{raw}, MaxTxBytes: 1 << 20})
	if err != nil || len(prepared.Txs) != 1 {
		t.Fatalf("PrepareProposal: %v %v", prepared, err)
	}
	processed, err := app.ProcessProposal(ctx, &abci.ProcessProposalRequest{Height: 2, Txs: prepared.Txs})
	if err != nil || processed.Status != abci.PROCESS_PROPOSAL_STATUS_ACCEPT {
		t.Fatalf("ProcessProposal: %v %v", processed, err)
	}
	if _, err := app.FinalizeBlock(ctx, &abci.FinalizeBlockRequest{Height: 2, Txs: prepared.Txs}); err != nil {
		t.Fatal(err)
	}
	if app.staged.Holds["moe-req"] == nil || app.staged.Version != 9 {
		t.Fatal("activation transaction not finalized")
	}
	if app.committed.Version != 8 || app.committed.Parameters.MoeEnabled {
		t.Fatal("trial changed committed state")
	}
}
