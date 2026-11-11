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
