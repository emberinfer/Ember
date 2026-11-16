package chain

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	core "ember/core-go"
	pb "ember/proto"

	abci "github.com/cometbft/cometbft/abci/types"
	"google.golang.org/protobuf/proto"
)

// setProfileCaps rewrites a registered node's signed profile in place,
// following the mutation pattern of TestEqualityEligibility.
func setProfileCaps(t *testing.T, s *State, a actor, mutate func(*pb.ResourceProfile)) {
	t.Helper()
	p := s.ActiveProfile(a.id, 0)
	if p == nil {
		t.Fatalf("no active profile for %s", a.id)
	}
	mutate(p)
	if err := core.Sign(p, a.key); err != nil {
		t.Fatal(err)
	}
	raw, err := proto.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	s.Nodes[a.id].Profile = raw
}

func logprobsCaps(roles ...string) *pb.EngineCapabilities {
	return &pb.EngineCapabilities{EngineName: "vllm", EngineVersion: "0.33.0", DeviceClass: "remote", NumericalFormat: "fp8", Roles: roles, ScoreKind: "logprobs", MaxTopK: 16, EvidenceLevels: []string{"topk-logprobs"}}
}

func fullCaps(roles ...string) *pb.EngineCapabilities {
	return &pb.EngineCapabilities{EngineName: "transformers", EngineVersion: "4.45.2", DeviceClass: "cuda", NumericalFormat: "bfloat16", Roles: roles, ForcedPrefixReplay: true, ScoreKind: "raw-logits", MaxTopK: 16, EvidenceLevels: []string{"full"}}
}

// TestAuditClassEligibility pins the v7 rule: the auditor bar is relative to
// the executor profile's evidence class. The v6 rows pin the pre-activation
// behavior so the upgrade's exact delta is visible.
func TestAuditClassEligibility(t *testing.T) {
	topk8Full := fullCaps("auditor")
	topk8Full.MaxTopK = 8
	topk8Logprobs := logprobsCaps("auditor")
	topk8Logprobs.MaxTopK = 8
	cases := []struct {
		name       string
		version    uint64
		execCaps   *pb.EngineCapabilities
		auditCaps  *pb.EngineCapabilities
		auditTweak func(*pb.ResourceProfile)
		want       bool
	}{
		{"v7 full executor, full auditor", 7, fullCaps("model"), fullCaps("auditor"), nil, true},
		{"v7 full executor, logprobs auditor stays barred", 7, fullCaps("model"), logprobsCaps("auditor"), nil, false},
		{"v7 full executor, full auditor topk8", 7, fullCaps("model"), topk8Full, nil, false},
		{"v7 logprobs executor, logprobs auditor", 7, logprobsCaps("model"), logprobsCaps("auditor"), nil, true},
		{"v7 logprobs executor, logprobs auditor topk8", 7, logprobsCaps("model"), topk8Logprobs, nil, false},
		{"v7 logprobs executor, full auditor (superset)", 7, logprobsCaps("model"), fullCaps("auditor"), nil, true},
		{"v7 logprobs executor, auditor on other manifest", 7, logprobsCaps("model"), logprobsCaps("auditor"), func(p *pb.ResourceProfile) { p.ManifestRoot = []byte("other-manifest-root") }, false},
		{"v7 logprobs executor, auditor without auditor role", 7, logprobsCaps("model"), logprobsCaps("model"), nil, false},
		{"v7 logprobs executor, auditor without capabilities", 7, logprobsCaps("model"), nil, nil, false},
		{"v6 logprobs executor, logprobs auditor still barred", 6, logprobsCaps("model"), logprobsCaps("auditor"), nil, false},
		{"v6 logprobs executor, full auditor admitted (v6 rule)", 6, logprobsCaps("model"), fullCaps("auditor"), nil, true},
		{"v6 full executor, full auditor", 6, fullCaps("model"), fullCaps("auditor"), nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, a := liveFixture(t)
			s.Version = c.version
			s.Parameters.AuditEquality = AuditEqualityParameters{Enabled: true, Epsilon: 1, MaxRounds: 3}
			setProfileCaps(t, s, a["node"], func(p *pb.ResourceProfile) { p.EngineCapabilities = c.execCaps })
			setProfileCaps(t, s, a["auditor"], func(p *pb.ResourceProfile) {
				p.EngineCapabilities = c.auditCaps
				if c.auditTweak != nil {
					c.auditTweak(p)
				}
			})
			if got := s.EligibleAuditor(a["auditor"].id, a["node"].id, 0); got != c.want {
				t.Fatalf("eligible = %v, want %v", got, c.want)
			}
		})
	}
}

// convertFixtureToLogprobs rewrites every registered node in a liveFixture
// to the HTTP-engine (logprobs) evidence class on the same manifest.
func convertFixtureToLogprobs(t *testing.T, s *State, a map[string]actor) {
	t.Helper()
	for _, name := range []string{"node", "auditor", "auditor2", "ghost", "extra", "extra2"} {
		setProfileCaps(t, s, a[name], func(p *pb.ResourceProfile) {
			p.EngineCapabilities = logprobsCaps("model", "auditor")
		})
	}
}

// Drill (i): a vLLM-class executor's request is assigned to a vLLM-class
// auditor, whose accept verdict settles the hold. Under the v6 rule the
// assignment itself fails with "no independent auditor available".
func TestLogprobsAuditSettlement(t *testing.T) {
	s, a := liveFixture(t)
	if s.Version != AppVersion {
		t.Fatalf("fixture version = %d, want %d", s.Version, AppVersion)
	}
	s.Parameters.AuditEquality = AuditEqualityParameters{Enabled: true, Epsilon: 1, MaxRounds: 3}
	convertFixtureToLogprobs(t, s, a)
	payerBefore := s.Accounts[a["payer"].id].Balance
	nodeBefore := s.Accounts[a["node"].id].Balance
	apply(t, s, a["payer"], 1, Action{Type: "freeze", HoldID: "lp1", Payer: a["payer"].id, Node: a["node"].id, Amount: 100}, 3)
	receipt := signedReceipt(t, "lp1", a["node"])
	apply(t, s, a["coordinator"], 1, Action{Type: "submit", HoldID: "lp1", Actual: 40, Receipt: receipt}, 4)
	apply(t, s, a["coordinator"], 2, Action{Type: "assign_audit", HoldID: "lp1"}, 5)
	h := s.Holds["lp1"]
	if h.Auditor == "" || h.Auditor == a["node"].id {
		t.Fatalf("no independent logprobs auditor assigned: %+v", h)
	}
	auditor, proof := assignedActor(t, s, a, "lp1")
	ap := s.ActiveProfile(auditor.id, 0)
	if ap == nil || ap.EngineCapabilities == nil || ap.EngineCapabilities.ScoreKind != "logprobs" {
		t.Fatalf("assigned auditor is not logprobs class: %+v", ap)
	}
	challenge, raw, _ := signedAudit(t, receipt, auditor, "accept", proof)
	apply(t, s, auditor, s.Accounts[auditor.id].Nonce+1, Action{Type: "audit", HoldID: "lp1", Challenge: challenge, Verdict: raw}, 6)
	h = s.Holds["lp1"] // Apply replaces hold objects; re-read after each step.
	if h.AuditVerdict != "accept" {
		t.Fatalf("verdict = %q, want accept", h.AuditVerdict)
	}
	apply(t, s, a["coordinator"], 3, Action{Type: "settle", HoldID: "lp1", Outcome: "accept"}, 7)
	h = s.Holds["lp1"]
	if h.State != "settled" {
		t.Fatalf("hold state = %q, want settled", h.State)
	}
	if got := s.Accounts[a["payer"].id].Balance; got != payerBefore-40 {
		t.Fatalf("payer balance = %d, want %d (charged actual only)", got, payerBefore-40)
	}
	if got := s.Accounts[a["node"].id].Balance; got != nodeBefore+40 {
		t.Fatalf("node balance = %d, want %d", got, nodeBefore+40)
	}
	t.Logf("logprobs auditor %s settled hold lp1: payer -40, executor +40", auditor.id)
}

// Drill (ii): a cheating executor (output condemned through the token-class
// margin grade path) is rejected by two distinct logprobs auditors and
// slashed on settlement.
func TestLogprobsCheaterRejected(t *testing.T) {
	s, a := liveFixture(t)
	s.Parameters.AuditEquality = AuditEqualityParameters{Enabled: true, Epsilon: 1, MaxRounds: 3}
	convertFixtureToLogprobs(t, s, a)
	stakeBefore := s.Accounts[a["node"].id].Stake
	apply(t, s, a["payer"], 1, Action{Type: "freeze", HoldID: "lp2", Payer: a["payer"].id, Node: a["node"].id, Amount: 100}, 3)
	receipt := signedReceipt(t, "lp2", a["node"])
	apply(t, s, a["coordinator"], 1, Action{Type: "submit", HoldID: "lp2", Actual: 40, Receipt: receipt}, 4)
	apply(t, s, a["coordinator"], 2, Action{Type: "assign_audit", HoldID: "lp2"}, 5)
	seen := map[string]bool{}
	for i, outcome := range []string{"reject", "reject"} {
		auditor, proof := assignedActor(t, s, a, "lp2")
		if seen[auditor.id] {
			t.Fatalf("round %d redrew the first rejecter", i)
		}
		seen[auditor.id] = true
		challenge, raw, _ := signedAudit(t, receipt, auditor, outcome, proof)
		v := &pb.AuditVerdict{}
		if err := proto.Unmarshal(raw, v); err != nil {
			t.Fatal(err)
		}
		v.Offense = &pb.OffenseGrade{Class: "token", Band: "token-equality-v1", Score: float64(5 - i)}
		if err := core.Sign(v, auditor.key); err != nil {
			t.Fatal(err)
		}
		raw, _ = proto.Marshal(v)
		apply(t, s, auditor, s.Accounts[auditor.id].Nonce+1, Action{Type: "audit", HoldID: "lp2", Challenge: challenge, Verdict: raw}, int64(6+i))
	}
	h := s.Holds["lp2"]
	if h.AuditVerdict != "reject" {
		t.Fatalf("verdict = %q, want reject", h.AuditVerdict)
	}
	apply(t, s, a["coordinator"], 3, Action{Type: "settle", HoldID: "lp2", Outcome: "reject"}, 8)
	if len(s.OffenseRecords) == 0 {
		t.Fatal("no offense record")
	}
	r := s.OffenseRecords[len(s.OffenseRecords)-1]
	if r.Class != "token" || r.Status != "slash" {
		t.Fatalf("offense record = %+v, want token/slash", r)
	}
	if s.Accounts[a["node"].id].Stake >= stakeBefore {
		t.Fatalf("executor stake not slashed: %d >= %d", s.Accounts[a["node"].id].Stake, stakeBefore)
	}
	t.Logf("two logprobs auditors rejected; offense %+v; stake %d -> %d", r, stakeBefore, s.Accounts[a["node"].id].Stake)
}

// Drill (iv), unit level: a v6 state file loads under the v7 binary, a
// manifest activates exactly at its height, and the migration changes
// nothing but the version. A missed activation height is a hard error.
func TestAuditClassActivation(t *testing.T) {
	s, _ := testState(t)
	s.Version = 6
	s.Parameters.AuditEquality = AuditEqualityParameters{Enabled: true, Epsilon: 1, MaxRounds: 3}
	s.Parameters.Liveness = LivenessParameters{}
	s.Parameters.Liveness.defaults()
	s.Height = 10
	paramsBefore, err := json.Marshal(s.Parameters)
	if err != nil {
		t.Fatal(err)
	}
	hashBefore, err := s.Hash()
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/state.json"
	if err := SaveState(path, s); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadState(path)
	if err != nil {
		t.Fatalf("v7 binary refused a v6 state: %v", err)
	}
	if loaded.Version != 6 {
		t.Fatalf("loaded version = %d, want 6", loaded.Version)
	}
	hashLoaded, err := loaded.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if string(hashLoaded) != string(hashBefore) {
		t.Fatal("v6 state hash changed across load under the v7 binary")
	}
	app := &Application{committed: loaded}
	if err := app.ConfigureMembershipUpgrade(MembershipUpgrade{ChainID: loaded.Parameters.ChainID, ActivationHeight: 12}); err != nil {
		t.Fatal(err)
	}
	for _, height := range []int64{11, 12} {
		r, err := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: height})
		if err != nil {
			t.Fatal(err)
		}
		if height == 11 && app.staged.Version != 6 {
			t.Fatal("early activation")
		}
		if height == 12 {
			if app.staged.Version != 7 {
				t.Fatal("missing activation")
			}
			if r.ConsensusParamUpdates == nil || r.ConsensusParamUpdates.Version.App != 7 {
				t.Fatal("consensus app version not raised to 7")
			}
		}
		if _, err = app.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	paramsAfter, err := json.Marshal(app.committed.Parameters)
	if err != nil {
		t.Fatal(err)
	}
	if string(paramsBefore) != string(paramsAfter) {
		t.Fatalf("migration changed parameters:\nbefore %s\nafter  %s", paramsBefore, paramsAfter)
	}
	// Missed activation height: stepping over it must fail loudly.
	s2, _ := testState(t)
	s2.Version = 6
	s2.Parameters.AuditEquality = AuditEqualityParameters{Enabled: true, Epsilon: 1, MaxRounds: 3}
	s2.Height = 10
	app2 := &Application{committed: s2}
	if err := app2.ConfigureMembershipUpgrade(MembershipUpgrade{ChainID: s2.Parameters.ChainID, ActivationHeight: 12}); err != nil {
		t.Fatal(err)
	}
	if _, err := app2.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: 11}); err != nil {
		t.Fatal(err)
	}
	if _, err := app2.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
		t.Fatal(err)
	}
	_, err = app2.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: 13})
	if err == nil || !strings.Contains(err.Error(), "activation height missed") {
		t.Fatalf("missed activation error = %v", err)
	}
}
