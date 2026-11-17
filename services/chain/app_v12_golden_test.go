package chain

import (
	"bytes"
	"context"
	"fmt"
	abci "github.com/cometbft/cometbft/abci/types"
	"testing"
	"time"
)

func TestParamV12Golden(t *testing.T) {
	s, actors := testState(t)
	s.Version = 12
	app := &Application{committed: s}
	golden := []string{"c499f2a19702ed9d90a8b5e69fa5a142355c7361ba35037321536cff50cec429", "dd53050faac36482f2ce50b556b4167ae93d88697dd9eeab9616b7dff6e572b5", "62dc3b416815047aa2e02d21a8bc7238af86a7964c5d3c7d46d1c1b01fd131c1"}
	for h := int64(1); h <= 3; h++ {
		tx, err := Sign(s.Parameters.ChainID, actors["payer"].key, uint64(h), Action{Type: "transfer", To: actors["governor"].id, Amount: h})
		if err != nil {
			t.Fatal(err)
		}
		res, err := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: h, Time: time.Unix(1700000000+h, 0), Hash: bytes.Repeat([]byte{byte(h)}, 32), Txs: [][]byte{tx}})
		if err != nil {
			t.Fatal(err)
		}
		if res.TxResults[0].Code != 0 {
			t.Fatal(res.TxResults[0])
		}
		got := fmt.Sprintf("%x", res.AppHash)
		t.Logf("height=%d apphash=%s", h, got)
		if len(golden) > 0 && got != golden[h-1] {
			t.Fatalf("hash: got %s want %s", got, golden[h-1])
		}
		if res.ConsensusParamUpdates != nil {
			t.Fatal("unchanged v12 emitted consensus update")
		}
		if _, err := app.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
			t.Fatal(err)
		}
	}
}
