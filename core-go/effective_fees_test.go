package core

import (
	pb "ember/proto"
	"testing"
)

func TestEffectiveLegacyUnchanged(t *testing.T) {
	for _, g := range []*pb.ChainGenesis{{PriceOutput: 1}, {PriceInput: 2, PriceOutput: 3, PriceContext: 1}, {PriceInput: 0, PriceOutput: 513}} {
		for _, in := range []uint32{0, 1, 33, 512} {
			for _, out := range []uint32{0, 1, 256, 1001} {
				want, e := TokenFee(g, in, out)
				got, err := EffectiveTokenFee(g, nil, in, out)
				if e != err || want != got {
					t.Fatal(got, want)
				}
				a, _ := ScaleFee(got.Total, 980000)
				b, _ := ScaleFee(want.Total, 980000)
				if a != b {
					t.Fatal(a, b)
				}
			}
		}
	}
	c, _ := EffectiveTokenFee(&pb.ChainGenesis{PriceOutput: 1}, nil, 512, 256)
	n, _ := ScaleFee(c.Total, 980000)
	if n != 250 {
		t.Fatal(n)
	}
}
func TestOverrideActivationTable(t *testing.T) {
	for _, tc := range []struct {
		id            string
		in, out, i, o int64
	}{
		{"ember-qwen2.5-1.5b-unified-v1", 229, 234, 118, 60},
		{"ember-qwen3.6-35b-a3b-fp8-unified-v1", 458, 571, 235, 147},
		{"ember-olmoe-1b-7b-moe-v1", 195, 217, 100, 56},
		{"ember-kimi-k3.1-pro-v1", 16660, 25630, 8530, 6562},
	} {
		t.Run(tc.id, func(t *testing.T) {
			c, e := EffectiveTokenFee(nil, &TokenPrices{tc.in, tc.out, 0}, 512, 256)
			if e != nil || c.Input != tc.i || c.Output != tc.o || c.Context != 0 {
				t.Fatal(c, e)
			}
			scaled, _ := ScaleFee(c.Total, 980000)
			if scaled != (tc.i+tc.o)*980000/1000000 {
				t.Fatal(scaled)
			}
		})
	}
	for _, tc := range []struct {
		p    int64
		n    uint32
		want int64
	}{{229, 1000, 229}, {1, 1, 1}, {0, 512, 0}} {
		c, e := EffectiveTokenFee(nil, &TokenPrices{tc.p, 0, 0}, tc.n, 0)
		if e != nil || c.Input != tc.want {
			t.Fatal(c, e)
		}
	}
	c, e := EffectiveTokenFee(nil, &TokenPrices{0, 0, 1000}, 32, 32)
	if e != nil || c.Context != 1 {
		t.Fatal(c, e)
	}
}
