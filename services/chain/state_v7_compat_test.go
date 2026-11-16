package chain

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression tests for the two v8 activation blockers found by ACT-1
// (2026-11-15):
//
//  1. TASK25 deleted CoverageSnapshot.ExecutorMultiplier /
//     AuditorMultiplier. Any v7 state containing a settled hold lost
//     those keys on re-serialization, so the v8 binary computed a
//     different AppHash than the v7 consensus hash and panicked at the
//     handshake replay. The scratch drills never caught it because
//     their v7 phase had no settled holds.
//  2. Pre-governance chains carry an empty Governors set, leaving
//     set_pricing / freeze_model_registration without any lawful
//     signer after activation. The 7->8 migration now seeds an empty
//     Governors set from the chain's (genesis-fixed) coordinators.

// v7ProductionShares is the byte shape of a real settled hold's shares
// object on ember-testnet-1 (settled at height 202, profile
// ember-qwen2.5-1.5b-s22-cpu-v1): coverage targets 2 executors /
// 3 auditors, actual 2 / 1, hence multipliers 1000000 / 1666666.
const v7ProductionShares = `{"fee":45,"executor":33,"auditor":10,"consumer":2,"coverage":{"height":202,"profile":"ember-qwen2.5-1.5b-s22-cpu-v1","runtime":"transformers/4.45.2|torch=2.5.1+cpu","executors":2,"auditors":1,"target_executors":2,"target_auditors":3,"executor_multiplier_ppm":1000000,"auditor_multiplier_ppm":1666666}}`

func TestV7SettledSharesRoundTrip(t *testing.T) {
	var shares SettlementShares
	if err := json.Unmarshal([]byte(v7ProductionShares), &shares); err != nil {
		t.Fatal(err)
	}
	if shares.Coverage.ExecutorMultiplier != 1000000 || shares.Coverage.AuditorMultiplier != 1666666 {
		t.Fatalf("v7 coverage multipliers lost on load: %+v", shares.Coverage)
	}
	got, err := json.Marshal(shares)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != v7ProductionShares {
		t.Fatalf("v7 shares bytes changed:\nwant %s\ngot  %s", v7ProductionShares, got)
	}
}

func TestCoverageMultipliersUseV7Formula(t *testing.T) {
	for _, tc := range []struct{ target, actual, want int64 }{
		{2, 2, 1000000},
		{3, 3, 1000000},
		{3, 1, 1666666},
		{3, 0, 2000000},
		{1, 5, 1000000},
	} {
		if got := multiplier(tc.target, tc.actual); got != tc.want {
			t.Fatalf("multiplier(%d,%d)=%d want %d", tc.target, tc.actual, got, tc.want)
		}
	}
	s, a := pricingState(t)
	h := pricingHold(s, a)
	policy := s.Models[h.ModelID].Pricing
	c := s.coverage(h.Node, s.ActiveProfile(h.Node, s.Height), policy)
	if c.ExecutorMultiplier != multiplier(policy.TargetExecutors, c.Executors) ||
		c.AuditorMultiplier != multiplier(policy.TargetAuditors, c.Auditors) {
		t.Fatalf("coverage snapshot not populated with v7 multipliers: %+v", c)
	}
}

// A v7-shaped state (settled hold with a coverage snapshot, plus a
// v2 audit-comp policy on the model) must survive LoadState -> Bytes
// byte-identically, with Hash() equal to the sha256 of the v7 bytes:
// that equality is exactly the AppHash the v7 chain committed.
func TestV7StateWithSettledHoldRoundTrip(t *testing.T) {
	s, a := testState(t)
	s.Version = 7
	var shares SettlementShares
	if err := json.Unmarshal([]byte(v7ProductionShares), &shares); err != nil {
		t.Fatal(err)
	}
	s.Holds["settled-1"] = &Hold{ID: "settled-1", Payer: a["payer"].id, Node: a["node"].id, State: "settled", Actual: 45, Shares: &shares}
	model := s.Models["ember-test-model"]
	model.AuditComp = &AuditCompParameters{FormulaVer: 2, MarginBPS: 10000, AuditShareBPS: 1500, BenchmarkRef: strings.Repeat("ab", 32)}
	s.Models[model.ID] = model

	raw, err := s.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"executor_multiplier_ppm":1000000`)) ||
		!bytes.Contains(raw, []byte(`"auditor_multiplier_ppm":1666666`)) {
		t.Fatal("settled coverage multipliers missing from serialized state")
	}
	if bytes.Contains(raw, []byte("r_seg_per_krow")) {
		t.Fatal("RC-only r_seg_per_krow leaked into a v7-shaped audit-comp policy")
	}
	path := filepath.Join(t.TempDir(), "ember-state.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := loaded.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, got) {
		t.Fatal("v7 state bytes changed across load/reserialize")
	}
	want := sha256.Sum256(raw)
	hash, err := loaded.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want[:], hash) {
		t.Fatal("reloaded state hash differs from v7 committed AppHash")
	}
}

func TestMigrateAuditSamplingSeedsGovernorsFromCoordinators(t *testing.T) {
	s, a := testState(t)
	s.Version = 7
	s.Governors = map[string]bool{}
	s.migrateAuditSampling()
	if s.Version != 8 {
		t.Fatal("migration did not reach version 8")
	}
	if len(s.Governors) != 1 || !s.Governors[a["coordinator"].id] {
		t.Fatalf("governors not seeded from coordinators: %v", s.Governors)
	}
	// The seeded coordinator is a lawful set_pricing signer; a
	// non-governor is not.
	apply(t, s, a["coordinator"], 1, Action{Type: "set_pricing", ModelID: "ember-test-model", Pricing: &RolePricing{TargetExecutors: 2, TargetAuditors: 3, ExecutorWeight: 80, AuditorWeight: 15, ConsumerWeight: 5}}, 10)
	if s.Models["ember-test-model"].Pricing == nil {
		t.Fatal("set_pricing by seeded governor had no effect")
	}
	raw, err := Sign(s.Parameters.ChainID, a["payer"].key, 1, Action{Type: "set_pricing", ModelID: "ember-test-model", Pricing: &RolePricing{TargetExecutors: 2, TargetAuditors: 3, ExecutorWeight: 80, AuditorWeight: 15, ConsumerWeight: 5}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(raw, 10); err == nil {
		t.Fatal("set_pricing by non-governor accepted")
	}
}

func TestMigrateAuditSamplingSeedsProductionCoordinator(t *testing.T) {
	// ember-testnet-1's sole coordinator (genesis-fixed) is the
	// coordinator-mandated Governors seed for the testnet.
	const coordinator = "90ef25ac87a1a59c3476123af952075a093354b6545df5117a5393d40fc565d0"
	s, _ := testState(t)
	s.Version = 7
	s.Governors = map[string]bool{}
	s.Coordinators = map[string]bool{coordinator: true}
	s.migrateAuditSampling()
	if len(s.Governors) != 1 || !s.Governors[coordinator] {
		t.Fatalf("production-shaped seed wrong: %v", s.Governors)
	}
}

func TestMigrateAuditSamplingKeepsExistingGovernors(t *testing.T) {
	s, a := testState(t)
	s.Version = 7
	before := map[string]bool{a["governor"].id: true}
	s.Governors = map[string]bool{a["governor"].id: true}
	s.migrateAuditSampling()
	if len(s.Governors) != 1 || !s.Governors[a["governor"].id] {
		t.Fatalf("existing governors rewritten: %v (was %v)", s.Governors, before)
	}
}
