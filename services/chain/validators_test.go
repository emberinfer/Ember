package chain

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	cmtproto "github.com/cometbft/cometbft/api/cometbft/types/v1"
	cmted "github.com/cometbft/cometbft/crypto/ed25519"
)

func membershipFixture(t *testing.T, n int) (*State, []actor) {
	s, _ := testState(t)
	actors := []actor{}
	m := &Membership{ChangeDelayBlocks: 5, ChangeWindowBlocks: 10, ReadinessBlocks: 20, MaxValidators: 10, Registry: map[string]*Validator{}, Ready: map[string]int64{}, Reachable: map[string]bool{}, FloorReached: n >= 3}
	for i := 0; i < 5; i++ {
		a := newActor(byte(40 + i))
		actors = append(actors, a)
		s.Accounts[a.id] = &Account{Stake: 1000}
		s.Nodes[a.id] = &NodeRegistration{ExpiresHeight: 1000}
		if i < n {
			v := &Validator{PubKey: a.key.Public().(ed25519.PublicKey), Operator: a.id, OrgID: "org", Power: 10, Status: "active"}
			id := validatorID(v.PubKey)
			m.Registry[id] = v
			m.Reachable[id] = true
		}
	}
	s.Membership = m
	s.Height = 2
	return s, actors
}
func joinCandidate(t *testing.T, s *State, a actor) {
	pub := a.key.Public().(ed25519.PublicKey)
	ready := &ValidatorReady{PubKey: pub, Height: 2, Signature: ed25519.Sign(a.key, ReadySignBytes(s.Parameters.ChainID, a.id, pub, 2, s.BlockHashes[2]))}
	apply(t, s, a, 1, Action{Type: "validator_register", Ready: ready, Validator: &Validator{PubKey: pub, Operator: a.id, OrgID: "new-org", Power: 10, Status: "pending"}}, 3)
	apply(t, s, a, 2, Action{Type: "validator_ready", Ready: ready}, 4)
}
func rejectAction(t *testing.T, s *State, a actor, action Action, h int64, contains string) {
	t.Helper()
	before, _ := s.Bytes()
	raw, _ := Sign(s.Parameters.ChainID, a.key, s.Accounts[a.id].Nonce+1, action)
	_, err := s.Apply(raw, h)
	if err == nil || !strings.Contains(err.Error(), contains) {
		t.Fatalf("want rejection %q got %v", contains, err)
	}
	after, _ := s.Bytes()
	if !bytes.Equal(before, after) {
		t.Fatal("failed tx mutated state")
	}
}
func TestMembershipJoinExitAndFloor(t *testing.T) {
	s, as := membershipFixture(t, 3)
	joinCandidate(t, s, as[3])
	v := *s.Membership.Registry[validatorID(as[3].key.Public().(ed25519.PublicKey))]
	c := &ValidatorSetChange{Add: []Validator{v}, EffectiveHeight: 10}
	rejectAction(t, s, as[3], Action{Type: "validator_set_change", SetChange: c}, 5, "active validator")
	apply(t, s, as[0], 1, Action{Type: "validator_set_change", SetChange: c}, 5)
	if got := s.finalizeMembership(7); len(got) != 0 {
		t.Fatal("early update")
	}
	if got := s.finalizeMembership(8); len(got) != 1 || got[0].Power != 10 {
		t.Fatal("missing ABCI join update")
	}
	if s.Membership.Registry[validatorID(v.PubKey)].Status != "pending" {
		t.Fatal("effective early")
	}
	s.finalizeMembership(10)
	s.Height = 10
	if len(s.Membership.active()) != 4 {
		t.Fatal("join not applied")
	}
	c = &ValidatorSetChange{Remove: []string{validatorID(v.PubKey)}, EffectiveHeight: 20}
	rejectAction(t, s, as[3], Action{Type: "validator_set_change", SetChange: c}, 11, "rate limited")
	for id := range s.Membership.active() {
		s.Membership.Reachable[id] = true
	}
	apply(t, s, as[3], 3, Action{Type: "validator_set_change", SetChange: c}, 15)
	rejectAction(t, s, as[3], Action{Type: "unbond", Amount: 100}, 16, "locks stake")
	updates := s.finalizeMembership(18)
	if len(updates) != 1 || updates[0].Power != 0 {
		t.Fatal("missing exit update")
	}
	s.finalizeMembership(20)
	s.Height = 20
	apply(t, s, as[3], 4, Action{Type: "unbond", Amount: 100}, 21)
	if s.Accounts[as[3].id].Unbondings[0].ReleaseHeight != 24 {
		t.Fatal("existing unbond delay lost")
	}
	c = &ValidatorSetChange{Remove: []string{validatorID(as[0].key.Public().(ed25519.PublicKey))}, EffectiveHeight: 30}
	rejectAction(t, s, as[0], Action{Type: "validator_set_change", SetChange: c}, 25, "floor")
}
func TestMembershipQuorumFailClosed(t *testing.T) {
	s, as := membershipFixture(t, 4)
	id := validatorID(as[0].key.Public().(ed25519.PublicKey))
	c := &ValidatorSetChange{Remove: []string{id}, EffectiveHeight: 10}
	// One absent validator among survivors makes new quorum 2/3: reject.
	s.Membership.Reachable[validatorID(as[1].key.Public().(ed25519.PublicKey))] = false
	rejectAction(t, s, as[0], Action{Type: "validator_set_change", SetChange: c}, 5, "new=2/3")
	s.Membership.Reachable[validatorID(as[1].key.Public().(ed25519.PublicKey))] = true
	apply(t, s, as[0], 1, Action{Type: "validator_set_change", SetChange: c}, 5)
	s.Membership.Reachable[validatorID(as[2].key.Public().(ed25519.PublicKey))] = false
	if updates := s.finalizeMembership(8); len(updates) != 0 || s.Membership.Pending != nil || s.Membership.Registry[id].Status != "active" || s.Membership.History[0].Status != "rejected" {
		t.Fatal("unsafe transition was not rejected")
	}
}
func TestMembershipReadinessAndAdmission(t *testing.T) {
	s, as := membershipFixture(t, 3)
	joinCandidate(t, s, as[3])
	id := validatorID(as[3].key.Public().(ed25519.PublicKey))
	v := *s.Membership.Registry[id]
	c := &ValidatorSetChange{Add: []Validator{v}, EffectiveHeight: 10}
	delete(s.Membership.Ready, id)
	rejectAction(t, s, as[0], Action{Type: "validator_set_change", SetChange: c}, 5, "readiness")
	s.Membership.Ready[id] = 2
	s.Nodes[as[3].id].ExpiresHeight = 5
	rejectAction(t, s, as[0], Action{Type: "validator_set_change", SetChange: c}, 5, "signed admission")
}
func TestMembershipUpgradePreservesLegacyUntilActivation(t *testing.T) {
	s, as := membershipFixture(t, 2)
	m := s.Membership
	s.Membership = nil
	s.Version = 4
	s.Height = 5
	path := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(path, s); err != nil {
		t.Fatal(err)
	}
	app, err := OpenApplication(path)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := app.committed.Hash()
	u := MembershipUpgrade{ChainID: s.Parameters.ChainID, ActivationHeight: 8, Membership: *m}
	if err = app.ConfigureMembershipUpgrade(u); err != nil {
		t.Fatal(err)
	}
	after, _ := app.committed.Hash()
	if !bytes.Equal(before, after) {
		t.Fatal("manifest changed legacy hash")
	}
	ctx := context.Background()
	info, _ := app.Info(ctx, &abci.InfoRequest{})
	if info.AppVersion != 4 {
		t.Fatal("reported early activation")
	}
	votes := []abci.VoteInfo{}
	for _, a := range as[:2] {
		votes = append(votes, abci.VoteInfo{Validator: abci.Validator{Address: cmted.PubKey(a.key.Public().(ed25519.PublicKey)).Address(), Power: 10}, BlockIdFlag: cmtproto.BlockIDFlagCommit})
	}
	for h := int64(6); h <= 8; h++ {
		_, err = app.FinalizeBlock(ctx, &abci.FinalizeBlockRequest{Height: h, DecidedLastCommit: abci.CommitInfo{Votes: votes}})
		if err != nil {
			t.Fatal(err)
		}
		app.Commit(ctx, &abci.CommitRequest{})
		if h < 8 && app.committed.Version != 4 {
			t.Fatal("early version migration")
		}
	}
	// The combined membership+liveness manifest lands a legacy chain on
	// version 6; reaching AppVersion (7) takes the subsequent 6 -> 7 manifest.
	if app.committed.Version != 6 || app.committed.Membership == nil {
		t.Fatal("activation missing")
	}
	restarted, err := OpenApplication(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(restarted.committed)
	if !bytes.Contains(raw, []byte(`"membership"`)) {
		t.Fatal("membership not persisted")
	}
}

func TestMembershipRegistrationProofAndCancellation(t *testing.T) {
	s, as := membershipFixture(t, 3)
	a := as[3]
	pub := a.key.Public().(ed25519.PublicKey)
	v := &Validator{PubKey: pub, Operator: a.id, OrgID: "org", Power: 10, Status: "pending"}
	rejectAction(t, s, a, Action{Type: "validator_register", Validator: v}, 3, "readiness proof")
	bad := &ValidatorReady{PubKey: pub, Height: 2, Signature: ed25519.Sign(as[4].key, ReadySignBytes(s.Parameters.ChainID, a.id, pub, 2, s.BlockHashes[2]))}
	rejectAction(t, s, a, Action{Type: "validator_register", Validator: v, Ready: bad}, 3, "readiness proof")
	joinCandidate(t, s, a)
	apply(t, s, a, 3, Action{Type: "validator_cancel", Validator: v}, 5)
	apply(t, s, a, 4, Action{Type: "unbond", Amount: 100}, 6)
	if s.Membership.Registry[validatorID(pub)].Status != "exited" {
		t.Fatal("cancelled admission not retained in history")
	}
}
func TestMembershipAnnouncementRenewsShortAdmission(t *testing.T) {
	s, as := membershipFixture(t, 3)
	joinCandidate(t, s, as[3])
	m := s.Membership
	m.ChangeDelayBlocks = 100
	m.ChangeWindowBlocks = 102
	v := *m.Registry[validatorID(as[3].key.Public().(ed25519.PublicKey))]
	s.Nodes[as[3].id].ExpiresHeight = 20
	// A two-week announcement must not require a two-week registration TTL.
	apply(t, s, as[0], 1, Action{Type: "validator_set_change", SetChange: &ValidatorSetChange{Add: []Validator{v}, EffectiveHeight: 105}}, 5)
	rejectAction(t, s, as[3], Action{Type: "validator_cancel", Validator: &v}, 6, "proposed join")
	// No refreshed readiness at execution: reject, even though 3/4 alone is a quorum.
	if len(s.finalizeMembership(103)) != 0 || s.Membership.Pending != nil {
		t.Fatal("stale readiness did not abort")
	}
	if s.Membership.History[0].Status != "rejected" {
		t.Fatal("missing failed change history")
	}
}
func TestMembershipExpiredAdmissionAtExecution(t *testing.T) {
	s, as := membershipFixture(t, 3)
	joinCandidate(t, s, as[3])
	v := *s.Membership.Registry[validatorID(as[3].key.Public().(ed25519.PublicKey))]
	s.Nodes[as[3].id].ExpiresHeight = 9
	apply(t, s, as[0], 1, Action{Type: "validator_set_change", SetChange: &ValidatorSetChange{Add: []Validator{v}, EffectiveHeight: 10}}, 5)
	if updates := s.finalizeMembership(8); len(updates) != 0 || s.Membership.Pending != nil || !strings.Contains(s.Membership.History[0].Reason, "admission expired") {
		t.Fatal("candidate admission lapse was not rejected")
	}
}
func TestMembershipHistoricalReplayAndConsensusVersion(t *testing.T) {
	s, as := membershipFixture(t, 2)
	m := s.Membership
	gen := GenesisState{Parameters: s.Parameters}
	for id, a := range s.Accounts {
		gen.Accounts = append(gen.Accounts, GenesisAccount{ID: id, Balance: a.Balance, Stake: a.Stake})
	}
	raw, _ := json.Marshal(gen)
	app := NewMemoryApplication()
	if err := app.ConfigureMembershipUpgrade(MembershipUpgrade{ChainID: s.Parameters.ChainID, ActivationHeight: 3, Membership: *m}); err != nil {
		t.Fatal(err)
	}
	info, _ := app.Info(context.Background(), &abci.InfoRequest{})
	if info.AppVersion != 4 {
		t.Fatal("empty observer announced wrong historical version")
	}
	if _, err := app.InitChain(context.Background(), &abci.InitChainRequest{ChainId: s.Parameters.ChainID, AppStateBytes: raw}); err != nil {
		t.Fatal(err)
	}
	votes := []abci.VoteInfo{}
	for _, a := range as[:2] {
		votes = append(votes, abci.VoteInfo{Validator: abci.Validator{Address: cmted.PubKey(a.key.Public().(ed25519.PublicKey)).Address(), Power: 10}, BlockIdFlag: cmtproto.BlockIDFlagCommit})
	}
	for h := int64(1); h <= 3; h++ {
		res, err := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: h, DecidedLastCommit: abci.CommitInfo{Votes: votes}})
		if err != nil {
			t.Fatal(err)
		}
		// Historical replay must reproduce the activation as it happened:
		// the combined membership+liveness manifest landed on version 6.
		// Reaching AppVersion (7) is the subsequent 6 -> 7 manifest's job.
		if h == 3 && (res.ConsensusParamUpdates == nil || res.ConsensusParamUpdates.Version.App != 6) {
			t.Fatal("consensus app version did not activate")
		}
		app.Commit(context.Background(), &abci.CommitRequest{})
	}
	if err := app.ConfigureMembershipUpgrade(MembershipUpgrade{ChainID: s.Parameters.ChainID, ActivationHeight: 3}); err != nil {
		t.Fatal("post-activation restart", err)
	}
}
func TestMembershipActivationRejectsMismatchedConsensusSet(t *testing.T) {
	s, as := membershipFixture(t, 2)
	m := s.Membership
	s.Membership = nil
	s.Version = 4
	app := NewMemoryApplication()
	app.committed = s
	if err := app.ConfigureMembershipUpgrade(MembershipUpgrade{ChainID: s.Parameters.ChainID, ActivationHeight: 3, Membership: *m}); err != nil {
		t.Fatal(err)
	}
	votes := []abci.VoteInfo{}
	for _, a := range []actor{as[0], as[2]} {
		votes = append(votes, abci.VoteInfo{Validator: abci.Validator{Address: cmted.PubKey(a.key.Public().(ed25519.PublicKey)).Address(), Power: 10}, BlockIdFlag: cmtproto.BlockIDFlagCommit})
	}
	_, err := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: 3, DecidedLastCommit: abci.CommitInfo{Votes: votes}})
	if err == nil || app.committed.Version != 4 || app.staged != nil {
		t.Fatal("mismatched manifest did not fail closed")
	}
}
