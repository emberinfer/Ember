package chain

import (
	"fmt"
	abci "github.com/cometbft/cometbft/abci/types"
	"testing"
)

func TestConsumedUpgradeDecisionTable(t *testing.T) {
	// Cross versions, membership content, chain identity, and past/equal/future heights.
	for version := uint64(4); version <= 12; version++ {
		for _, membership := range []bool{false, true} {
			for _, matching := range []bool{false, true} {
				for _, height := range []int64{19, 20, 21} {
					t.Run(fmt.Sprintf("v%d/membership=%t/matching=%t/activation=%d", version, membership, matching, height), func(t *testing.T) {
						s, _, u, _ := membershipInitFixture(t)
						s.Version = version
						s.Height = 20
						u.ActivationHeight = height
						if membership {
							s.Membership = &Membership{}
						} else {
							u.MembershipInit = nil
						}
						if !matching {
							u.ChainID += "-wrong"
						}
						app := &Application{committed: s}
						err := app.ConfigureMembershipUpgrade(u)
						consumed := matching && height <= s.Height && version >= 6 && version != 10
						armed := matching && height > s.Height && version >= 5 && version <= 11
						if (err == nil) != (consumed || armed) {
							t.Fatalf("err=%v consumed=%t armed=%t", err, consumed, armed)
						}
						if (app.upgrade != nil) != armed {
							t.Fatalf("upgrade=%v armed=%t", app.upgrade, armed)
						}
						if armed && app.upgradeFromVersion != version {
							t.Fatalf("source=%d want=%d", app.upgradeFromVersion, version)
						}
						if !armed && app.upgradeFromVersion != 0 {
							t.Fatal("ignored/rejected manifest armed source version")
						}
					})
				}
			}
		}
	}
}

func TestConsumedPureUpgradeRestart(t *testing.T) {
	s, _, u, _ := membershipInitFixture(t)
	s.Version = 11
	s.Membership = nil
	u.MembershipInit = nil
	app := &Application{committed: s}
	if err := app.ConfigureMembershipUpgrade(u); err != nil {
		t.Fatal(err)
	}
	if app.upgrade == nil || app.upgradeFromVersion != 11 {
		t.Fatal("not armed from v11")
	}
	if err := app.activateMembership(s, &abci.FinalizeBlockRequest{Height: u.ActivationHeight - 1}); err != nil || s.Version != 11 {
		t.Fatal("early activation", err)
	}
	if err := app.activateMembership(s, &abci.FinalizeBlockRequest{Height: u.ActivationHeight}); err != nil || s.Version != 12 {
		t.Fatal("activation", err)
	}
	s.Height = u.ActivationHeight
	before, _ := s.Bytes()
	if err := app.activateMembership(s, &abci.FinalizeBlockRequest{Height: u.ActivationHeight + 1}); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Bytes()
	if string(before) != string(after) {
		t.Fatal("second activation changed state")
	}
	restarted := &Application{committed: s}
	if err := restarted.ConfigureMembershipUpgrade(u); err != nil {
		t.Fatal("restart", err)
	}
	if restarted.upgrade != nil || restarted.upgradeFromVersion != 0 {
		t.Fatal("restart rearmed consumed manifest")
	}
}
