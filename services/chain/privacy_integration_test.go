package chain

import (
	"bytes"
	"crypto/sha256"
	core "ember/core-go"
	pb "ember/proto"
	"encoding/json"
	"fmt"
	"google.golang.org/protobuf/proto"
	"reflect"
	"strings"
	"testing"
)

func privacyFixture(t *testing.T) (*State, map[string]actor) {
	s, a, n, _, _ := pricedFixture(t)
	a["node"], a["target"] = n[0], n[1]
	m := s.Models["ember-unit-test"]
	m.Pricing = &RolePricing{TargetExecutors: 2, TargetAuditors: 2, ExecutorWeight: 80, AuditorWeight: 15, ConsumerWeight: 5}
	s.Models[m.ID] = m
	for _, who := range []string{"node", "target", "auditor", "auditor2"} {
		profile := s.ActiveProfile(a["node"].id, s.Height)
		profile.NodeId = a[who].id
		roles := []string{"model"}
		if who == "auditor" || who == "auditor2" {
			roles = []string{"auditor"}
		}
		profile.EngineCapabilities = &pb.EngineCapabilities{Roles: roles, ForcedPrefixReplay: true, ScoreKind: "raw-logits", MaxTopK: 16, EvidenceLevels: []string{"full"}}
		core.Sign(profile, a[who].key)
		raw, _ := proto.Marshal(profile)
		s.Nodes[a[who].id] = &NodeRegistration{NodeID: a[who].id, Profile: raw, ExpiresHeight: 100}
		if s.Accounts[a[who].id].Stake == 0 {
			s.Accounts[a[who].id].Stake = 5000
		}
	}
	policy := json.RawMessage(`{"retention_seconds":60,"deletion_practice":"best effort","evidence_handling":"encrypted"}`)
	for _, who := range []string{"node", "target", "payer", "auditor", "auditor2", "coordinator", "governor"} {
		privacySend(t, s, a[who], PrivacyAction{Op: "operator", WrapKey: bytes.Repeat([]byte{3}, 32), Policy: policy})
	}
	privacySend(t, s, a["governor"], PrivacyAction{Op: "epoch", Epoch: "e", Threshold: 2, Members: []string{a["payer"].id, a["governor"].id}})
	return s, a
}
func privacySend(t *testing.T, s *State, who actor, p PrivacyAction) {
	t.Helper()
	apply(t, s, who, s.Accounts[who.id].Nonce+1, Action{Type: "privacy", Privacy: &p}, s.Height+1)
}
func privacyFail(t *testing.T, s *State, who actor, p PrivacyAction) {
	t.Helper()
	rejected(t, s, who, Action{Type: "privacy", Privacy: &p}, s.Height+1)
}
func privacyRequest(t *testing.T, s *State, a map[string]actor, id string, input, max uint32) {
	c := &pb.ExecutionCommitment{RequestId: id, ChainId: "ember-unit-test", ProfileVer: "units-test", InputTokens: input, MaxOutputTokens: max, InputCommitment: bytes.Repeat([]byte{9}, 32)}
	core.Sign(c, a["node"].key)
	raw, _ := proto.Marshal(c)
	g := &pb.ChainGenesis{}
	proto.Unmarshal(s.Models[c.ChainId].Genesis, g)
	fee, _ := core.TokenFee(g, input, max)
	privacySend(t, s, a["payer"], PrivacyAction{Op: "request", RequestID: id, ModelID: c.ChainId, Executor: a["node"].id, Tier: "P0", Epoch: "e", EnvelopeHash: strings.Repeat("a", 64), Amount: fee.Total, PricingCommitment: raw})
}
func privacyActor(t *testing.T, s *State, a map[string]actor, id string) actor {
	t.Helper()
	for _, who := range a {
		if who.id == s.Privacy.Requests[id].Auditor {
			return who
		}
	}
	t.Fatal("unknown auditor")
	return actor{}
}
func TestPrivacyLedgerParityAndEntropy(t *testing.T) {
	for _, scarce := range []bool{false, true} {
		t.Run(fmt.Sprint(scarce), func(t *testing.T) {
			s, a := privacyFixture(t)
			if scarce {
				m := s.Models["ember-unit-test"]
				m.Pricing.TargetExecutors = 10
				m.Pricing.TargetAuditors = 10
				s.Models[m.ID] = m
			}
			privacyRequest(t, s, a, "units", 33, 100)
			h := s.Holds["units"]
			if h.State != "frozen" || h.Amount != 369 || s.Accounts[a["payer"].id].Balance != 631 {
				t.Fatal("not frozen through ledger")
			}
			p := PrivacyAction{Op: "commit", RequestID: "units", Root: strings.Repeat("b", 64), BundleRef: strings.Repeat("c", 64), InputTokens: 33, OutputTokens: 33, Actual: 166}
			bad := p
			bad.InputTokens = 32
			privacyFail(t, s, a["node"], bad)
			bad = p
			bad.Actual++
			privacyFail(t, s, a["node"], bad)
			privacySend(t, s, a["node"], p)
			privacyFail(t, s, a["coordinator"], PrivacyAction{Op: "assign", RequestID: "units", Auditor: a["auditor"].id})
			saved := s.BlockHashes
			s.BlockHashes = nil
			privacyFail(t, s, a["coordinator"], PrivacyAction{Op: "assign", RequestID: "units"})
			s.BlockHashes = saved
			draw, _ := s.Clone()
			expected, proof, err := draw.selectAuditor(draw.Holds["units"], "")
			if err != nil {
				t.Fatal(err)
			}
			privacySend(t, s, a["coordinator"], PrivacyAction{Op: "assign", RequestID: "units"})
			h = s.Holds["units"]
			if h.Auditor != expected || h.AuditProof != fmt.Sprintf("%x", proof) {
				t.Fatal("different entropy assignment")
			}
			// Compare the ordinary signed receipt path on a clone with identical hold and policy.
			legacy, _ := s.Clone()
			delete(legacy.Privacy.Requests, "units")
			lh := legacy.Holds["units"]
			lh.State = "frozen"
			lh.Auditor = ""
			lh.AuditRound = 0
			lh.AuditProof = ""
			receipt := &pb.ExecutionReceipt{}
			proto.Unmarshal(signedReceipt(t, "units", a["node"]), receipt)
			receipt.InputTokens = 33
			receipt.OutputTokens = 33
			receipt.ChunkRoots = nil
			var previous []byte
			for i, upto := range []uint32{16, 32, 33} {
				chunk := &pb.ChunkRoot{RequestId: "units", Seq: uint32(i), UptoStep: upto, PrevRoot: previous, MerkleRoot: bytes.Repeat([]byte{9}, 32)}
				core.Sign(chunk, a["node"].key)
				previous, _ = core.Digest(chunk)
				receipt.ChunkRoots = append(receipt.ChunkRoots, chunk)
			}
			ref := sha256.Sum256(lh.TokenFee.Commitment)
			receipt.CommitmentRef = ref[:]
			core.Sign(receipt, a["node"].key)
			raw, _ := proto.Marshal(receipt)
			apply(t, legacy, a["node"], legacy.Accounts[a["node"].id].Nonce+1, Action{Type: "submit", HoldID: "units", Actual: 166, Receipt: raw}, legacy.Height+1)
			lh = legacy.Holds["units"]
			lh.Auditor = expected
			lh.AuditVerdict = "accept"
			apply(t, legacy, a["payer"], legacy.Accounts[a["payer"].id].Nonce+1, Action{Type: "settle", HoldID: "units", Outcome: "accept"}, legacy.Height+1)
			lh = legacy.Holds["units"]
			privacySend(t, s, a["payer"], PrivacyAction{Op: "event", Kind: "share_release", RequestID: "units", Epoch: "e"})
			privacySend(t, s, privacyActor(t, s, a, "units"), PrivacyAction{Op: "verdict", RequestID: "units", Verdict: "accept", StatsHash: strings.Repeat("d", 64)})
			privacySend(t, s, a["payer"], PrivacyAction{Op: "finalize", RequestID: "units"})
			h = s.Holds["units"]
			if h.State != "settled" || h.Actual != lh.Actual || h.Shares.Executor != lh.Shares.Executor || h.Shares.Auditor != lh.Shares.Auditor || h.Shares.Consumer != lh.Shares.Consumer {
				t.Fatal("legacy settlement parity failed", h, lh)
			}
			if !scarce {
				want := allocateFee(166, [3]int64{80, 15, 5})
				if h.Shares.Executor != want[0] || h.Shares.Auditor != want[1] || h.Shares.Consumer != want[2] {
					t.Fatal("not 80/15/5")
				}
			}
			if s.Privacy.Events[0].RequestID != h.ID {
				t.Fatal("unlinked escrow event")
			}
		})
	}
}
func TestPrivacyCommonSlashingLadder(t *testing.T) {
	s, a := privacyFixture(t)
	s.Accounts[a["node"].id].Stake = 50000
	// Cheap work makes the stake floor, then repeat multiplier, observable.
	for i := 0; i < 2; i++ {
		id := fmt.Sprint("slash", i)
		privacyRequest(t, s, a, id, 1, 1)
		privacySend(t, s, a["node"], PrivacyAction{Op: "commit", RequestID: id, Root: strings.Repeat("b", 64), BundleRef: strings.Repeat("c", 64), InputTokens: 1, OutputTokens: 1, Actual: 5})
		privacySend(t, s, a["coordinator"], PrivacyAction{Op: "assign", RequestID: id})
		privacyFail(t, s, privacyActor(t, s, a, id), PrivacyAction{Op: "verdict", RequestID: id, Verdict: "reject", StatsHash: strings.Repeat("f", 64)})
		privacySend(t, s, privacyActor(t, s, a, id), PrivacyAction{Op: "verdict", RequestID: id, Verdict: "reject", Offense: &pb.OffenseGrade{Class: "objective", Fault: "hash"}, StatsHash: strings.Repeat("d", 64)})
		if s.Privacy.Requests[id].Status != "assigned" || s.Holds[id].AuditRound != 2 {
			t.Fatal("no independent confirmation")
		}
		privacySend(t, s, privacyActor(t, s, a, id), PrivacyAction{Op: "verdict", RequestID: id, Verdict: "reject", Offense: &pb.OffenseGrade{Class: "objective", Fault: "hash"}, StatsHash: strings.Repeat("e", 64)})
		before, _ := s.Clone()
		expected := before.Holds[id]
		if err := before.settleAt(expected, "reject", s.Height+1); err != nil {
			t.Fatal(err)
		}
		privacySend(t, s, a["payer"], PrivacyAction{Op: "finalize", RequestID: id})
		got := s.Holds[id]
		if got.State != "slashed" || !reflect.DeepEqual(got.Slashes, expected.Slashes) || got.Slashes[0].Multiplier != uint32(1<<i) || got.Slashes[0].Amount != []int64{500, 990}[i] {
			t.Fatal("common slash mismatch", got.Slashes)
		}
	}
}
func TestPrivacyMigrationConsensus(t *testing.T) {
	s, a := privacyFixture(t)
	s.Auditors[a["node"].id] = true
	privacyRequest(t, s, a, "units", 2, 3)
	checkpoint := PrivacyAction{Op: "checkpoint", RequestID: "units", Root: strings.Repeat("b", 64), BundleRef: strings.Repeat("c", 64), InputTokens: 2, OutputTokens: 1}
	privacySend(t, s, a["node"], checkpoint)
	migration := PrivacyAction{Op: "migrate", RequestID: "units", Executor: a["target"].id, MigrationID: strings.Repeat("d", 64)}
	privacyFail(t, s, a["node"], migration)
	privacySend(t, s, a["payer"], migration)
	rewrap := PrivacyAction{Op: "rewrap", RequestID: "units", Executor: a["auditor"].id, MigrationID: migration.MigrationID, WrappedKey: []byte("opaque authenticated wrapper")}
	privacyFail(t, s, a["payer"], rewrap)
	rewrap.Executor = a["target"].id
	privacySend(t, s, a["payer"], rewrap)
	privacyFail(t, s, a["payer"], rewrap)
	privacyFail(t, s, a["payer"], migration)
	privacyFail(t, s, a["node"], checkpoint)
	rejected(t, s, a["node"], Action{Type: "submit", HoldID: "units"}, s.Height+1)
	privacyFail(t, s, a["node"], PrivacyAction{Op: "event", Kind: "share_release", Epoch: "e", RequestID: "units"})
	r := s.Privacy.Requests["units"]
	if r.Executor != a["target"].id || s.Holds[r.ID].Node != r.Executor || len(r.Segments) != 1 || len(s.Holds) != 1 {
		t.Fatal("migration forked request or ledger")
	}
	privacySend(t, s, a["node"], PrivacyAction{Op: "event", Kind: "migration_deleted", Epoch: "e", RequestID: "units"})
	checkpoint.Op = "commit"
	checkpoint.OutputTokens = 3
	checkpoint.Actual = 13
	privacySend(t, s, a["target"], checkpoint)
	privacySend(t, s, a["coordinator"], PrivacyAction{Op: "assign", RequestID: "units"})
	if s.Privacy.Requests["units"].Auditor == a["node"].id {
		t.Fatal("retired executor assigned auditor")
	}
}

func TestPrivacyRefundAndAssignmentTiming(t *testing.T) {
	s, a := privacyFixture(t)
	privacyRequest(t, s, a, "units", 2, 3)
	p := PrivacyAction{Op: "assign", RequestID: "units"}
	privacyFail(t, s, a["coordinator"], p)
	privacySend(t, s, a["node"], PrivacyAction{Op: "commit", RequestID: "units", InputTokens: 2, OutputTokens: 1, Actual: 7, Root: strings.Repeat("b", 64), BundleRef: strings.Repeat("c", 64)})
	rejected(t, s, a["coordinator"], Action{Type: "privacy", Privacy: &p}, s.Height)
	expiry := s.Holds["units"].ExpireHeight
	apply(t, s, a["payer"], s.Accounts[a["payer"].id].Nonce+1, Action{Type: "privacy", Privacy: &PrivacyAction{Op: "expire", RequestID: "units"}}, expiry)
	if s.Privacy.Requests["units"].Status != "final" || s.Holds["units"].State != "cancelled" || s.Accounts[a["payer"].id].Balance != 1000 {
		t.Fatal("privacy expiry did not refund")
	}
}
