package chain

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestParametersLegacyByteIdentical(t *testing.T) {
	// Produce the exact v9 field order, with its absent adjudication key.
	type plain Parameters
	legacy, err := json.Marshal(struct {
		plain
		SupplyDemand  *PriceParameters         `json:"supply_demand,omitempty"`
		AuditSampling *AuditSamplingParameters `json:"audit_sampling,omitempty"`
		Liveness      *LivenessParameters      `json:"liveness,omitempty"`
		AuditEquality *AuditEqualityParameters `json:"audit_equality,omitempty"`
	}{})
	if err != nil {
		t.Fatal(err)
	}
	legacy = bytes.Replace(legacy, []byte(`"adjudication":{"enabled":false,"appeal_window_blocks":0,"max_auditors":0,"budget_units":0,"deposit_units":0},`), nil, 1)
	var p Parameters
	if err = json.Unmarshal(legacy, &p); err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(p)
	if err != nil || !bytes.Equal(legacy, got) {
		t.Fatalf("legacy bytes changed: %s %v", got, err)
	}
}

func TestParametersAdjudicationRoundTrip(t *testing.T) {
	for _, a := range []AdjudicationParameters{{Enabled: true, AppealWindowBlocks: 39268, MaxAuditors: 3, BudgetUnits: 3, DepositUnits: 2}, {AppealWindowBlocks: 17}, {Enabled: true}} {
		p := Parameters{Adjudication: a}
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		var got Parameters
		if err = json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		again, err := json.Marshal(got)
		if err != nil || got.Adjudication != a || !bytes.Equal(raw, again) {
			t.Fatalf("configured values lost: %s %v", again, err)
		}
	}
}

func TestAdjudicationDepositJSONRoundTrip(t *testing.T) {
	for _, d := range []AdjudicationDeposit{{}, {From: "payer", Locked: 30}, {From: "payer", Locked: 30, Settled: true, Outcome: Upheld}} {
		c := AdjudicationCase{Deposit: d}
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		if d == (AdjudicationDeposit{}) && bytes.Contains(raw, []byte(`"deposit"`)) {
			t.Fatal("empty deposit emitted")
		}
		var got AdjudicationCase
		if err = json.Unmarshal(raw, &got); err != nil || got.Deposit != d {
			t.Fatalf("deposit round trip: %+v %v", got.Deposit, err)
		}
		again, err := json.Marshal(got)
		if err != nil || !bytes.Equal(raw, again) {
			t.Fatal("deposit bytes changed")
		}
	}
}
