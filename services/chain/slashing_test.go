package chain

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math"
	"path/filepath"
	"testing"

	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

func confirmedSlash(t *testing.T, state *State, actors map[string]actor, index int, height int64, confirmation string) *Hold {
	t.Helper()
	holdID := fmt.Sprintf("confirmed-slash-%d", index)
	apply(t, state, actors["payer"], state.Accounts[actors["payer"].id].Nonce+1, Action{Type: "freeze", HoldID: holdID, Payer: actors["payer"].id, Node: actors["node"].id, Amount: 5}, height)
	rejected(t, state, actors["node"], Action{Type: "unbond", Amount: state.Accounts[actors["node"].id].Stake}, height)
	receipt := signedReceipt(t, holdID, actors["node"])
	apply(t, state, actors["coordinator"], state.Accounts[actors["coordinator"].id].Nonce+1, Action{Type: "submit", HoldID: holdID, Actual: 1, Receipt: receipt}, height+1)
	apply(t, state, actors["coordinator"], state.Accounts[actors["coordinator"].id].Nonce+1, Action{Type: "assign_audit", HoldID: holdID}, height+2)
	first, proof := assignedActor(t, state, actors, holdID)
	challenge, verdict, _ := signedAudit(t, receipt, first, "reject", proof)
	apply(t, state, first, state.Accounts[first.id].Nonce+1, Action{Type: "audit", HoldID: holdID, Challenge: challenge, Verdict: verdict}, height+3)
	rejected(t, state, actors["coordinator"], Action{Type: "settle", HoldID: holdID, Outcome: "reject"}, height+3)
	if len(state.Offenses) != 0 && index == 0 {
		t.Fatal("first accusation created an offense")
	}
	second, proof := assignedActor(t, state, actors, holdID)
	if first.id == second.id {
		t.Fatal("confirmation reused first auditor")
	}
	challenge, verdict, _ = signedAudit(t, receipt, second, confirmation, proof)
	apply(t, state, second, state.Accounts[second.id].Nonce+1, Action{Type: "audit", HoldID: holdID, Challenge: challenge, Verdict: verdict}, height+4)
	outcome := state.Holds[holdID].AuditVerdict
	apply(t, state, actors["coordinator"], state.Accounts[actors["coordinator"].id].Nonce+1, Action{Type: "settle", HoldID: holdID, Outcome: outcome}, height+5)
	return state.Holds[holdID]
}

func TestSlashFloorConfirmedLadderWindowAndRefund(t *testing.T) {
	state, actors := testState(t)
	state.Parameters.Misjudge.CapBlockBPS = 10000
	state.Parameters.SlashRepeatWindow = 100
	state.Nodes[actors["node"].id].ExpiresHeight = 1000
	for index, multiplier := range []uint32{1, 2, 4, 8, 8} {
		stake := state.Accounts[actors["node"].id].Stake
		payer := state.Accounts[actors["payer"].id].Balance
		height := int64(10 + index*10)
		hold := confirmedSlash(t, state, actors, index, height, "reject")
		floor := (stake + 99) / 100
		penalty := floor * int64(multiplier)
		if len(hold.Slashes) != 1 || hold.Slashes[0].Multiplier != multiplier || hold.Slashes[0].Height != height+5 || hold.Slashes[0].StakeFloor != floor || hold.Slashes[0].Amount != penalty || state.Accounts[actors["node"].id].Stake != stake-penalty || state.Accounts[actors["payer"].id].Balance != payer+penalty || state.exposure(actors["node"].id) != 0 {
			t.Fatalf("wrong floor, ladder, refund or settlement height: %+v", hold)
		}
		rejected(t, state, actors["coordinator"], Action{Type: "settle", HoldID: hold.ID, Outcome: "reject"}, height+5)
	}
	if len(state.Offenses[actors["node"].id]) != 3 {
		t.Fatal("unbounded offense history")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(path, state); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadState(path)
	if err != nil || len(reloaded.Offenses[actors["node"].id]) != 3 {
		t.Fatal("offenses did not survive persistence", err)
	}
	hold := confirmedSlash(t, reloaded, actors, 5, 150, "reject")
	if hold.Slashes[0].Height != 155 || hold.Slashes[0].Multiplier != 1 {
		t.Fatal("offense at exact rolling-window boundary did not expire", hold.Slashes)
	}
}

func TestSlashBPSCeilingRevenueCapAndOverflow(t *testing.T) {
	for _, test := range []struct {
		name           string
		stake, revenue int64
		bps            uint32
		floor, amount  int64
	}{
		{"ceiling", 10001, 1, 100, 101, 101},
		{"one-basis-point", 1, 0, 1, 1, 1},
		{"revenue-primary", 5000, 100, 100, 50, 2000},
		{"full-stake", 5000, 1, 10000, 5000, 5000},
		{"revenue-overflow-capped", math.MaxInt64, math.MaxInt64, 100, 92233720368547759, math.MaxInt64},
		{"floor-overflow-safe", math.MaxInt64, 0, 10000, math.MaxInt64, math.MaxInt64},
	} {
		t.Run(test.name, func(t *testing.T) {
			state, actors := testState(t)
			state.Parameters.Misjudge.CapBlockBPS = 10000
			state.Parameters.SlashFloorBPS = test.bps
			state.Accounts[actors["node"].id].Stake = test.stake
			state.Accounts[actors["payer"].id].Balance = 0
			hold := &Hold{Payer: actors["payer"].id}
			if err := state.slashOffender(hold, actors["node"].id, test.revenue, 8); err != nil {
				t.Fatal(err)
			}
			if hold.Slashes[0].Amount != test.amount || hold.Slashes[0].StakeFloor != test.floor || state.Accounts[actors["node"].id].Stake != test.stake-test.amount || state.Accounts[actors["payer"].id].Balance != test.amount {
				t.Fatal("slash arithmetic or conservation failed", hold.Slashes)
			}
		})
	}
}

func TestSlashFalseAccusationAndHonestExitHaveNoOffense(t *testing.T) {
	state, actors := testState(t)
	hold := confirmedSlash(t, state, actors, 0, 1, "accept")
	if hold.State != "settled" || len(hold.Slashes) != 0 || len(state.Offenses) != 0 || state.Accounts[actors["node"].id].Stake != 5000 || state.Accounts[actors["payer"].id].Balance != 999 {
		t.Fatal("false accusation changed stake, refund or offenses", hold)
	}
	apply(t, state, actors["node"], 1, Action{Type: "unbond", Amount: 5000}, 7)
	rejected(t, state, actors["node"], Action{Type: "claim_unbonded"}, 9)
	apply(t, state, actors["node"], 2, Action{Type: "claim_unbonded"}, 10)
	if state.Accounts[actors["node"].id].Balance != 5001 || len(state.Offenses) != 0 {
		t.Fatal("honest exit changed")
	}
}

func TestSlashPipelineOnlyConfirmedAttributedOffender(t *testing.T) {
	state, actors, nodes, auth, receipt := pipelineState(t)
	for _, node := range nodes {
		state.Accounts[node.id].Stake = 50000
	}
	apply(t, state, actors["payer"], 1, Action{Type: "freeze_pipeline", HoldID: "units", Payer: actors["payer"].id, Amount: 6, TotalLayers: 4, PipelineAuthorizations: auth}, 2)
	raw, _ := proto.Marshal(receipt)
	apply(t, state, actors["coordinator"], 1, Action{Type: "submit_pipeline", HoldID: "units", Actual: 6, Receipt: raw}, 3)
	apply(t, state, actors["coordinator"], 2, Action{Type: "assign_audit", HoldID: "units"}, 4)
	for round := 0; round < 2; round++ {
		hold := state.Holds["units"]
		auditor, proof := assignedActor(t, state, actors, "units")
		ref, _ := hex.DecodeString(hold.ReceiptRef)
		challenge := &pb.AuditChallenge{ReceiptRef: ref, VrfProof: proof}
		core.Sign(challenge, auditor.key)
		challengeRef, _ := core.Digest(challenge)
		verdict := &pb.AuditVerdict{ChallengeRef: challengeRef, AuditorId: auditor.id, Verdict: "reject", EvidenceRef: bytes.Repeat([]byte{7}, 32), FaultNodeIds: []string{nodes[1].id}, Offense: &pb.OffenseGrade{Class: "objective", Fault: "hash"}}
		core.Sign(verdict, auditor.key)
		challengeRaw, _ := proto.Marshal(challenge)
		verdictRaw, _ := proto.Marshal(verdict)
		apply(t, state, auditor, 1, Action{Type: "audit", HoldID: "units", Challenge: challengeRaw, Verdict: verdictRaw}, int64(5+round))
		if round == 0 {
			rejected(t, state, actors["coordinator"], Action{Type: "settle", HoldID: "units", Outcome: "reject"}, 5)
		}
	}
	apply(t, state, actors["coordinator"], 3, Action{Type: "settle", HoldID: "units", Outcome: "reject"}, 7)
	hold := state.Holds["units"]
	if len(hold.Slashes) != 1 || hold.Slashes[0].Revenue != 4 || hold.Slashes[0].Amount != 500 || len(state.Offenses) != 1 || state.Accounts[actors["payer"].id].Balance != 1500 {
		t.Fatal("pipeline fault floor, attribution or refund failed", hold)
	}
	for index, node := range nodes {
		want := int64(50000)
		if index == 1 {
			want -= 500
		}
		if state.Accounts[node.id].Stake != want || state.exposure(node.id) != 0 {
			t.Fatal("honest stage slashed or exposure retained")
		}
	}
}

func TestSlashGenesisPolicyBounds(t *testing.T) {
	for _, test := range []struct {
		bps     uint32
		window  int64
		invalid bool
	}{
		{0, 0, false}, {1, 1, false}, {10000, math.MaxInt64, false}, {10001, 1, true}, {100, -1, true},
	} {
		state, err := NewState(GenesisState{Parameters: Parameters{ChainID: "slash-policy", SlashMultiplier: 20, UnbondingBlocks: 1, SlashFloorBPS: test.bps, SlashRepeatWindow: test.window}})
		if (err != nil) != test.invalid {
			t.Fatal("wrong policy validation", test, err)
		}
		if !test.invalid && (state.Parameters.SlashFloorBPS == 0 || state.Parameters.SlashRepeatWindow == 0 || state.Version != AppVersion) {
			t.Fatal("missing slash defaults or version")
		}
	}
}

func TestSlashServiceFailureAndExpiryRefundWithoutOffenses(t *testing.T) {
	state, actors := testState(t)
	apply(t, state, actors["payer"], 1, Action{Type: "freeze", HoldID: "no-confirmation", Payer: actors["payer"].id, Node: actors["node"].id, Amount: 5}, 1)
	receipt := signedReceipt(t, "no-confirmation", actors["node"])
	apply(t, state, actors["coordinator"], 1, Action{Type: "submit", HoldID: "no-confirmation", Actual: 1, Receipt: receipt}, 2)
	apply(t, state, actors["coordinator"], 2, Action{Type: "assign_audit", HoldID: "no-confirmation"}, 3)
	auditor, proof := assignedActor(t, state, actors, "no-confirmation")
	state.Auditors = map[string]bool{auditor.id: true}
	challenge, verdict, _ := signedAudit(t, receipt, auditor, "reject", proof)
	apply(t, state, auditor, 1, Action{Type: "audit", HoldID: "no-confirmation", Challenge: challenge, Verdict: verdict}, 4)
	apply(t, state, actors["coordinator"], 3, Action{Type: "settle", HoldID: "no-confirmation", Outcome: "service_failure"}, 5)
	apply(t, state, actors["payer"], 2, Action{Type: "freeze", HoldID: "expired", Payer: actors["payer"].id, Node: actors["node"].id, Amount: 5}, 6)
	apply(t, state, actors["payer"], 3, Action{Type: "expire_hold", HoldID: "expired"}, 6+state.Parameters.HoldTimeout)
	if len(state.Offenses) != 0 || state.Accounts[actors["payer"].id].Balance != 1000 || state.Accounts[actors["node"].id].Stake != 5000 || state.exposure(actors["node"].id) != 0 {
		t.Fatal("unconfirmed failure or expiry lost refund/stake or added an offense")
	}
}

func TestSlashStateVersionRequiresExplicitMigration(t *testing.T) {
	state, _ := testState(t)
	state.Version = 2
	path := filepath.Join(t.TempDir(), "old-state.json")
	if err := SaveState(path, state); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(path); err == nil {
		t.Fatal("old consensus state was silently upgraded")
	}
}
