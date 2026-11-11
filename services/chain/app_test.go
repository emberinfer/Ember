package chain

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
)

func TestApplicationCommitAndRestart(t *testing.T) {
	state, actors := testState(t)
	genesis := GenesisState{Parameters: state.Parameters,
		Coordinators: []string{actors["coordinator"].id}, Auditors: []string{actors["auditor"].id}, Governors: []string{actors["governor"].id}}
	for id, account := range state.Accounts {
		genesis.Accounts = append(genesis.Accounts, GenesisAccount{ID: id, Balance: account.Balance, Stake: account.Stake})
	}
	rawGenesis, err := json.Marshal(genesis)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	app, err := OpenApplication(path)
	if err != nil {
		t.Fatal(err)
	}
	init, err := app.InitChain(context.Background(), &abci.InitChainRequest{ChainId: state.Parameters.ChainID, AppStateBytes: rawGenesis})
	if err != nil || len(init.AppHash) != 32 {
		t.Fatalf("init: %v", err)
	}
	if _, err = app.InitChain(context.Background(), &abci.InitChainRequest{ChainId: state.Parameters.ChainID, AppStateBytes: rawGenesis}); err != nil {
		t.Fatalf("idempotent height-zero init: %v", err)
	}
	tx, err := Sign(state.Parameters.ChainID, actors["payer"].key, 1, Action{Type: "transfer", To: actors["node"].id, Amount: 7})
	if err != nil {
		t.Fatal(err)
	}
	bad, err := Sign(state.Parameters.ChainID, actors["payer"].key, 1, Action{Type: "transfer", To: actors["node"].id, Amount: 8})
	if err != nil {
		t.Fatal(err)
	}
	response, err := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: 1, Txs: [][]byte{tx, bad}})
	if err != nil {
		t.Fatal(err)
	}
	if response.TxResults[0].Code != 0 || response.TxResults[1].Code == 0 {
		t.Fatalf("unexpected tx results: %+v", response.TxResults)
	}
	if _, err = app.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenApplication(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := restarted.State()
	if err != nil {
		t.Fatal(err)
	}
	if got.Height != 1 || got.Accounts[actors["payer"].id].Balance != 993 || got.Accounts[actors["node"].id].Balance != 7 || got.Accounts[actors["payer"].id].Nonce != 1 {
		t.Fatalf("unexpected restarted state: %+v", got.Accounts)
	}
	if _, err = restarted.InitChain(context.Background(), &abci.InitChainRequest{ChainId: state.Parameters.ChainID, AppStateBytes: rawGenesis}); err == nil {
		t.Fatal("initialized committed chain twice")
	}
}
