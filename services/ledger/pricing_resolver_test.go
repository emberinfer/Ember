package ledger

import (
	core "ember/core-go"
	chain "ember/services/chain"
	"errors"
	"testing"
)

func TestPricingResolver(t *testing.T) {
	var r pricingResolver
	fail := func(string, any) error { return errors.New("offline") }
	if _, e := r.resolve("model", fail); e == nil {
		t.Fatal("quoted without chain price")
	}
	fetch := func(path string, v any) error {
		if path != "/effective-price/model" {
			t.Fatal(path)
		}
		*v.(*chain.EffectivePricing) = chain.EffectivePricing{TokenPrices: &core.TokenPrices{229, 234, 0}, MultiplierPPM: 980000, EffectiveHeight: 5}
		return nil
	}
	p, e := r.resolve("model", fetch)
	if e != nil {
		t.Fatal(e)
	}
	c, e := p.Fee(512, 256)
	if e != nil || c.Total != 174 {
		t.Fatal(c, e)
	}
	old, e := r.resolve("model", fail)
	if e != nil || old != p {
		t.Fatal(old, e)
	}
	if _, e = r.resolve("other", fail); e == nil {
		t.Fatal("cross-profile cache")
	}
}
