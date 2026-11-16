package chain

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	cmtproto "github.com/cometbft/cometbft/api/cometbft/types/v1"
	cmted "github.com/cometbft/cometbft/crypto/ed25519"
)

func membershipInitFixture(t *testing.T) (*State, []actor, MembershipUpgrade, *abci.FinalizeBlockRequest) {
	t.Helper()
	s, actors := membershipFixture(t, 4)
	s.Version, s.Membership = 10, nil
	init := &MembershipInit{ChangeDelayBlocks: 5, ChangeWindowBlocks: 10, ReadinessBlocks: 20, MaxValidators: 10, Operators: map[string]string{}, OrgIDs: map[string]string{}}
	req := &abci.FinalizeBlockRequest{Height: 3}
	for _, a := range actors[:4] {
		pub := a.key.Public().(ed25519.PublicKey)
		id := validatorID(pub)
		init.Operators[id], init.OrgIDs[id] = a.id, "org-"+id
		s.Accounts[a.id].Balance = 1000
		req.DecidedLastCommit.Votes = append(req.DecidedLastCommit.Votes, abci.VoteInfo{Validator: abci.Validator{Address: cmted.PubKey(pub).Address(), Power: EqualValidatorPower}, BlockIdFlag: cmtproto.BlockIDFlagCommit})
	}
	return s, actors, MembershipUpgrade{ChainID: s.Parameters.ChainID, ActivationHeight: 3, MembershipInit: init}, req
}

func TestMembershipInitActivationAndF6(t *testing.T) {
	s, actors, u, req := membershipInitFixture(t)
	rejectAction(t, s, actors[4], Action{Type: "validator_register", Validator: &Validator{}}, 3, "validator dynamics not activated")
	app := &Application{committed: s}
	if err := app.ConfigureMembershipUpgrade(u); err != nil {
		t.Fatal(err)
	}
	early := *req
	early.Height = 2
	if err := app.activateMembership(s, &early); err != nil || s.Version != 10 || s.Membership != nil {
		t.Fatal("early activation", err)
	}
	// Exercise the real FinalizeBlock path, including vote observation and consensus version update.
	res, err := app.FinalizeBlock(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	s = app.staged
	if s.Version != 11 || s.Membership == nil || !s.Membership.FloorReached || res.ConsensusParamUpdates.Version.App != 11 {
		t.Fatal("missing activation")
	}
	m := s.Membership
	if len(m.Registry) != 4 || m.ChangeDelayBlocks != 5 || m.ChangeWindowBlocks != 10 || m.ReadinessBlocks != 20 || m.MaxValidators != 10 {
		t.Fatal("incorrect params or registry")
	}
	for id, op := range u.MembershipInit.Operators {
		v := m.Registry[id]
		if v == nil || v.Operator != op || v.OrgID != u.MembershipInit.OrgIDs[id] || validatorID(v.PubKey) != id || v.Power != 10 || v.Status != "active" {
			t.Fatal("incorrect validator")
		}
	}
	a := actors[4]
	s.Accounts[a.id].Balance = 1000
	s.Accounts[a.id].Stake = s.Parameters.MinNodeStake
	s.Nodes[a.id].ExpiresHeight = 1000
	pub := a.key.Public().(ed25519.PublicKey)
	ready := &ValidatorReady{PubKey: pub, Height: 2, Signature: ed25519.Sign(a.key, ReadySignBytes(s.Parameters.ChainID, a.id, pub, 2, s.BlockHashes[2]))}
	apply(t, s, a, 1, Action{Type: "validator_register", Ready: ready, Validator: &Validator{PubKey: pub, Operator: a.id, OrgID: "candidate", Power: 10, Status: "pending"}}, 4)
	v := *s.Membership.Registry[validatorID(pub)]
	if v.Status != "pending" {
		t.Fatal("candidate not pending")
	}
	apply(t, s, actors[0], 1, Action{Type: "validator_set_change", SetChange: &ValidatorSetChange{Add: []Validator{v}, EffectiveHeight: 10}}, 5)
	if s.Membership.Pending == nil || len(s.Membership.Pending.Add) != 1 || s.Membership.Pending.EffectiveHeight != 10 {
		t.Fatal("missing proposed change")
	}
}

func TestMembershipInitActivationRejectsMismatch(t *testing.T) {
	for _, name := range []string{"missing", "extra", "unknown operator", "duplicate operator", "power", "duplicate vote", "unknown vote", "missed height"} {
		t.Run(name, func(t *testing.T) {
			s, actors, u, req := membershipInitFixture(t)
			app := &Application{committed: s}
			if err := app.ConfigureMembershipUpgrade(u); err != nil {
				t.Fatal(err)
			}
			// Mutate the detached armed manifest to also verify activation revalidation.
			init := app.upgrade.MembershipInit
			id := validatorID(actors[0].key.Public().(ed25519.PublicKey))
			switch name {
			case "missing":
				delete(init.Operators, id)
			case "extra":
				init.Operators[validatorID(actors[4].key.Public().(ed25519.PublicKey))] = actors[4].id
			case "unknown operator":
				init.Operators[id] = "missing-account"
			case "duplicate operator":
				init.Operators[id] = actors[1].id
			case "power":
				req.DecidedLastCommit.Votes[0].Validator.Power = 11
			case "duplicate vote":
				req.DecidedLastCommit.Votes[0] = req.DecidedLastCommit.Votes[1]
			case "unknown vote":
				req.DecidedLastCommit.Votes[0].Validator.Address = cmted.PubKey(actors[4].key.Public().(ed25519.PublicKey)).Address()
			case "missed height":
				req.Height++
			}
			before, _ := s.Bytes()
			if err := app.activateMembership(s, req); err == nil {
				t.Fatal("accepted invalid activation")
			}
			after, _ := s.Bytes()
			if s.Membership != nil || s.Version != 10 || !bytes.Equal(before, after) {
				t.Fatal("failed activation mutated state")
			}
		})
	}
}

func TestMembershipInitArmValidation(t *testing.T) {
	for _, name := range []string{"height", "chain", "delay", "window", "readiness", "max", "size", "hex", "key length", "operator", "duplicate operator"} {
		t.Run(name, func(t *testing.T) {
			s, actors, u, _ := membershipInitFixture(t)
			id := validatorID(actors[0].key.Public().(ed25519.PublicKey))
			switch name {
			case "height":
				u.ActivationHeight = s.Height
			case "chain":
				u.ChainID += "wrong"
			case "delay":
				u.MembershipInit.ChangeDelayBlocks = 2
			case "window":
				u.MembershipInit.ChangeWindowBlocks = 6
			case "readiness":
				u.MembershipInit.ReadinessBlocks = 1001
			case "max":
				u.MembershipInit.MaxValidators = 2
			case "size":
				u.MembershipInit.Operators = map[string]string{id: actors[0].id}
			case "hex":
				u.MembershipInit.Operators["zz"] = actors[4].id
			case "key length":
				u.MembershipInit.Operators["ab"] = actors[4].id
			case "operator":
				u.MembershipInit.Operators[id] = "unknown"
			case "duplicate operator":
				u.MembershipInit.Operators[id] = actors[1].id
			}
			app := &Application{committed: s}
			before, _ := s.Bytes()
			if err := app.ConfigureMembershipUpgrade(u); err == nil {
				t.Fatal("accepted invalid manifest")
			}
			after, _ := s.Bytes()
			if !bytes.Equal(before, after) || app.upgrade != nil {
				t.Fatal("arming mutated state")
			}
		})
	}
}

func TestMembershipInitNoInitAndDefaultOrg(t *testing.T) {
	for _, withInit := range []bool{false, true} {
		s, _, u, req := membershipInitFixture(t)
		if withInit {
			u.MembershipInit.OrgIDs = nil
		} else {
			u.MembershipInit = nil
		}
		app := &Application{committed: s}
		if err := app.ConfigureMembershipUpgrade(u); err != nil {
			t.Fatal(err)
		}
		if err := app.activateMembership(s, req); err != nil {
			t.Fatal(err)
		}
		if s.Version != 11 || (s.Membership != nil) != withInit {
			t.Fatal("incorrect no-init behavior")
		}
		if withInit {
			for id, v := range s.Membership.Registry {
				if v.OrgID != "validator-"+id {
					t.Fatal("incorrect default org")
				}
			}
		}
	}
}

func TestMembershipInitV10RoundTrip(t *testing.T) {
	s, _, u, _ := membershipInitFixture(t)
	path := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(path, s); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, upgrade := range []*MembershipUpgrade{nil, &u} {
		app, err := OpenApplicationWithUpgrade(path, upgrade)
		if err != nil {
			t.Fatal(err)
		}
		if err := SaveState(path, app.committed); err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("v10 bytes changed before activation")
		}
	}
}

func TestMembershipInitABCITrials(t *testing.T) {
	ctx := context.Background()
	s, actors, u, votes := membershipInitFixture(t)
	s.Height = 1
	app := &Application{committed: s}
	if err := app.ConfigureMembershipUpgrade(u); err != nil {
		t.Fatal(err)
	}
	for height := int64(2); height <= 4; height++ {
		raw, err := Sign(s.Parameters.ChainID, actors[0].key, app.committed.Accounts[actors[0].id].Nonce+1, Action{Type: "transfer", To: actors[1].id, Amount: 1})
		if err != nil {
			t.Fatal(err)
		}
		check, err := app.CheckTx(ctx, &abci.CheckTxRequest{Tx: raw})
		if err != nil || check.Code != 0 {
			t.Fatalf("height %d CheckTx: %v %v", height, check, err)
		}
		prepared, err := app.PrepareProposal(ctx, &abci.PrepareProposalRequest{Height: height, Txs: [][]byte{raw}, MaxTxBytes: 100000})
		if err != nil || len(prepared.Txs) != 1 {
			t.Fatalf("height %d prepare: %v %v", height, prepared, err)
		}
		processed, err := app.ProcessProposal(ctx, &abci.ProcessProposalRequest{Height: height, Txs: prepared.Txs})
		if err != nil || processed.Status != abci.PROCESS_PROPOSAL_STATUS_ACCEPT {
			t.Fatalf("height %d process: %v %v", height, processed, err)
		}
		if height == 3 {
			if app.committed.Version != 10 || app.committed.Membership != nil {
				t.Fatal("trial changed committed state")
			}
			for _, mutation := range []string{"missing", "extra", "wrong"} {
				bad := *votes
				bad.DecidedLastCommit.Votes = append([]abci.VoteInfo(nil), votes.DecidedLastCommit.Votes...)
				switch mutation {
				case "missing":
					bad.DecidedLastCommit.Votes = bad.DecidedLastCommit.Votes[1:]
				case "extra":
					bad.DecidedLastCommit.Votes = append(bad.DecidedLastCommit.Votes, bad.DecidedLastCommit.Votes[0])
				case "wrong":
					bad.DecidedLastCommit.Votes[0].Validator.Power++
				}
				if _, err := app.FinalizeBlock(ctx, &bad); err == nil || app.staged != nil {
					t.Fatalf("accepted %s live votes", mutation)
				}
			}
		}
		req := *votes
		req.Height = height
		req.Txs = prepared.Txs
		finalized, err := app.FinalizeBlock(ctx, &req)
		if err != nil || finalized.TxResults[0].Code != 0 {
			t.Fatalf("height %d finalize: %v %v", height, finalized, err)
		}
		if _, err = app.Commit(ctx, &abci.CommitRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	if app.committed.Version != 11 || len(app.committed.Membership.Registry) != 4 {
		t.Fatal("membership not installed")
	}
}

func TestMembershipInitTrialValidationAndVersion(t *testing.T) {
	s, actors, u, _ := membershipInitFixture(t)
	app := &Application{committed: s}
	if err := app.ConfigureMembershipUpgrade(u); err != nil {
		t.Fatal(err)
	}
	trial, err := s.blockCopy()
	if err != nil {
		t.Fatal(err)
	}
	if err = app.activateMembershipTrial(trial, 3); err != nil {
		t.Fatal(err)
	}
	if trial.Version != 11 || trial.Membership != nil {
		t.Fatal("trial must upgrade semantics without installing members")
	}
	app.upgrade.MembershipInit.Operators[validatorID(actors[0].key.Public().(ed25519.PublicKey))] = "missing-account"
	raw, err := Sign(s.Parameters.ChainID, actors[0].key, 1, Action{Type: "transfer", To: actors[1].id, Amount: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	check, err := app.CheckTx(ctx, &abci.CheckTxRequest{Tx: raw})
	if err != nil || check.Code == 0 {
		t.Fatal("CheckTx skipped operator validation", err)
	}
	if _, err = app.PrepareProposal(ctx, &abci.PrepareProposalRequest{Height: 3}); err == nil {
		t.Fatal("PrepareProposal skipped operator validation")
	}
	if _, err = app.ProcessProposal(ctx, &abci.ProcessProposalRequest{Height: 3}); err == nil {
		t.Fatal("ProcessProposal skipped operator validation")
	}
}
