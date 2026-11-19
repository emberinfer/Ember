package chain

import (
	"bytes"
	"context"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	"google.golang.org/protobuf/proto"
)

func TestV14ParameterActivationHoldDeadlines(t *testing.T) {
	s, a, _, auth, receipt := pipelineState(t)
	s.Version = 13
	s.Parameters.DisputeBlocks, s.Parameters.HoldTimeout = 1, 5000
	apply(t, s, a["payer"], 1, Action{Type: "freeze_pipeline", HoldID: "units", Payer: a["payer"].id, Amount: 6, TotalLayers: 4, PipelineAuthorizations: auth}, 1)
	oldExpiry := s.Holds["units"].ExpireHeight
	s.Holds["inflight-dispute"] = &Hold{ID: "inflight-dispute", State: "dispute", ExpireHeight: 5001, SettleHeight: 100}
	app := &Application{committed: s}
	if err := app.ConfigureMembershipUpgrade(MembershipUpgrade{ChainID: s.Parameters.ChainID, ActivationHeight: 2}); err != nil {
		t.Fatal(err)
	}
	rawReceipt, err := proto.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	submit, err := Sign(s.Parameters.ChainID, a["coordinator"].key, 1, Action{Type: "submit_pipeline", HoldID: "units", Actual: 6, Receipt: rawReceipt, SpotStepCount: 1, CommitmentRoot: bytes.Repeat([]byte{1}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	freeze, err := Sign(s.Parameters.ChainID, a["payer"].key, 2, Action{Type: "freeze_pipeline", HoldID: "new", Payer: a["payer"].id, Amount: 6, TotalLayers: 4, PipelineAuthorizations: auth})
	if err != nil {
		t.Fatal(err)
	}
	for height := int64(2); height <= 3; height++ {
		req := &abci.FinalizeBlockRequest{Height: height}
		if height == 2 {
			req.Txs = [][]byte{submit, freeze}
		}
		res, err := app.FinalizeBlock(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		for _, tx := range res.TxResults {
			if tx.Code != 0 {
				t.Fatal(tx.Log)
			}
		}
		current := app.staged
		if current.Version != 14 || current.Parameters.DisputeBlocks != 5000 || current.Parameters.HoldTimeout != 7000 {
			t.Fatal("migration missing")
		}
		if current.Holds["units"].SettleHeight-current.Holds["units"].SubmitHeight != 5000 {
			t.Fatal("new submit window")
		}
		if current.Holds["new"].ExpireHeight-current.Holds["new"].FreezeHeight != 7000 {
			t.Fatal("new freeze timeout")
		}
		if current.Holds["units"].ExpireHeight != oldExpiry || current.Holds["inflight-dispute"].SettleHeight != 100 || current.Holds["inflight-dispute"].ExpireHeight != 5001 {
			t.Fatal("inflight deadlines rewritten")
		}
		if _, err = app.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
			t.Fatal(err)
		}
	}
}
