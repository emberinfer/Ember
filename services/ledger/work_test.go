package ledger

import (
	"math"
	"testing"
)

func TestSplitStageChargeConservesIntegerSpark(t *testing.T) {
	stages := []StageWork{{NodeID: "a", StartLayer: 0, EndLayer: 9, Tokens: 7}, {NodeID: "b", StartLayer: 9, EndLayer: 18, Tokens: 7}, {NodeID: "c", StartLayer: 18, EndLayer: 28, Tokens: 7}}
	for _, amount := range []int64{0, 1, 2, 7, 28, 1001, math.MaxInt64} {
		shares, err := SplitStageCharge(amount, stages, 28, 7)
		if err != nil {
			t.Fatal(err)
		}
		var sum int64
		for _, s := range shares {
			sum += s.Amount
			if s.Amount < 0 {
				t.Fatal("negative share")
			}
		}
		if sum != amount {
			t.Fatalf("%d != %d", sum, amount)
		}
		if amount == 28 && (shares[0].Amount != 9 || shares[1].Amount != 9 || shares[2].Amount != 10) {
			t.Fatal(shares)
		}
	}
}

func TestSplitStageChargeRejectsInvalidWork(t *testing.T) {
	for _, stages := range [][]StageWork{
		{{NodeID: "a", StartLayer: 0, EndLayer: 14, Tokens: 1}, {NodeID: "b", StartLayer: 15, EndLayer: 28, Tokens: 1}},
		{{NodeID: "a", StartLayer: 0, EndLayer: 14, Tokens: 1}, {NodeID: "b", StartLayer: 13, EndLayer: 28, Tokens: 1}},
		{{NodeID: "a", StartLayer: 0, EndLayer: 14, Tokens: 1}, {NodeID: "a", StartLayer: 14, EndLayer: 28, Tokens: 1}},
		{{NodeID: "a", StartLayer: 0, EndLayer: 14, Tokens: 1}, {NodeID: "b", StartLayer: 14, EndLayer: 28, Tokens: 2}},
		{{NodeID: "a", StartLayer: 0, EndLayer: 14, Tokens: 1}},
	} {
		if _, err := SplitStageCharge(100, stages, 28, 1); err == nil {
			t.Fatal("invalid work accepted", stages)
		}
	}
}
