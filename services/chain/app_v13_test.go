package chain

import (
	"context"
	abci "github.com/cometbft/cometbft/abci/types"
	"path/filepath"
	"testing"
)

func TestParamActivationEmission(t *testing.T) {
	for _, from := range []uint64{11, 12, 13, 14} {
		s, _ := testState(t)
		s.Version = from
		app := &Application{committed: s}
		if err := app.ConfigureMembershipUpgrade(MembershipUpgrade{ChainID: s.Parameters.ChainID, ActivationHeight: 2}); err != nil {
			t.Fatal(err)
		}
		trial, err := s.blockCopy()
		if err != nil {
			t.Fatal(err)
		}
		if err = app.activateMembershipTrial(trial, 2); err != nil || trial.Version != from+1 {
			t.Fatalf("trial: %v version %d", err, trial.Version)
		}
		for h := int64(1); h <= 4; h++ {
			res, err := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: h})
			if err != nil {
				t.Fatal(err)
			}
			want := from
			if h >= 2 {
				want++
			}
			if app.staged.Version != want {
				t.Fatalf("height %d version %d", h, app.staged.Version)
			}
			u := res.ConsensusParamUpdates
			if want >= 13 {
				if u == nil || u.Version == nil || u.Version.App != want || u.Block == nil || u.Block.MaxBytes != 8388608 || u.Block.MaxGas != 10000000 {
					t.Fatalf("height %d update %v", h, u)
				}
			} else if h == 2 {
				if u == nil || u.Version.App != 12 || u.Block != nil {
					t.Fatalf("v12 activation update %v", u)
				}
			} else if u != nil {
				t.Fatalf("height %d unexpected update %v", h, u)
			}
			if _, err = app.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
				t.Fatal(err)
			}
		}
		// Reload without a manifest: v13 must keep emitting parameters after restart.
		path := filepath.Join(t.TempDir(), "state.json")
		if err := SaveState(path, app.committed); err != nil {
			t.Fatal(err)
		}
		cold, err := OpenApplication(path)
		if err != nil {
			t.Fatal(err)
		}
		res, err := cold.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: 5})
		if err != nil {
			t.Fatal(err)
		}
		if from == 12 && (res.ConsensusParamUpdates == nil || res.ConsensusParamUpdates.Block == nil) {
			t.Fatal("restart lost block update")
		}
	}
}

func TestParamMissedAndConsumed(t *testing.T) {
	s, _ := testState(t)
	s.Version = 12
	app := &Application{committed: s}
	u := MembershipUpgrade{ChainID: s.Parameters.ChainID, ActivationHeight: 2}
	if err := app.ConfigureMembershipUpgrade(u); err != nil {
		t.Fatal(err)
	}
	if _, err := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: 3}); err == nil || err.Error() != "v13 activation height missed" {
		t.Fatalf("missed: %v", err)
	}
	app.upgradeFromVersion = 11
	if _, err := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: 3}); err != nil || app.staged.Version != 12 {
		t.Fatalf("old rung: %v", err)
	}
	s.Height = 3
	app = &Application{committed: s}
	if err := app.ConfigureMembershipUpgrade(u); err != nil || app.upgrade != nil {
		t.Fatalf("consumed: %v", err)
	}
	if _, err := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: 4}); err != nil || app.staged.Version != 12 {
		t.Fatalf("consumed finalize: %v", err)
	}
}

func TestParamLoadVersions(t *testing.T) {
	// RC ruling: new chains remain at v11; loading and activating v13 stays supported.
	initial, _ := testState(t)
	if initial.Version != 11 {
		t.Fatalf("new state version %d, want 11", initial.Version)
	}
	for version := uint64(4); version <= 13; version++ {
		s, _ := testState(t)
		s.Version = version
		path := filepath.Join(t.TempDir(), "state.json")
		if err := SaveState(path, s); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadState(path); err != nil {
			t.Fatalf("version %d: %v", version, err)
		}
	}
}
