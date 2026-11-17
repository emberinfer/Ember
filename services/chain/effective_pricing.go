package chain

import (
	core "ember/core-go"
	pb "ember/proto"
	"errors"
	"google.golang.org/protobuf/proto"
)

// EffectivePricing contains only data needed to quote a profile.
type EffectivePricing struct {
	RolePricing     *RolePricing      `json:"role_pricing,omitempty"`
	TokenPrices     *core.TokenPrices `json:"token_prices,omitempty"`
	PriceInput      int64             `json:"price_input"`
	PriceOutput     int64             `json:"price_output"`
	PriceContext    int64             `json:"price_context"`
	MultiplierPPM   int64             `json:"multiplier_ppm"`
	EffectiveHeight int64             `json:"effective_height"`
}

func (s *State) EffectivePricing(id string) (EffectivePricing, error) {
	m := s.Models[id]
	raw, _ := modelArtifacts(m)
	g := &pb.ChainGenesis{}
	if m.ID == "" || len(raw) == 0 || proto.Unmarshal(raw, g) != nil {
		return EffectivePricing{}, errors.New("missing anchored pricing profile")
	}
	p := s.PriceForProfile(id)
	q := EffectivePricing{RolePricing: m.Pricing, PriceInput: g.PriceInput, PriceOutput: g.PriceOutput, PriceContext: g.PriceContext, MultiplierPPM: p.MultiplierPPM, EffectiveHeight: p.EffectiveHeight}
	if s.Version >= 11 {
		q.TokenPrices = m.TokenPrices
	}
	return q, nil
}
func (p EffectivePricing) Fee(input, output uint32) (core.FeeComponents, error) {
	c, err := core.EffectiveTokenFee(&pb.ChainGenesis{PriceInput: p.PriceInput, PriceOutput: p.PriceOutput, PriceContext: p.PriceContext}, p.TokenPrices, input, output)
	if err != nil {
		return c, err
	}
	c.Total, err = core.ScaleFee(c.Total, p.MultiplierPPM)
	return c, err
}
