package chain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	abci "github.com/cometbft/cometbft/abci/types"
)

const invalidTxCode uint32 = 1

type Application struct {
	abci.BaseApplication
	mu        sync.Mutex
	path      string
	committed *State
	staged    *State
}

func OpenApplication(path string) (*Application, error) {
	app := &Application{path: path}
	state, err := LoadState(path)
	if err == nil {
		app.committed = state
		return app, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	return app, nil
}

func NewMemoryApplication() *Application { return &Application{} }

func (app *Application) Info(context.Context, *abci.InfoRequest) (*abci.InfoResponse, error) {
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.committed == nil {
		return &abci.InfoResponse{AppVersion: AppVersion}, nil
	}
	hash, err := app.committed.Hash()
	if err != nil {
		return nil, err
	}
	return &abci.InfoResponse{Data: "emberchain", AppVersion: AppVersion, LastBlockHeight: app.committed.Height, LastBlockAppHash: hash}, nil
}

func (app *Application) InitChain(_ context.Context, req *abci.InitChainRequest) (*abci.InitChainResponse, error) {
	app.mu.Lock()
	defer app.mu.Unlock()
	var genesis GenesisState
	if len(req.AppStateBytes) == 0 || json.Unmarshal(req.AppStateBytes, &genesis) != nil {
		return nil, errors.New("invalid ember genesis state")
	}
	state, err := NewState(genesis)
	if err != nil {
		return nil, err
	}
	if req.ChainId != state.Parameters.ChainID {
		return nil, errors.New("CometBFT and Ember chain IDs differ")
	}
	if app.committed != nil {
		if app.committed.Height != 0 {
			return nil, errors.New("chain already initialized")
		}
		got, gotErr := app.committed.Hash()
		want, wantErr := state.Hash()
		if gotErr != nil || wantErr != nil || !equal(got, want) {
			return nil, errors.New("persisted height-zero state differs from genesis")
		}
		hash := append([]byte(nil), got...)
		return &abci.InitChainResponse{AppHash: hash}, nil
	}
	app.committed = state
	hash, err := state.Hash()
	if err != nil {
		return nil, err
	}
	if app.path != "" {
		if err = SaveState(app.path, state); err != nil {
			return nil, err
		}
	}
	return &abci.InitChainResponse{AppHash: hash}, nil
}

func txError(err error) *abci.CheckTxResponse {
	return &abci.CheckTxResponse{Code: invalidTxCode, Log: err.Error()}
}

func (app *Application) CheckTx(_ context.Context, req *abci.CheckTxRequest) (*abci.CheckTxResponse, error) {
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.committed == nil {
		return txError(errors.New("chain not initialized")), nil
	}
	trial, err := app.committed.Clone()
	if err == nil {
		trial.finalized = app.committed
		_, err = trial.Apply(req.Tx, app.committed.Height+1)
	}
	if err != nil {
		return txError(err), nil
	}
	return &abci.CheckTxResponse{Code: abci.CodeTypeOK, GasWanted: 1}, nil
}

func (app *Application) PrepareProposal(_ context.Context, req *abci.PrepareProposalRequest) (*abci.PrepareProposalResponse, error) {
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.committed == nil {
		return &abci.PrepareProposalResponse{}, nil
	}
	trial, err := app.committed.Clone()
	if err != nil {
		return nil, err
	}
	trial.finalized = app.committed
	var size int64
	selected := make([][]byte, 0, len(req.Txs))
	for _, raw := range req.Txs {
		if size+int64(len(raw)) > req.MaxTxBytes {
			continue
		}
		next, cloneErr := trial.Clone()
		if cloneErr != nil {
			return nil, cloneErr
		}
		if _, applyErr := next.Apply(raw, req.Height); applyErr == nil {
			trial, size = next, size+int64(len(raw))
			selected = append(selected, raw)
		}
	}
	return &abci.PrepareProposalResponse{Txs: selected}, nil
}

func (app *Application) ProcessProposal(_ context.Context, req *abci.ProcessProposalRequest) (*abci.ProcessProposalResponse, error) {
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.committed == nil {
		return &abci.ProcessProposalResponse{Status: abci.PROCESS_PROPOSAL_STATUS_REJECT}, nil
	}
	trial, err := app.committed.Clone()
	if err != nil {
		return nil, err
	}
	trial.finalized = app.committed
	for _, raw := range req.Txs {
		next, cloneErr := trial.Clone()
		if cloneErr != nil {
			return nil, cloneErr
		}
		if _, applyErr := next.Apply(raw, req.Height); applyErr != nil {
			return &abci.ProcessProposalResponse{Status: abci.PROCESS_PROPOSAL_STATUS_REJECT}, nil
		}
		trial = next
	}
	return &abci.ProcessProposalResponse{Status: abci.PROCESS_PROPOSAL_STATUS_ACCEPT}, nil
}

func (app *Application) FinalizeBlock(_ context.Context, req *abci.FinalizeBlockRequest) (*abci.FinalizeBlockResponse, error) {
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.committed == nil {
		return nil, errors.New("chain not initialized")
	}
	trial, err := app.committed.Clone()
	if err != nil {
		return nil, err
	}
	trial.finalized = app.committed
	results := make([]*abci.ExecTxResult, len(req.Txs))
	for i, raw := range req.Txs {
		next, cloneErr := trial.Clone()
		if cloneErr != nil {
			return nil, cloneErr
		}
		txHash, applyErr := next.Apply(raw, req.Height)
		if applyErr != nil {
			results[i] = &abci.ExecTxResult{Code: invalidTxCode, Log: applyErr.Error()}
			continue
		}
		trial = next
		results[i] = &abci.ExecTxResult{Code: abci.CodeTypeOK, Data: []byte(txHash), GasWanted: 1, GasUsed: 1}
	}
	// Height is consensus state even for an empty block.
	trial.Height = req.Height
	trial.Entropy = append([]byte(nil), req.Hash...)
	if trial.BlockHashes == nil {
		trial.BlockHashes = map[int64][]byte{}
	}
	trial.BlockHashes[req.Height] = append([]byte(nil), req.Hash...)
	hash, err := trial.Hash()
	if err != nil {
		return nil, err
	}
	trial.finalized = nil
	app.staged = trial
	return &abci.FinalizeBlockResponse{TxResults: results, AppHash: hash}, nil
}

func (app *Application) Commit(context.Context, *abci.CommitRequest) (*abci.CommitResponse, error) {
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.staged == nil {
		return &abci.CommitResponse{}, nil
	}
	if app.path != "" {
		if err := SaveState(app.path, app.staged); err != nil {
			return nil, err
		}
	}
	app.committed, app.staged = app.staged, nil
	return &abci.CommitResponse{}, nil
}

func (app *Application) Query(_ context.Context, req *abci.QueryRequest) (*abci.QueryResponse, error) {
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.committed == nil {
		return queryError(req.Path, errors.New("chain not initialized")), nil
	}
	var value any
	switch {
	case req.Path == "/latency":
		value = app.committed.Latencies
		if app.committed.Latencies == nil {
			value = map[string]*LatencyStats{}
		}
	case req.Path == "/state":
		value = app.committed
	case req.Path == "/publishing":
		value = app.committed.EffectivePublishing()
	case req.Path == "/endorsements":
		value = app.committed.Endorsements
		if app.committed.Endorsements == nil {
			value = map[string]map[string]Endorsement{}
		}
	case req.Path == "/parameters":
		value = app.committed.Parameters
	case strings.HasPrefix(req.Path, "/account/"):
		value = app.committed.Accounts[strings.TrimPrefix(req.Path, "/account/")]
	case strings.HasPrefix(req.Path, "/hold/"):
		value = app.committed.Holds[strings.TrimPrefix(req.Path, "/hold/")]
	case strings.HasPrefix(req.Path, "/model/"):
		model, ok := app.committed.Models[strings.TrimPrefix(req.Path, "/model/")]
		if ok {
			value = model
		}
	case strings.HasPrefix(req.Path, "/node/"):
		value = app.committed.Nodes[strings.TrimPrefix(req.Path, "/node/")]
	case req.Path == "/nodes":
		value = app.committed.Nodes
	case strings.HasPrefix(req.Path, "/coverage/"):
		coverage, err := app.committed.CoverageForNode(strings.TrimPrefix(req.Path, "/coverage/"))
		if err != nil {
			return queryError(req.Path, err), nil
		}
		value = coverage
	case req.Path == "/coverage":
		coverage := map[string]CoverageSnapshot{}
		for id := range app.committed.Nodes {
			if c, err := app.committed.CoverageForNode(id); err == nil {
				coverage[id] = c
			}
		}
		value = coverage
	case req.Path == "/auditors":
		value = app.committed.Auditors
	default:
		return queryError(req.Path, errors.New("unknown query path")), nil
	}
	if value == nil {
		return queryError(req.Path, errors.New("not found")), nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &abci.QueryResponse{Code: abci.CodeTypeOK, Key: []byte(req.Path), Value: raw, Height: app.committed.Height}, nil
}

func queryError(path string, err error) *abci.QueryResponse {
	return &abci.QueryResponse{Code: invalidTxCode, Key: []byte(path), Log: err.Error()}
}

func StatePath(root string) string { return filepath.Join(root, "data", "ember-state.json") }

func (app *Application) State() (*State, error) {
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.committed == nil {
		return nil, fmt.Errorf("chain not initialized")
	}
	return app.committed.Clone()
}
