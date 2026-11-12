package chain

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

type actor struct {
	key ed25519.PrivateKey
	id  string
}

func newActor(value byte) actor {
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = value
	key := ed25519.NewKeyFromSeed(seed)
	return actor{key: key, id: AccountID(key.Public().(ed25519.PublicKey))}
}

func testState(t *testing.T) (*State, map[string]actor) {
	t.Helper()
	actors := map[string]actor{"payer": newActor(1), "node": newActor(2), "coordinator": newActor(3), "auditor": newActor(4), "governor": newActor(5), "auditor2": newActor(6)}
	accounts := make([]GenesisAccount, 0, len(actors))
	for name, actor := range actors {
		balance := int64(1000)
		stake := int64(0)
		if name == "node" {
			balance, stake = 0, 5000
		}
		accounts = append(accounts, GenesisAccount{ID: actor.id, Balance: balance, Stake: stake})
	}
	state, err := NewState(GenesisState{Parameters: Parameters{ChainID: "ember-testnet", SubsidyCap: 50, UnbondingBlocks: 3, SlashMultiplier: 20, DisputeBlocks: 1, MinNodeStake: 100, HoldTimeout: 20, RegistrationTTL: 100},
		Accounts: accounts, Models: []Model{{ID: "ember-test-model", GenesisHash: strings.Repeat("1", 64), ProfileHash: strings.Repeat("2", 64)}},
		Coordinators: []string{actors["coordinator"].id}, Auditors: []string{actors["auditor"].id, actors["auditor2"].id}, Governors: []string{actors["governor"].id}})
	if err != nil {
		t.Fatal(err)
	}
	profile := &pb.ResourceProfile{NodeId: actors["node"].id, ChainId: "ember-test-model", ManifestRoot: bytes.Repeat([]byte{9}, 32), ExecutionProfileVer: "v1"}
	if err = core.Sign(profile, actors["node"].key); err != nil {
		t.Fatal(err)
	}
	raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(profile)
	state.Nodes[actors["node"].id] = &NodeRegistration{NodeID: actors["node"].id, Profile: raw, RegisteredAt: 0, ExpiresHeight: 100}
	state.Entropy = bytes.Repeat([]byte{7}, 32)
	// Unit action fixtures supply finalized block hashes explicitly. ABCI tests
	// exercise arrival and persistence of actual future entropy independently.
	// Repeated scenario fixtures deliberately isolate other policies from cooldown.
	state.Parameters.Assignment.CooldownRequests = 100
	// Repeated scenarios isolate other policies; dedicated work-cap tests set limits.
	state.Parameters.AuditWork.MaxPerExecutor = 100000
	state.BlockHashes = map[int64][]byte{}
	for i := int64(1); i <= 200; i++ {
		b := sha256.Sum256([]byte(fmt.Sprint(i)))
		state.BlockHashes[i] = b[:]
	}
	return state, actors
}

func apply(t *testing.T, state *State, actor actor, nonce uint64, action Action, height int64) []byte {
	t.Helper()
	raw, err := Sign(state.Parameters.ChainID, actor.key, nonce, action)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = state.Apply(raw, height); err != nil {
		t.Fatal(err)
	}
	return raw
}

func signedReceipt(t *testing.T, holdID string, node actor) []byte {
	t.Helper()
	receipt := &pb.ExecutionReceipt{RequestId: holdID, NodeId: node.id, OutputTokens: 1}
	root := sha256.Sum256([]byte("root"))
	chunk := &pb.ChunkRoot{RequestId: holdID, Seq: 0, UptoStep: 1, MerkleRoot: root[:]}
	if err := core.Sign(chunk, node.key); err != nil {
		t.Fatal(err)
	}
	receipt.ChunkRoots = []*pb.ChunkRoot{chunk}
	if err := core.Sign(receipt, node.key); err != nil {
		t.Fatal(err)
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func signedAudit(t *testing.T, receiptRaw []byte, auditor actor, outcome string, proof []byte) ([]byte, []byte, string) {
	t.Helper()
	receipt := &pb.ExecutionReceipt{}
	if err := proto.Unmarshal(receiptRaw, receipt); err != nil {
		t.Fatal(err)
	}
	receiptRef, _ := core.Digest(receipt)
	challenge := &pb.AuditChallenge{ReceiptRef: receiptRef, Positions: []uint32{0}, VrfProof: proof}
	if err := core.Sign(challenge, auditor.key); err != nil {
		t.Fatal(err)
	}
	challengeRef, _ := core.Digest(challenge)
	evidence := sha256.Sum256([]byte("verified work"))
	verdict := &pb.AuditVerdict{ChallengeRef: challengeRef, AuditorId: auditor.id, Verdict: outcome, EvidenceRef: evidence[:]}
	if outcome == "reject" {
		verdict.Offense = &pb.OffenseGrade{Class: "objective", Fault: "hash"}
	}
	if err := core.Sign(verdict, auditor.key); err != nil {
		t.Fatal(err)
	}
	challengeRaw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(challenge)
	verdictRaw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(verdict)
	return challengeRaw, verdictRaw, hex.EncodeToString(evidence[:])
}

func assignedActor(t *testing.T, state *State, actors map[string]actor, holdID string) (actor, []byte) {
	t.Helper()
	hold := state.Holds[holdID]
	for _, value := range actors {
		if value.id == hold.Auditor {
			proof, err := hex.DecodeString(hold.AuditProof)
			if err != nil {
				t.Fatal(err)
			}
			return value, proof
		}
	}
	t.Fatalf("assigned auditor %s not found", hold.Auditor)
	return actor{}, nil
}

func TestLifecycleTransferAndSubsidy(t *testing.T) {
	state, actors := testState(t)
	holdID := "request-1"
	apply(t, state, actors["payer"], 1, Action{Type: "freeze", HoldID: holdID, Payer: actors["payer"].id, Node: actors["node"].id, Amount: 100}, 1)
	receipt := signedReceipt(t, holdID, actors["node"])
	apply(t, state, actors["coordinator"], 1, Action{Type: "submit", HoldID: holdID, Actual: 4, Receipt: receipt}, 2)
	apply(t, state, actors["coordinator"], 2, Action{Type: "assign_audit", HoldID: holdID}, 3)
	auditor, proof := assignedActor(t, state, actors, holdID)
	challenge, verdict, evidence := signedAudit(t, receipt, auditor, "accept", proof)
	apply(t, state, auditor, 1, Action{Type: "audit", HoldID: holdID, Challenge: challenge, Verdict: verdict}, 4)
	apply(t, state, actors["coordinator"], 3, Action{Type: "settle", HoldID: holdID, Outcome: "accept"}, 4)
	if got := state.Accounts[actors["payer"].id].Balance; got != 996 {
		t.Fatalf("payer balance %d", got)
	}
	if got := state.Accounts[actors["node"].id].Balance; got != 4 {
		t.Fatalf("node balance %d", got)
	}
	apply(t, state, actors["node"], 1, Action{Type: "claim_subsidy", EvidenceRef: evidence, Amount: 30}, 4)
	apply(t, state, actors["node"], 2, Action{Type: "transfer", To: actors["payer"].id, Amount: 10}, 5)
	if got := state.Accounts[actors["node"].id].Balance; got != 24 {
		t.Fatalf("node post-transfer %d", got)
	}
	if got := state.Accounts[actors["payer"].id].Balance; got != 1006 {
		t.Fatalf("payer post-transfer %d", got)
	}
	raw, _ := Sign(state.Parameters.ChainID, actors["node"].key, 3, Action{Type: "claim_subsidy", EvidenceRef: evidence, Amount: 1})
	if _, err := state.Apply(raw, 6); err == nil {
		t.Fatal("subsidy evidence replay accepted")
	}
}

func TestReplayWrongChainSlashAndUnbond(t *testing.T) {
	state, actors := testState(t)
	state.Parameters.Misjudge.CapBlockBPS = 10000
	raw := apply(t, state, actors["payer"], 1, Action{Type: "transfer", To: actors["governor"].id, Amount: 1}, 1)
	if _, err := state.Apply(raw, 2); err == nil {
		t.Fatal("replay accepted")
	}
	wrong, _ := Sign("wrong", actors["payer"].key, 2, Action{Type: "transfer", To: actors["governor"].id, Amount: 1})
	if _, err := state.Apply(wrong, 2); err == nil {
		t.Fatal("wrong chain accepted")
	}
	holdID := "request-slash"
	apply(t, state, actors["payer"], 2, Action{Type: "freeze", HoldID: holdID, Payer: actors["payer"].id, Node: actors["node"].id, Amount: 100}, 2)
	receipt := signedReceipt(t, holdID, actors["node"])
	apply(t, state, actors["coordinator"], 1, Action{Type: "submit", HoldID: holdID, Actual: 100, Receipt: receipt}, 3)
	apply(t, state, actors["coordinator"], 2, Action{Type: "assign_audit", HoldID: holdID}, 4)
	first, proof := assignedActor(t, state, actors, holdID)
	challenge, verdict, _ := signedAudit(t, receipt, first, "reject", proof)
	apply(t, state, first, 1, Action{Type: "audit", HoldID: holdID, Challenge: challenge, Verdict: verdict}, 5)
	second, secondProof := assignedActor(t, state, actors, holdID)
	challenge, verdict, _ = signedAudit(t, receipt, second, "reject", secondProof)
	apply(t, state, second, 1, Action{Type: "audit", HoldID: holdID, Challenge: challenge, Verdict: verdict}, 6)
	apply(t, state, actors["coordinator"], 3, Action{Type: "settle", HoldID: holdID, Outcome: "reject"}, 6)
	if got := state.Accounts[actors["node"].id].Stake; got != 3000 {
		t.Fatalf("stake after 20x slash %d", got)
	}
	apply(t, state, actors["node"], 1, Action{Type: "unbond", Amount: 1000}, 7)
	early, _ := Sign(state.Parameters.ChainID, actors["node"].key, 2, Action{Type: "claim_unbonded"})
	if _, err := state.Apply(early, 9); err == nil {
		t.Fatal("early unbond claimed")
	}
	apply(t, state, actors["node"], 2, Action{Type: "claim_unbonded"}, 10)
	if got := state.Accounts[actors["node"].id].Balance; got != 1000 {
		t.Fatalf("claimed balance %d", got)
	}
}

func TestGenesisPreservesAccountReplayAndUnbondingState(t *testing.T) {
	account := newActor(19)
	genesis := GenesisState{Parameters: Parameters{ChainID: "ember-upgrade-test",
		SubsidyCap: 10, UnbondingBlocks: 3, SlashMultiplier: 20,
		MinNodeStake: 1, HoldTimeout: 20, RegistrationTTL: 100},
		Accounts: []GenesisAccount{{ID: account.id, Balance: 7, Stake: 5, Nonce: 9,
			Unbondings: []Unbond{{Amount: 3, ReleaseHeight: 40}}}}}
	state, err := NewState(genesis)
	if err != nil {
		t.Fatal(err)
	}
	got := state.Accounts[account.id]
	if got.Nonce != 9 || len(got.Unbondings) != 1 || got.Unbondings[0].Amount != 3 {
		t.Fatalf("account migration state lost: %+v", got)
	}
}

func TestNodeRegistrationRequiresStakeAndRateLimitsUpdates(t *testing.T) {
	state, actors := testState(t)
	joining := newActor(9)
	state.Accounts[joining.id] = &Account{Balance: 1000}
	profile := &pb.ResourceProfile{NodeId: joining.id, ChainId: "ember-test-model", ManifestRoot: bytes.Repeat([]byte{3}, 32), ExecutionProfileVer: "v1"}
	if err := core.Sign(profile, joining.key); err != nil {
		t.Fatal(err)
	}
	rawProfile, _ := proto.MarshalOptions{Deterministic: true}.Marshal(profile)
	register, _ := Sign(state.Parameters.ChainID, joining.key, 1, Action{Type: "register_node", Profile: rawProfile, ExpiresHeight: 20})
	if _, err := state.Apply(register, 1); err == nil {
		t.Fatal("under-staked registration accepted")
	}
	apply(t, state, joining, 1, Action{Type: "stake", Amount: 100}, 1)
	apply(t, state, joining, 2, Action{Type: "register_node", Profile: rawProfile, ExpiresHeight: 20}, 2)
	tooSoon, _ := Sign(state.Parameters.ChainID, joining.key, 3, Action{Type: "register_node", Profile: rawProfile, ExpiresHeight: 21})
	if _, err := state.Apply(tooSoon, 3); err == nil {
		t.Fatal("registration spam accepted")
	}
	if state.Nodes[joining.id] == nil || state.Nodes[joining.id].ExpiresHeight != 20 {
		t.Fatal("registration missing")
	}
	_ = actors
}

func TestFalseRejectCannotSlashWithoutIndependentConfirmation(t *testing.T) {
	state, actors := testState(t)
	holdID := "false-reject"
	apply(t, state, actors["payer"], 1, Action{Type: "freeze", HoldID: holdID, Payer: actors["payer"].id, Node: actors["node"].id, Amount: 50}, 1)
	receipt := signedReceipt(t, holdID, actors["node"])
	apply(t, state, actors["coordinator"], 1, Action{Type: "submit", HoldID: holdID, Actual: 10, Receipt: receipt}, 2)
	apply(t, state, actors["coordinator"], 2, Action{Type: "assign_audit", HoldID: holdID}, 3)
	malicious, proof := assignedActor(t, state, actors, holdID)
	challenge, verdict, _ := signedAudit(t, receipt, malicious, "reject", proof)
	apply(t, state, malicious, 1, Action{Type: "audit", HoldID: holdID, Challenge: challenge, Verdict: verdict}, 4)
	honest, honestProof := assignedActor(t, state, actors, holdID)
	challenge, verdict, _ = signedAudit(t, receipt, honest, "accept", honestProof)
	apply(t, state, honest, 1, Action{Type: "audit", HoldID: holdID, Challenge: challenge, Verdict: verdict}, 5)
	apply(t, state, actors["payer"], 2, Action{Type: "settle", HoldID: holdID, Outcome: "accept"}, 5)
	if state.Accounts[actors["node"].id].Stake != 5000 || state.Holds[holdID].State != "settled" {
		t.Fatalf("false reject caused penalty: hold=%+v stake=%d", state.Holds[holdID], state.Accounts[actors["node"].id].Stake)
	}
}

func TestExpiredHoldCanBeRefundedPermissionlessly(t *testing.T) {
	state, actors := testState(t)
	holdID := "grief"
	apply(t, state, actors["payer"], 1, Action{Type: "freeze", HoldID: holdID, Payer: actors["payer"].id, Node: actors["node"].id, Amount: 50}, 1)
	if state.Accounts[actors["payer"].id].Balance != 950 {
		t.Fatal("freeze did not debit payer")
	}
	expire := actors["auditor"]
	apply(t, state, expire, 1, Action{Type: "expire_hold", HoldID: holdID}, 21)
	if state.Accounts[actors["payer"].id].Balance != 1000 || state.Holds[holdID].State != "cancelled" {
		t.Fatal("expired hold was not refunded")
	}
}

func TestModelImmutableAndHashStable(t *testing.T) {
	state, actors := testState(t)
	action := publicationAction(t, actors["governor"], "ember-model")
	apply(t, state, actors["governor"], 1, action, 1)
	raw, _ := Sign(state.Parameters.ChainID, actors["governor"].key, 2, action)
	if _, err := state.Apply(raw, 2); err == nil {
		t.Fatal("model mutation accepted")
	}
	bytes1, _ := state.Bytes()
	var copied State
	if err := json.Unmarshal(bytes1, &copied); err != nil {
		t.Fatal(err)
	}
	hash1, _ := state.Hash()
	hash2, _ := copied.Hash()
	if !equal(hash1, hash2) {
		t.Fatal("canonical state hash changed after round trip")
	}
}

func TestApplyIsAtomicOnLateFailure(t *testing.T) {
	state, actors := testState(t)
	recipient := actors["governor"].id
	state.Accounts[recipient].Balance = math.MaxInt64
	before, err := state.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Sign(state.Parameters.ChainID, actors["payer"].key, 1,
		Action{Type: "transfer", To: recipient, Amount: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = state.Apply(raw, 1); err == nil {
		t.Fatal("overflowing transfer accepted")
	}
	after, err := state.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("failed transition mutated state")
	}
}
