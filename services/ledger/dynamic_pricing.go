package ledger

import (
	core "ember/core-go"
	pb "ember/proto"
	chain "ember/services/chain"
	"errors"
	"google.golang.org/protobuf/proto"
	"log"
	"sync"
)

// ConsumerPricing reads the chain's one price signal. It never measures demand.
type ConsumerPricing interface {
	QuoteFee(model string, input, output uint32) (core.FeeComponents, int64, int64, error)
	HoldFee(id string, input, output uint32) (core.FeeComponents, error)
}

// pricingResolver retains the last successful chain quote, never genesis prices.
type pricingResolver struct {
	mu    sync.Mutex
	cache map[string]chain.EffectivePricing
}

func (r *pricingResolver) resolve(model string, fetch func(string, any) error) (chain.EffectivePricing, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var p chain.EffectivePricing
	if err := fetch("/effective-price/"+model, &p); err != nil {
		log.Printf("WARNING: CHAIN PRICING QUERY FAILED for %s; using last known price if available: %v", model, err)
		if old, ok := r.cache[model]; ok {
			return old, nil
		}
		return p, err
	}
	if r.cache == nil {
		r.cache = map[string]chain.EffectivePricing{}
	}
	r.cache[model] = p
	return p, nil
}
func (l *ChainLedger) QuoteFee(model string, input, output uint32) (core.FeeComponents, int64, int64, error) {
	p, err := l.pricing.resolve(model, l.query)
	if err != nil {
		return core.FeeComponents{}, 0, 0, err
	}
	c, err := p.Fee(input, output)
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
	g := &pb.ChainGenesis{}
	if len(h.TokenFee.Profile) == 0 {
		return core.FeeComponents{}, errors.New("missing frozen profile")
	}
	if err := proto.Unmarshal(h.TokenFee.Profile, g); err != nil {
		return core.FeeComponents{}, err
	}
	return (chain.EffectivePricing{TokenPrices: h.TokenFee.TokenPrices, PriceInput: g.PriceInput, PriceOutput: g.PriceOutput, PriceContext: g.PriceContext, MultiplierPPM: ppm}).Fee(input, output)
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

// Pricing exposes the same targeted response used for gateway quotes.
func (l *ChainLedger) Pricing(model string) (chain.EffectivePricing, error) {
	return l.pricing.resolve(model, l.query)
}
