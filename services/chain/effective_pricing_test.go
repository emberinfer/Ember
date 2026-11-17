package chain

import (
	"bytes"
	"context"
	core "ember/core-go"
	pb "ember/proto"
	"encoding/json"
	abci "github.com/cometbft/cometbft/abci/types"
	"os"
	"path/filepath"
	"testing"
)

func TestV11PriceGatingAndCompatibility(t *testing.T) {
	for _, version := range []uint64{10, 11} {
		s, a, _, _, _ := pricedFixture(t)
		s.Version = version
		before, _ := s.AppHash()
		path := filepath.Join(t.TempDir(), "state.json")
		mustPrice(t, SaveState(path, s))
		raw, _ := os.ReadFile(path)
		if bytes.Contains(raw, []byte("token_prices")) {
			t.Fatal("absent override serialized")
		}
		loaded, err := LoadState(path)
		mustPrice(t, err)
		after, _ := loaded.AppHash()
		if !bytes.Equal(before, after) {
			t.Fatal("hash changed")
		}
		mustPrice(t, SaveState(path, loaded))
		round, _ := os.ReadFile(path)
		if !bytes.Equal(raw, round) {
			t.Fatal("roundtrip bytes changed")
		}
		policy := &RolePricing{4, 3, 80, 15, 5}
		action := Action{Type: "set_pricing", ModelID: "ember-unit-test", Pricing: policy, TokenPrices: &core.TokenPrices{PriceInputPer1K: 229, PriceOutputPer1K: 234, PriceContextPer1K: 0}}
		apply(t, s, a["governor"], 1, action, 2)
		p, err := s.EffectivePricing(action.ModelID)
		mustPrice(t, err)
		if (p.TokenPrices != nil) != (version == 11) {
			t.Fatal("version gate")
		}
		if version == 10 {
			got, err := p.Fee(512, 256)
			mustPrice(t, err)
			want, _ := core.TokenFee(&pb.ChainGenesis{PriceInput: 2, PriceOutput: 3, PriceContext: 1}, 512, 256)
			if got != want {
				t.Fatal(got, want)
			}
		}
	}
}

func TestV11FreezePinsOverride(t *testing.T) {
	s, _, n, _, _ := pricedFixture(t)
	s.Version = 11
	m := s.Models["ember-unit-test"]
	m.TokenPrices = &core.TokenPrices{PriceInputPer1K: 229, PriceOutputPer1K: 234, PriceContextPer1K: 0}
	s.Models[m.ID] = m
	c := tokenCommitment(t, n[0], 512, 256)
	f, err := s.freezeFee(Action{HoldID: "units", Amount: 178, PricingCommitment: c}, m.ID, n[0].id)
	mustPrice(t, err)
	// A later governance table cannot change the pinned override.
	m.TokenPrices.PriceOutputPer1K = 9999
	raw, _ := json.Marshal(f)
	var restored FrozenFee
	mustPrice(t, json.Unmarshal(raw, &restored))
	mustPrice(t, verifyTokenFee(&restored, 512, 256, 178, nil))
	if verifyTokenFee(&restored, 512, 256, 177, nil) == nil {
		t.Fatal("wrong amount accepted")
	}
}

func TestV11ActivationAndTargetedQuery(t *testing.T) {
	s, _, _, _, _ := pricedFixture(t)
	s.Version = 10
	app := NewMemoryApplication()
	app.committed = s
	app.upgrade = &MembershipUpgrade{ChainID: s.Parameters.ChainID, ActivationHeight: 3}
	mustPrice(t, app.activateMembership(s, &abci.FinalizeBlockRequest{Height: 2}))
	if s.Version != 10 {
		t.Fatal("early activation")
	}
	mustPrice(t, app.activateMembership(s, &abci.FinalizeBlockRequest{Height: 3}))
	if s.Version != 11 {
		t.Fatal("activation failed")
	}
	response, err := app.Query(context.Background(), &abci.QueryRequest{Path: "/effective-price/ember-unit-test"})
	mustPrice(t, err)
	var p EffectivePricing
	mustPrice(t, json.Unmarshal(response.Value, &p))
	if p.PriceInput != 2 || p.PriceOutput != 3 || bytes.Contains(response.Value, []byte("accounts")) || bytes.Contains(response.Value, []byte("signed_profile")) {
		t.Fatal(string(response.Value))
	}
}

func TestV11OverrideMultiplierFreeze(t *testing.T) {
	s, _, n, _, _ := pricedFixture(t)
	s.Version = 11
	m := s.Models["ember-unit-test"]
	m.TokenPrices = &core.TokenPrices{PriceInputPer1K: 229, PriceOutputPer1K: 234, PriceContextPer1K: 0}
	s.Models[m.ID] = m
	s.Prices = map[string]*ProfilePrice{m.ID: {MultiplierPPM: 980000, EffectiveHeight: 3}}
	c := tokenCommitment(t, n[0], 512, 256)
	f, err := s.freezeFee(Action{HoldID: "units", Amount: 174, PricingCommitment: c}, m.ID, n[0].id)
	mustPrice(t, err)
	mustPrice(t, verifyTokenFee(f, 512, 256, 174, nil))
	if _, err = s.freezeFee(Action{HoldID: "units", Amount: 178, PricingCommitment: c}, m.ID, n[0].id); err == nil {
		t.Fatal("unscaled freeze accepted")
	}
}
