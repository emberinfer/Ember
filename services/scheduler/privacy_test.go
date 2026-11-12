package scheduler

import "testing"

func TestPrivacyTierNeverDowngrades(t *testing.T) {
	if _, err := Choose(Request{Tier: "P1"}, nil); err == nil || err.Error() != "TIER_UNAVAILABLE" {
		t.Fatal(err)
	}
	if _, err := ChoosePipeline(Request{Tier: "P1"}, nil, 1); err == nil || err.Error() != "TIER_UNAVAILABLE" {
		t.Fatal(err)
	}
	if _, err := PlanPipeline(Request{Tier: "P1"}, nil, 1); err == nil || err.Error() != "TIER_UNAVAILABLE" {
		t.Fatal(err)
	}
	if _, err := PlanRanges(Request{Tier: "P1"}, nil, 1); err == nil || err.Error() != "TIER_UNAVAILABLE" {
		t.Fatal(err)
	}
	if len(Rank(Request{Tier: "P1"}, nil)) != 0 {
		t.Fatal("P1 ranked")
	}
}
