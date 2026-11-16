package chain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	core "ember/core-go"
	pb "ember/proto"
	abci "github.com/cometbft/cometbft/abci/types"
	"google.golang.org/protobuf/proto"
)

func migrationFixture(t *testing.T) ([]byte, MembershipUpgrade) {
	t.Helper()
	s, _ := membershipFixture(t, 2)
	u := MembershipUpgrade{ChainID: s.Parameters.ChainID, ActivationHeight: 30, Membership: *s.Membership}
	s.Membership = nil
	s.Version = 2
	s.Holds["old"] = &Hold{ID: "old", State: "settled", Amount: 13, Actual: 9, VerdictBytes: []byte{1, 2, 3}}
	raw, _ := s.Bytes()
	var fields map[string]json.RawMessage
	json.Unmarshal(raw, &fields)
	var params map[string]json.RawMessage
	json.Unmarshal(fields["parameters"], &params)
	for _, k := range []string{"slash_floor_bps", "slash_repeat_window_blocks", "assignment", "audit_work", "misjudge", "evidence"} {
		delete(params, k)
	}
	fields["parameters"], _ = json.Marshal(params)
	raw, _ = json.Marshal(fields)
	return raw, u
}

func TestV2MigrationDefaultsDeterminismAndRoundTrip(t *testing.T) {
	raw, u := migrationFixture(t)
	s, err := MigrateV2State(raw, u)
	if err != nil {
		t.Fatal(err)
	}
	again, err := MigrateV2State(raw, u)
	if err != nil {
		t.Fatal(err)
	}
	h1, _ := s.Hash()
	h2, _ := again.Hash()
	if !bytes.Equal(h1, h2) {
		t.Fatal("nondeterministic migration")
	}
	if s.Version != 4 || s.Membership != nil || len(s.Offenses) != 0 || s.Height != 2 {
		t.Fatal("bridge boundary", s)
	}
	p := s.Parameters
	if p.SlashFloorBPS != 100 || p.SlashRepeatWindow != 1000 || p.Assignment != (AssignmentParameters{3, 8}) || p.AuditWork != (AuditWorkParameters{1000, 64}) || p.Misjudge.SlashMode != "gray_gate" || p.Misjudge.CapBlockBPS != 500 || len(p.Misjudge.Thresholds) != 0 || p.Evidence.FrameTokens() != 512 {
		t.Fatal("audit parameter defaults", p)
	}
	old := s.Holds["old"]
	if old.TokenFee != nil || old.AuditCompPolicy != nil || old.AuditWorkPolicy != (AuditWorkParameters{}) || old.AuditInputTokens != 0 || old.AuditOutputTokens != 0 || old.AuditEscrow != 0 || old.FirstGrade != nil || old.RequestID != "" || len(old.Assignments) != 0 || old.Amount != 13 || old.Actual != 9 || !bytes.Equal(old.VerdictBytes, []byte{1, 2, 3}) {
		t.Fatal("hold defaults/preservation", old)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(path); err == nil || err.Error() != "unsupported state version" {
		t.Fatal("ungated load", err)
	}
	if _, err := OpenApplication(path); err == nil || err.Error() != "unsupported state version" {
		t.Fatal("ungated open", err)
	}
	app, err := OpenApplicationWithUpgrade(path, &u)
	if err != nil {
		t.Fatal(err)
	}
	info, err := app.Info(context.Background(), &abci.InfoRequest{})
	legacy := sha256.Sum256(raw)
	if err != nil || info.AppVersion != 4 || !bytes.Equal(info.LastBlockAppHash, legacy[:]) {
		t.Fatal("load path", info, err)
	}
	if bytes.Equal(legacy[:], h1) {
		t.Fatal("fixture cannot distinguish legacy and migrated hashes")
	}
	unchanged, _ := os.ReadFile(path)
	if !bytes.Equal(unchanged, raw) {
		t.Fatal("load wrote source")
	}
	if err := SaveState(path, s); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	h3, _ := loaded.Hash()
	if !bytes.Equal(h1, h3) {
		t.Fatal("round trip hash")
	}
	if _, err := OpenApplicationWithUpgrade(path, &u); err != nil {
		t.Fatal(err)
	}
	bad := u
	bad.ActivationHeight = s.Height
	if _, err := MigrateV2State(raw, bad); err == nil {
		t.Fatal("past activation accepted")
	}
	bad = u
	bad.ChainID = "other"
	if _, err := MigrateV2State(raw, bad); err == nil {
		t.Fatal("wrong chain accepted")
	}
}

func TestV2MigrationPreservesPresentPoliciesAndSignedBytes(t *testing.T) {
	raw, u := migrationFixture(t)
	var s State
	json.Unmarshal(raw, &s)
	s.Parameters.SlashFloorBPS = 17
	s.Parameters.SlashRepeatWindow = 71
	s.Parameters.Assignment = AssignmentParameters{5, 7}
	s.Parameters.Misjudge = MisjudgeParameters{SlashMode: "off", GrayRepeatLimit: 9, WindowBlocks: 3, CapBlockBPS: 11}
	s.Parameters.AuditWork = AuditWorkParameters{20, 21}
	s.Models["later"] = Model{ID: "later", Manifest: []byte{9, 8, 7}, Genesis: []byte{6, 5, 4}, Pricing: &RolePricing{1, 2, 80, 15, 5}}
	s.Holds["old"].ModelID = "later"
	s.Holds["old"].ProducerProfile = []byte{255, 0, 3}
	s.Holds["old"].Shares = &SettlementShares{Fee: 9, Executor: 7, Auditor: 1, Consumer: 1}
	s.Offenses = map[string][]int64{"invented": {1}}
	raw, _ = s.Bytes()
	migrated, err := MigrateV2State(raw, u)
	if err != nil {
		t.Fatal(err)
	}
	beforeParams, _ := json.Marshal(s.Parameters)
	afterParams, _ := json.Marshal(migrated.Parameters)
	if !bytes.Equal(beforeParams, afterParams) {
		t.Fatal("present parameters replaced")
	}
	for _, pair := range [][2]any{{s.Models, migrated.Models}, {s.Holds, migrated.Holds}, {s.Accounts, migrated.Accounts}, {s.Nodes, migrated.Nodes}} {
		before, _ := json.Marshal(pair[0])
		after, _ := json.Marshal(pair[1])
		if !bytes.Equal(before, after) {
			t.Fatal("present value replaced")
		}
	}
	if len(migrated.Offenses) != 0 {
		t.Fatal("confirmed offenses backfilled")
	}
}

func TestMigratedLegacyHoldTransitions(t *testing.T) {
	for _, outcome := range []string{"accept", "reject", "service_failure"} {
		t.Run(outcome, func(t *testing.T) {
			s, a := testState(t)
			s.Version = 4
			receipt := signedReceipt(t, "legacy", a["node"])
			s.Accounts[a["payer"].id].Balance -= 100
			s.Holds["legacy"] = &Hold{ID: "legacy", Payer: a["payer"].id, Node: a["node"].id, Amount: 100, State: "frozen", FreezeHeight: 1, ExpireHeight: 100}
			apply(t, s, a["coordinator"], 1, Action{Type: "submit", HoldID: "legacy", Receipt: receipt, Actual: 10}, 3)
			apply(t, s, a["coordinator"], 2, Action{Type: "assign_audit", HoldID: "legacy"}, 4)
			h := s.Holds["legacy"]
			digest := sha256.New()
			digest.Write([]byte("ember/audit-selection/v1\x00"))
			digest.Write(s.Entropy)
			digest.Write([]byte(h.ID))
			digest.Write([]byte(h.ReceiptRef))
			digest.Write([]byte{1})
			if h.AuditProof != hex.EncodeToString(digest.Sum(nil)) || len(h.Assignments) != 0 {
				t.Fatal("legacy selector domain")
			}
			for round := 0; round < 2; round++ {
				auditor, proof := assignedActor(t, s, a, "legacy")
				verdictOutcome := outcome
				if outcome == "service_failure" {
					verdictOutcome = "inconclusive"
				}
				challenge, verdict, _ := signedAudit(t, receipt, auditor, verdictOutcome, proof)
				v := &pb.AuditVerdict{}
				proto.Unmarshal(verdict, v)
				v.Offense = nil
				if err := core.Sign(v, auditor.key); err != nil {
					t.Fatal(err)
				}
				verdict, _ = proto.Marshal(v)
				apply(t, s, auditor, s.Accounts[auditor.id].Nonce+1, Action{Type: "audit", HoldID: "legacy", Challenge: challenge, Verdict: verdict}, int64(5+round))
				if outcome != "reject" {
					break
				}
				// Reopen between the two legacy rounds: no synthetic first grade is needed.
				path := filepath.Join(t.TempDir(), "state.json")
				if err := SaveState(path, s); err != nil {
					t.Fatal(err)
				}
				var err error
				s, err = LoadState(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			apply(t, s, a["coordinator"], 3, Action{Type: "settle", HoldID: "legacy", Outcome: outcome}, 8)
			h = s.Holds["legacy"]
			wantPayer, wantStake, wantNode := int64(1000), int64(5000), int64(0)
			switch outcome {
			case "accept":
				wantPayer = 990
				wantNode = 10
			case "reject":
				wantPayer = 1200
				wantStake = 4800
			}
			if s.Accounts[a["payer"].id].Balance != wantPayer || s.Accounts[a["node"].id].Stake != wantStake || s.Accounts[a["node"].id].Balance != wantNode || len(s.OffenseRecords) != 0 || h.FirstGrade != nil {
				t.Fatal("legacy accounting", h)
			}
		})
	}
}

func TestV2MigrationHandshakeBridge(t *testing.T) {
	raw, u := migrationFixture(t)
	legacy := sha256.Sum256(raw)

	s, err := MigrateV2State(raw, u)
	if err != nil {
		t.Fatal(err)
	}
	bridge, err := s.AppHash()
	if err != nil || !bytes.Equal(bridge, legacy[:]) {
		t.Fatal("bridge hash", bridge, err)
	}
	migrated, err := s.Hash()
	if err != nil || bytes.Equal(migrated, bridge) {
		t.Fatal("migrated root must differ from bridge hash in this fixture", migrated)
	}
	again, err := MigrateV2State(raw, u)
	if err != nil {
		t.Fatal(err)
	}
	againBridge, _ := again.AppHash()
	againRoot, _ := again.Hash()
	if !bytes.Equal(againBridge, bridge) || !bytes.Equal(againRoot, migrated) {
		t.Fatal("bridge or root hash nondeterministic")
	}
	// Only the state object produced by the migration load presents the
	// bridge; clones (trial/proposal/read paths) fall back to Hash().
	clone, err := s.Clone()
	if err != nil {
		t.Fatal(err)
	}
	cloneHash, _ := clone.AppHash()
	if !bytes.Equal(cloneHash, migrated) {
		t.Fatal("clone must not present the bridge hash")
	}

	// Boundary restart: loading the untouched v2 file again reproduces
	// the same bridge presentation, and the load does not rewrite it.
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	app, err := OpenApplicationWithUpgrade(path, &u)
	if err != nil {
		t.Fatal(err)
	}
	info, err := app.Info(context.Background(), &abci.InfoRequest{})
	if err != nil || info.LastBlockHeight != s.Height || !bytes.Equal(info.LastBlockAppHash, legacy[:]) {
		t.Fatal("boundary info", info, err)
	}
	unchanged, _ := os.ReadFile(path)
	if !bytes.Equal(unchanged, raw) {
		t.Fatal("load wrote source")
	}

	// The first block past the boundary ends the bridge: FinalizeBlock
	// already speaks the migrated hash chain, and after Commit, Info
	// (and any later restart from the persisted v4 file) agrees with it.
	resp, err := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: s.Height + 1, Hash: bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(resp.AppHash, legacy[:]) {
		t.Fatal("finalize block still presenting bridge hash")
	}
	if _, err := app.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
		t.Fatal(err)
	}
	info, err = app.Info(context.Background(), &abci.InfoRequest{})
	if err != nil || info.LastBlockHeight != s.Height+1 || !bytes.Equal(info.LastBlockAppHash, resp.AppHash) {
		t.Fatal("post-boundary info", info, err)
	}
	reopened, err := OpenApplicationWithUpgrade(path, &u)
	if err != nil {
		t.Fatal(err)
	}
	info, err = reopened.Info(context.Background(), &abci.InfoRequest{})
	if err != nil || !bytes.Equal(info.LastBlockAppHash, resp.AppHash) {
		t.Fatal("post-commit restart info", info, err)
	}
}
