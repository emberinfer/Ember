package core

import (
	pb "ember/proto"
	"math"
	"testing"
)

func TestTokenFeeRoundingOverflowAndLegacy(t *testing.T) {
	p := &pb.ChainGenesis{PriceInput: 2, PriceOutput: 3, PriceContext: 1}
	c, err := TokenFee(p, 33, 100)
	if err != nil || c != (FeeComponents{Input: 66, Output: 300, Context: 3, Total: 369}) {
		t.Fatalf("%+v %v", c, err)
	}
	for n := uint32(0); n <= 100; n++ {
		v, _ := TokenFee(p, 33, n)
		if v.Total > c.Total {
			t.Fatal("under-reserved")
		}
	}
	// The numerator overflows int64 but the divided result still fits.
	c, err = TokenFee(&pb.ChainGenesis{PriceContext: math.MaxInt64}, 1, 1)
	if err != nil || c.Total != math.MaxInt64/1024 {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err = TokenFee(&pb.ChainGenesis{PriceOutput: math.MaxInt64}, 1, 2); err == nil {
		t.Fatal("accepted overflow")
	}
	if _, err = TokenFee(&pb.ChainGenesis{PriceInput: -1}, 1, 1); err == nil {
		t.Fatal("accepted negative price")
	}
	for _, output := range []uint32{0, 1, 2, 16, 4096, math.MaxUint32} {
		for _, input := range []uint32{0, 32, 8192, math.MaxUint32} {
			v, err := TokenFee(&pb.ChainGenesis{PriceOutput: 7}, input, output)
			if err != nil || v.Total != 7*int64(output) || v.Input != 0 || v.Context != 0 {
				t.Fatal("legacy fee changed")
			}
		}
	}
}
