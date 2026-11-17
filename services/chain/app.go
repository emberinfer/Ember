package chain

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	abci "github.com/cometbft/cometbft/abci/types"
	cmtproto "github.com/cometbft/cometbft/api/cometbft/types/v1"
)

const invalidTxCode uint32 = 1

const BlockMaxBytesV13 int64 = 8388608

// BlockMaxGas is the verified current chain value carried unchanged into v13.
const BlockMaxGas int64 = 10000000

type Application struct {
	abci.BaseApplication
	mu                 sync.Mutex
	path               string
	committed          *State
	staged             *State
	stagedBytes        []byte
	upgrade            *MembershipUpgrade
	upgradeFromVersion uint64
}

func OpenApplication(path string) (*Application, error) {
	return OpenApplicationWithUpgrade(path, nil)
}

// OpenApplicationWithUpgrade enables the reviewed v2 bridge before loading state.
func OpenApplicationWithUpgrade(path string, upgrade *MembershipUpgrade) (*Application, error) {
	app := &Application{path: path}
	state, err := LoadStateWithUpgrade(path, upgrade)
	if err == nil {
		app.committed = state
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if upgrade != nil {
		if err := app.ConfigureMembershipUpgrade(*upgrade); err != nil {
			return nil, err
		}
	}
	return app, nil
}

func NewMemoryApplication() *Application { return &Application{} }

func (app *Application) Info(context.Context, *abci.InfoRequest) (*abci.InfoResponse, error) {
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.committed == nil {
		if app.upgrade != nil {
			return &abci.InfoResponse{AppVersion: 4}, nil
		}
		return &abci.InfoResponse{AppVersion: AppVersion}, nil
	}
	hash, err := app.committed.AppHash()
	if err != nil {
		return nil, err
	}
	return &abci.InfoResponse{Data: "emberchain", AppVersion: app.committed.Version, LastBlockHeight: app.committed.Height, LastBlockAppHash: hash}, nil
}

func (app *Application) InitChain(_ context.Context, req *abci.InitChainRequest) (*abci.InitChainResponse, error) {
	app.mu.Lock()
	defer app.mu.Unlock()
	var genesis GenesisState
	if len(req.AppStateBytes) == 0 || json.Unmarshal(req.AppStateBytes, &genesis) != nil {
		return nil, errors.New("invalid ember genesis state")
	}
	if genesis.Parameters.GenesisIssuance != nil && genesis.Parameters.GenesisIssuance.GenesisTime != req.Time.Unix() {
		return nil, errors.New("issuance genesis time differs from consensus genesis")
	}
	state, err := NewState(genesis)
	if err != nil {
		return nil, err
	}
	if req.ChainId != state.Parameters.ChainID {
		return nil, errors.New("CometBFT and Ember chain IDs differ")
	}
	if app.upgrade != nil {
		if genesis.Membership != nil || req.ChainId != app.upgrade.ChainID {
			return nil, errors.New("legacy replay manifest requires matching legacy genesis")
		}
		if err := app.upgrade.Membership.validate(state); err != nil {
			return nil, err
		}
		state.Version = 4
		state.Parameters.Liveness = LivenessParameters{}
	}
	if state.Membership != nil {
		if err := state.Membership.matchGenesis(req.Validators); err != nil {
			return nil, err
		}
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
	trial, err := app.committed.blockCopy()
	if err == nil {
		trial.finalized = app.committed
		if trial.Version >= 5 && trial.Version <= 10 {
			err = app.activateMembershipTrial(trial, app.committed.Height+1)
		}
		if err == nil {
			_, err = trial.Apply(req.Tx, app.committed.Height+1)
		}
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
	trial, err := app.committed.blockCopy()
	if err != nil {
		return nil, err
	}
	if err := trial.AdvanceTime(req.Time.Unix()); err != nil {
		return nil, err
	}
	trial.finalized = app.committed
	if trial.Version >= 5 && trial.Version <= 10 {
		if err := app.activateMembershipTrial(trial, req.Height); err != nil {
			return nil, err
		}
	}
	if err := trial.advanceSubsidyTime(req.Time.Unix()); err != nil {
		return nil, err
	}
	var size int64
	selected := make([][]byte, 0, len(req.Txs))
	for _, raw := range req.Txs {
		if size+int64(len(raw)) > req.MaxTxBytes {
			continue
		}
		next, cloneErr := proposalTrial(trial)
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
	trial, err := app.committed.blockCopy()
	if err != nil {
		return nil, err
	}
	if err := trial.AdvanceTime(req.Time.Unix()); err != nil {
		return &abci.ProcessProposalResponse{Status: abci.PROCESS_PROPOSAL_STATUS_REJECT}, nil
	}
	trial.finalized = app.committed
	if trial.Version >= 5 && trial.Version <= 10 {
		if err := app.activateMembershipTrial(trial, req.Height); err != nil {
			return nil, err
		}
	}
	if err := trial.advanceSubsidyTime(req.Time.Unix()); err != nil {
		return &abci.ProcessProposalResponse{Status: abci.PROCESS_PROPOSAL_STATUS_REJECT}, nil
	}
	for _, raw := range req.Txs {
		next, cloneErr := proposalTrial(trial)
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
	trial, err := app.committed.blockCopy()
	if err != nil {
		return nil, err
	}
	if err := trial.AdvanceTime(req.Time.Unix()); err != nil {
		return nil, err
	}
	trial.finalized = app.committed
	if err := app.activateMembership(trial, req); err != nil {
		return nil, err
	}
	trial.observeValidatorVotes(req)
	if err := trial.advanceSubsidyTime(req.Time.Unix()); err != nil {
		return nil, err
	}
	results := make([]*abci.ExecTxResult, len(req.Txs))
	for i, raw := range req.Txs {
		next, cloneErr := proposalTrial(trial)
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
	updates := trial.finalizeMembership(req.Height)
	if err := trial.finishPriceEpoch(req.Height); err != nil {
		return nil, err
	}
	trial.pruneBlockHashes()
	trial.archiveTerminalHolds()
	var hash, stagedBytes []byte
	if trial.Version >= 11 {
		stagedBytes, err = trial.Bytes()
		sum := sha256.Sum256(stagedBytes)
		hash = sum[:]
	} else {
		hash, err = trial.Hash()
	}
	if err != nil {
		return nil, err
	}
	trial.finalized = nil
	app.staged = trial
	app.stagedBytes = stagedBytes
	response := &abci.FinalizeBlockResponse{TxResults: results, AppHash: hash, ValidatorUpdates: updates}
	if trial.Version >= 13 {
		response.ConsensusParamUpdates = &cmtproto.ConsensusParams{
			Version: &cmtproto.VersionParams{App: trial.Version},
			Block:   &cmtproto.BlockParams{MaxBytes: BlockMaxBytesV13, MaxGas: BlockMaxGas},
		}
	} else if trial.Version != app.committed.Version {
		response.ConsensusParamUpdates = &cmtproto.ConsensusParams{Version: &cmtproto.VersionParams{App: trial.Version}}
	}
	return response, nil
}

func (app *Application) Commit(context.Context, *abci.CommitRequest) (*abci.CommitResponse, error) {
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.staged == nil {
		return &abci.CommitResponse{}, nil
	}
	if app.path != "" {
		var err error
		if app.staged.Version >= 11 {
			err = persistArchive(app.path, app.staged, app.stagedBytes)
			if err == nil {
				err = durableWrite(app.path, app.stagedBytes)
			}
		} else {
			err = SaveState(app.path, app.staged)
		}
		if err != nil {
			return nil, err
		}
	}
	if app.staged.Version >= 11 && app.path != "" && app.staged.history != nil {
		app.staged.history.head = archiveKey(app.stagedBytes)
		app.staged.history.pendingHashes = nil
		app.staged.history.pendingHolds = nil
	}
	app.committed, app.staged = app.staged, nil
	app.stagedBytes = nil
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
	case strings.HasPrefix(req.Path, "/privacy/"):
		parts := strings.SplitN(strings.TrimPrefix(req.Path, "/privacy/"), "/", 2)
		if len(parts) != 2 || app.committed.Privacy == nil {
			return queryError(req.Path, errors.New("not found")), nil
		}
		p := app.committed.Privacy
		switch parts[0] {
		case "request":
			if v := p.Requests[parts[1]]; v != nil {
				value = v
			}
		case "epoch":
			if v := p.Epochs[parts[1]]; v != nil {
				value = v
			}
		case "operator":
			if v, ok := p.Operators[parts[1]]; ok {
				value = v
			}
		case "request-by-bundle":
			ids := make([]string, 0, len(p.Requests))
			for id := range p.Requests {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for _, id := range ids {
				r := p.Requests[id]
				match := r.BundleRef == parts[1]
				for _, seg := range r.Segments {
					match = match || seg.BundleRef == parts[1]
				}
				if match {
					value = r
					break
				}
			}
		}
	case strings.HasPrefix(req.Path, "/audit-route/"):
		v, err := app.committed.QueryAuditRoute(strings.TrimPrefix(req.Path, "/audit-route/"))
		if err != nil {
			return queryError(req.Path, err), nil
		}
		value = v
	case strings.HasPrefix(req.Path, "/settlement-receipts/"):
		u, err := url.Parse(req.Path)
		if err != nil {
			return queryError(req.Path, err), nil
		}
		h := app.committed.LookupHold(strings.TrimPrefix(u.Path, "/settlement-receipts/"))
		if h == nil {
			return queryError(req.Path, errors.New("not found")), nil
		}
		receipts, err := app.committed.ExpectedSettlementAuditReceipts(h, u.Query().Get("outcome"), app.committed.Height+1)
		if err != nil {
			return queryError(req.Path, err), nil
		}
		value = receipts
	case req.Path == "/expired-holds":
		value = app.committed.ExpiredHolds()
	case strings.HasPrefix(req.Path, "/liveness/"):
		id := strings.TrimPrefix(req.Path, "/liveness/")
		if app.committed.Nodes[id] == nil {
			return queryError(req.Path, errors.New("not found")), nil
		}
		value = map[string]any{"nodes": map[string]*NodeRegistration{id: app.committed.Nodes[id]}, "height": app.committed.Height, "parameters": map[string]any{"liveness": app.committed.Parameters.Liveness}}
	case strings.HasPrefix(req.Path, "/node-status/"):
		v := app.committed.QueryNodeStatus(strings.TrimPrefix(req.Path, "/node-status/"))
		if v != nil {
			value = v
		}
	case req.Path == "/node-statuses":
		out := map[string]*NodeStatusView{}
		for id := range app.committed.Nodes {
			out[id] = app.committed.QueryNodeStatus(id)
		}
		value = out
	case strings.HasPrefix(req.Path, "/model-trust/"):
		id := strings.TrimPrefix(req.Path, "/model-trust/")
		if m, ok := app.committed.Models[id]; ok {
			endorsements := map[string]map[string]Endorsement{}
			if m.Publication != nil {
				root := m.Publication.ManifestRoot
				endorsements[root] = app.committed.Endorsements[root]
			}
			value = map[string]any{"models": map[string]Model{id: m}, "governors": app.committed.Governors, "endorsements": endorsements}
		}
	case req.Path == "/scheduling":
		value = map[string]any{"parameters": map[string]any{"scheduling": app.committed.Parameters.Scheduling}, "latencies": app.committed.Latencies}
	case strings.HasPrefix(req.Path, "/contribution-admission?"):
		u, err := url.Parse(req.Path)
		if err != nil {
			return queryError(req.Path, err), nil
		}
		var a Action
		if err = json.Unmarshal([]byte(u.Query().Get("action")), &a); err != nil {
			return queryError(req.Path, err), nil
		}
		if err = app.committed.CheckContributionAdmission(a, app.committed.Height+1); err != nil {
			return queryError(req.Path, err), nil
		}
		value = true
	case req.Path == "/validators":
		value = app.committed.Membership
	case req.Path == "/genesis-subsidy":
		value = app.committed.GenesisSubsidy
	case req.Path == "/latency":
		value = app.committed.Latencies
		if app.committed.Latencies == nil {
			value = map[string]*LatencyStats{}
		}
	case strings.HasPrefix(req.Path, "/effective-price/"):
		id := strings.TrimPrefix(req.Path, "/effective-price/")
		p, err := app.committed.EffectivePricing(id)
		if err != nil {
			return queryError(req.Path, err), nil
		}
		value = p
	case strings.HasPrefix(req.Path, "/price/"):
		id := strings.TrimPrefix(req.Path, "/price/")
		if app.committed.Models[id].ID == "" {
			return queryError(req.Path, errors.New("unknown profile")), nil
		}
		value = app.committed.PriceForProfile(id)
	case req.Path == "/subsidy-monthly":
		value = app.committed.subsidyMonthly()
	case req.Path == "/subsidy":
		value = app.committed.Issuance
	case req.Path == "/governor-metrics":
		value = app.committed.governorMetrics()
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
		value = app.committed.LookupHold(strings.TrimPrefix(req.Path, "/hold/"))
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
	return app.committed.blockCopy()
}
