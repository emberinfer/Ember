package ledger

import (
	core "ember/core-go"
	pb "ember/proto"
	chain "ember/services/chain"
	"errors"
	"google.golang.org/protobuf/proto"
)

// ConsumerPricing reads the chain's one price signal. It never measures demand.
type ConsumerPricing interface {
	QuoteFee(model string, input, output uint32) (core.FeeComponents, int64, int64, error)
	HoldFee(id string, input, output uint32) (core.FeeComponents, error)
}

func priceComponents(raw []byte, input, output uint32, ppm int64) (core.FeeComponents, error) {
	g := &pb.ChainGenesis{}
	if len(raw) == 0 || proto.Unmarshal(raw, g) != nil {
		return core.FeeComponents{}, errors.New("missing anchored fee profile")
	}
	c, err := core.TokenFee(g, input, output)
	if err != nil {
		return c, err
	}
	// Component values remain base-table amounts; Total is the payable gross fee.
	c.Total, err = core.ScaleFee(c.Total, ppm)
	return c, err
}
func (l *ChainLedger) QuoteFee(model string, input, output uint32) (core.FeeComponents, int64, int64, error) {
	var s chain.State
	if err := l.query("/state", &s); err != nil {
		return core.FeeComponents{}, 0, 0, err
	}
	m := s.Models[model]
	raw := m.Genesis
	if m.Publication != nil {
		raw = m.Publication.Genesis
	}
	p := s.PriceForProfile(model)
	c, err := priceComponents(raw, input, output, p.MultiplierPPM)
	return c, p.MultiplierPPM, p.EffectiveHeight, err
}
func (l *ChainLedger) HoldFee(id string, input, output uint32) (core.FeeComponents, error) {
	h, err := l.hold(id)
	if err != nil {
		return core.FeeComponents{}, err
	}
	if h.TokenFee == nil {
		return core.FeeComponents{}, errors.New("missing frozen fee")
	}
	ppm := h.TokenFee.MultiplierPPM
	if ppm == 0 {
		ppm = chain.PriceScale
	}
	return priceComponents(h.TokenFee.Profile, input, output, ppm)
}

// QuotedFreezer binds a signed freeze to the quote's exact rate and height.
// A stale quote is rejected before funds are locked.
type QuotedFreezer interface {
	FreezeQuoted(id, payer, node string, amount int64, commitment []byte, ppm, height int64) error
	FreezePipelineQuoted(id, payer string, amount int64, auth []StageAuthorization, total uint32, commitment []byte, ppm, height int64) error
}

func (l *ChainLedger) FreezeQuoted(id, payer, node string, amount int64, commitment []byte, ppm, height int64) error {
	return l.withAccount(payer, chain.Action{Type: "freeze", HoldID: id, Payer: payer, Node: node, Amount: amount, PricingCommitment: commitment, ExpectedPrice: &chain.PriceStamp{MultiplierPPM: ppm, EffectiveHeight: height}})
}
func (l *ChainLedger) FreezePipelineQuoted(id, payer string, amount int64, auth []StageAuthorization, total uint32, commitment []byte, ppm, height int64) error {
	a := make([]chain.PipelineAuthorization, len(auth))
	for i, v := range auth {
		a[i] = chain.PipelineAuthorization{NodeID: v.NodeID, StartLayer: v.StartLayer, EndLayer: v.EndLayer}
	}
	return l.withAccount(payer, chain.Action{Type: "freeze_pipeline", HoldID: id, Payer: payer, Amount: amount, TotalLayers: total, PipelineAuthorizations: a, PricingCommitment: commitment, ExpectedPrice: &chain.PriceStamp{MultiplierPPM: ppm, EffectiveHeight: height}})
}
