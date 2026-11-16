package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	core "ember/core-go"
	pb "ember/proto"
	"ember/services/ledger"
)

var feePolicy = pb.ChainGenesis{PriceInput: 1, PriceOutput: 513}

type changingPriceLedger struct {
	ledger.Backend
	current int64
	frozen  int64
	stamp   []int64
}

func (b *changingPriceLedger) QuoteFee(_ string, input, output uint32) (core.FeeComponents, int64, int64, error) {
	c, err := core.TokenFee(&feePolicy, input, output)
	if err == nil {
		c.Total, err = core.ScaleFee(c.Total, b.current)
	}
	return c, b.current, 41, err
}
func (b *changingPriceLedger) HoldFee(_ string, input, output uint32) (core.FeeComponents, error) {
	c, err := core.TokenFee(&feePolicy, input, output)
	if err == nil {
		c.Total, err = core.ScaleFee(c.Total, b.frozen)
	}
	return c, err
}
func (b *changingPriceLedger) FreezeQuoted(_, _, _ string, _ int64, _ []byte, ppm, height int64) error {
	b.stamp = []int64{ppm, height}
	return nil
}
func (b *changingPriceLedger) FreezePipelineQuoted(_, _ string, _ int64, _ []ledger.StageAuthorization, _ uint32, _ []byte, _, _ int64) error {
	return nil
}

func TestGatewayPinsQuoteAndBillsFrozenRate(t *testing.T) {
	b := &changingPriceLedger{current: 2000000, frozen: 1050000}
	g := &Gateway{Ledger: b, ChainID: "model"}
	quote, ppm, height, err := g.quoteFee(33, 32)
	if err != nil || quote.Total != 32898 || ppm != 2000000 || height != 41 {
		t.Fatal(quote, ppm, height, err)
	}
	if !g.priced() {
		t.Fatal("dynamic profile skipped tokenization")
	}
	if err := g.freeze("id", "payer", "node", quote.Total, []byte{1}, ppm, height); err != nil {
		t.Fatal(err)
	}
	if len(b.stamp) != 2 || b.stamp[0] != ppm || b.stamp[1] != height {
		t.Fatal("quote stamp lost")
	}
	charge, err := g.settlementFee("id", 33, 32)
	if err != nil || charge.Total != 17271 {
		t.Fatal("gateway billed current price", charge, err)
	}
	w := httptest.NewRecorder()
	g.servePricing(w, httptest.NewRequest(http.MethodGet, "/v1/pricing", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"multiplier_ppm":2000000`) {
		t.Fatal(w.Code, w.Body.String())
	}
}
