package core

import (
	"errors"
	"math/big"

	pb "ember/proto"
)

const ContextDivisor int64 = 1024

type FeeComponents struct {
	Input   int64 `json:"input"`
	Output  int64 `json:"output"`
	Context int64 `json:"context"`
	Total   int64 `json:"total"`
}

// TokenFee floors only after multiplying both token counts. Intermediate
// products may exceed int64 even when the final charge fits.
func TokenFee(p *pb.ChainGenesis, input, output uint32) (FeeComponents, error) {
	var c FeeComponents
	if p == nil || p.PriceInput < 0 || p.PriceOutput < 0 || p.PriceContext < 0 {
		return c, errors.New("invalid token prices")
	}
	n, m := new(big.Int).SetUint64(uint64(input)), new(big.Int).SetUint64(uint64(output))
	i := new(big.Int).Mul(big.NewInt(p.PriceInput), n)
	o := new(big.Int).Mul(big.NewInt(p.PriceOutput), m)
	x := new(big.Int).Mul(big.NewInt(p.PriceContext), n)
	x.Mul(x, m).Quo(x, big.NewInt(ContextDivisor))
	total := new(big.Int).Add(i, o)
	total.Add(total, x)
	if !total.IsInt64() {
		return c, errors.New("token fee overflow")
	}
	return FeeComponents{Input: i.Int64(), Output: o.Int64(), Context: x.Int64(), Total: total.Int64()}, nil
}

// ScaleFee applies the one consumer multiplier after the base table formula.
// Historical callers use ppm=1000000; all arithmetic is exact until final floor.
func ScaleFee(base, ppm int64) (int64, error) {
	if base < 0 || ppm <= 0 {
		return 0, errors.New("invalid price multiplier")
	}
	n := new(big.Int).Mul(big.NewInt(base), big.NewInt(ppm))
	n.Quo(n, big.NewInt(1000000))
	if !n.IsInt64() {
		return 0, errors.New("scaled fee overflow")
	}
	return n.Int64(), nil
}

// TokenPrices is an explicitly present, complete per-1000-token override.
// A present zero context price is distinct from an absent override.
type TokenPrices struct {
	PriceInputPer1K   int64 `json:"price_input_per_1k"`
	PriceOutputPer1K  int64 `json:"price_output_per_1k"`
	PriceContextPer1K int64 `json:"price_context_per_1k"`
}

// EffectiveTokenFee selects override ?? artifact. Override input/output terms
// each ceil-divide by 1000; context ceil-divides by 1000*1024, preserving the
// interaction structure and interpreting context in the same per-1K units.
// Callers apply ScaleFee AFTER summing these base terms (existing floor).
// The absent-override path delegates unchanged to the historical formula.
func EffectiveTokenFee(g *pb.ChainGenesis, p *TokenPrices, input, output uint32) (FeeComponents, error) {
	if p == nil {
		return TokenFee(g, input, output)
	}
	if p.PriceInputPer1K < 0 || p.PriceOutputPer1K < 0 || p.PriceContextPer1K < 0 {
		return FeeComponents{}, errors.New("invalid token prices")
	}
	ceil := func(price int64, a, b uint32, divisor int64) *big.Int {
		n := new(big.Int).Mul(big.NewInt(price), new(big.Int).SetUint64(uint64(a)))
		n.Mul(n, new(big.Int).SetUint64(uint64(b)))
		n.Add(n, big.NewInt(divisor-1)).Quo(n, big.NewInt(divisor))
		return n
	}
	i, o, x := ceil(p.PriceInputPer1K, input, 1, 1000), ceil(p.PriceOutputPer1K, output, 1, 1000), ceil(p.PriceContextPer1K, input, output, 1000*ContextDivisor)
	total := new(big.Int).Add(i, o)
	total.Add(total, x)
	if !total.IsInt64() {
		return FeeComponents{}, errors.New("token fee overflow")
	}
	return FeeComponents{Input: i.Int64(), Output: o.Int64(), Context: x.Int64(), Total: total.Int64()}, nil
}
