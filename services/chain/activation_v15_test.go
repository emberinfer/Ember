package chain

import (
	"context"
	abci "github.com/cometbft/cometbft/abci/types"
	"testing"
)

func TestV15ActivationMissedAndWrongRung(t *testing.T) {
	s, _ := testState(t)
	s.Version = 14
	app := &Application{committed: s}
	u := MembershipUpgrade{ChainID: s.Parameters.ChainID, ActivationHeight: 2}
	if err := app.ConfigureMembershipUpgrade(u); err != nil {
		t.Fatal(err)
	}
	if _, err := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: 3}); err == nil || err.Error() != "v15 activation height missed" {
		t.Fatalf("missed: %v", err)
	}
	app.upgradeFromVersion = 13
	if _, err := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: 3}); err != nil || app.staged.Version != 14 {
		t.Fatalf("old rung: %v", err)
	}
	s.Version, s.Height = 15, 3
	cold := &Application{committed: s}
	if err := cold.ConfigureMembershipUpgrade(u); err != nil || cold.upgrade != nil {
		t.Fatalf("consumed: %v", err)
	}
	if _, err := cold.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: 4}); err != nil || cold.staged.Version != 15 {
		t.Fatalf("restart: %v", err)
	}
}
