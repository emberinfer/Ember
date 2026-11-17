package ledger

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	pb "ember/proto"
	chain "ember/services/chain"
	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	"google.golang.org/protobuf/proto"
)

func TestChainPricingUsesAnchoredQuoteAndFrozenHold(t *testing.T) {
	raw, _ := proto.Marshal(&pb.ChainGenesis{PriceInput: 1, PriceOutput: 513})
	s := chain.State{Models: map[string]chain.Model{"model": {ID: "model", Genesis: raw}}, Prices: map[string]*chain.ProfilePrice{"model": {MultiplierPPM: 2000000, EffectiveHeight: 41}}}
	h := chain.Hold{TokenFee: &chain.FrozenFee{Profile: raw, MultiplierPPM: 1050000, EffectiveHeight: 21}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Path string `json:"path"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		var value any
		switch req.Params.Path {
		case "/effective-price/model":
			value, _ = s.EffectivePricing("model")
		case "/effective-price/unknown":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"response":{"code":1,"log":"unknown profile"}}}`, req.ID)
			return
		case "/hold/request":
			value = h
		case "/model/unknown":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"response":{"code":1,"log":"not found"}}}`, req.ID)
			return
		default:
			t.Error("unexpected query", req.Params.Path)
			return
		}
		data, _ := json.Marshal(value)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"response":{"code":0,"value":"%s"}}}`, req.ID, base64.StdEncoding.EncodeToString(data))
	}))
	defer server.Close()
	rpc, err := rpchttp.New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	l := &ChainLedger{rpc: rpc}
	quote, ppm, height, err := l.QuoteFee("model", 33, 32)
	if err != nil || quote.Total != 32898 || ppm != 2000000 || height != 41 {
		t.Fatal(quote, ppm, height, err)
	}
	charge, err := l.HoldFee("request", 33, 32)
	if err != nil || charge.Total != 17271 {
		t.Fatal("settlement used current rate", charge, err)
	}
	// Legacy snapshots default to 1.0; malformed snapshots fail closed.
	h.TokenFee.MultiplierPPM = 0
	charge, err = l.HoldFee("request", 33, 32)
	if err != nil || charge.Total != 16449 {
		t.Fatal(charge, err)
	}
	h.TokenFee = nil
	if _, err = l.HoldFee("request", 33, 32); err == nil {
		t.Fatal("missing frozen policy accepted")
	}
	if _, _, _, err = l.QuoteFee("unknown", 33, 32); err == nil {
		t.Fatal("unknown profile accepted")
	}
}
