package gateway

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	core "ember/core-go"
	chain "ember/services/chain"
	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
)

// ImageRPCBackend is an opt-in chain transport for isolated deployments. The
// account keys must have one transaction writer, as with ChainLedger. It uses
// commit confirmation, never periodic fate queries. No constructor sends a TX.
type ImageRPCBackend struct {
	rpc         *rpchttp.HTTP
	domain      string
	coordinator ed25519.PrivateKey
	accounts    map[string]ed25519.PrivateKey
	mu          *sync.Mutex
}

func NewImageRPCBackend(url, domain string, coordinator ed25519.PrivateKey, accounts map[string]ed25519.PrivateKey) (*ImageRPCBackend, error) {
	if domain == "" || len(coordinator) != ed25519.PrivateKeySize {
		return nil, errors.New("chain domain and coordinator key required")
	}
	rpc, e := rpchttp.New(url)
	if e != nil {
		return nil, e
	}
	keys := map[string]ed25519.PrivateKey{}
	for id, key := range accounts {
		if len(key) != ed25519.PrivateKeySize || core.NodeID(key.Public().(ed25519.PublicKey)) != id {
			return nil, errors.New("image account key mismatch")
		}
		keys[id] = append(ed25519.PrivateKey(nil), key...)
	}
	return &ImageRPCBackend{rpc: rpc, domain: domain, coordinator: append(ed25519.PrivateKey(nil), coordinator...), accounts: keys, mu: &sync.Mutex{}}, nil
}

// NewImageRPCBackendOn shares the daemon's RPC client and transaction lock.
// The host retains ownership of the client and signing credentials.
func NewImageRPCBackendOn(rpc *rpchttp.HTTP, domain string, coordinator ed25519.PrivateKey, accounts map[string]ed25519.PrivateKey, mu *sync.Mutex) (*ImageRPCBackend, error) {
	if rpc == nil || mu == nil || domain == "" || len(coordinator) != ed25519.PrivateKeySize {
		return nil, errors.New("image chain transport required")
	}
	return &ImageRPCBackend{rpc: rpc, domain: domain, coordinator: coordinator, accounts: accounts, mu: mu}, nil
}

func (b *ImageRPCBackend) query(ctx context.Context, path string, out any) (int64, error) {
	result, e := b.rpc.ABCIQuery(ctx, path, nil)
	if e != nil {
		return 0, e
	}
	if result.Response.Code != 0 {
		return 0, fmt.Errorf("image chain query: %s", result.Response.Log)
	}
	return result.Response.Height, json.Unmarshal(result.Response.Value, out)
}
func (b *ImageRPCBackend) Model(ctx context.Context, id string) (chain.Model, error) {
	var m chain.Model
	_, e := b.query(ctx, "/model/"+id, &m)
	return m, e
}
func (b *ImageRPCBackend) Hold(ctx context.Context, id string) (*chain.Hold, int64, error) {
	var h chain.Hold
	height, e := b.query(ctx, "/hold/"+id, &h)
	return &h, height, e
}
func (b *ImageRPCBackend) Quote(ctx context.Context, model string, r chain.ImageRequest) (int64, error) {
	var state chain.State
	if _, e := b.query(ctx, "/state", &state); e != nil {
		return 0, e
	}
	return state.ImageQuote(model, r)
}
func (b *ImageRPCBackend) Submit(ctx context.Context, raw []byte) error {
	result, e := b.rpc.BroadcastTxCommit(ctx, raw)
	if e != nil {
		return e
	}
	if result.CheckTx.Code != 0 {
		return fmt.Errorf("image CheckTx: %s", result.CheckTx.Log)
	}
	if result.TxResult.Code != 0 {
		return fmt.Errorf("image transaction: %s", result.TxResult.Log)
	}
	return nil
}
func (b *ImageRPCBackend) send(ctx context.Context, key ed25519.PrivateKey, a chain.Action) error {
	if len(key) != ed25519.PrivateKeySize {
		return errors.New("image transaction signer unavailable")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var account chain.Account
	if _, e := b.query(ctx, "/account/"+core.NodeID(key.Public().(ed25519.PublicKey)), &account); e != nil {
		return e
	}
	raw, e := chain.Sign(b.domain, key, account.Nonce+1, a)
	if e != nil {
		return e
	}
	return b.Submit(ctx, raw)
}
func (b *ImageRPCBackend) Freeze(ctx context.Context, a chain.Action) error {
	if a.Type != "image_request" {
		return errors.New("image_request required")
	}
	return b.send(ctx, b.accounts[a.Payer], a)
}
func (b *ImageRPCBackend) Apply(ctx context.Context, a chain.Action) error {
	switch a.Type {
	case "assign_audit", "settle":
		return b.send(ctx, b.coordinator, a)
	case "image_received":
		h, _, e := b.Hold(ctx, a.HoldID)
		if e != nil {
			return e
		}
		return b.send(ctx, b.accounts[h.Payer], a)
	default:
		return errors.New("unsupported image coordinator action")
	}
}

// WatchFinalized consumes CometBFT block events. The host runs this once for
// each service, including before accepting requests after a restart. Errors
// are surfaced to its supervisor; reconnect must replay FinalizedBlock.
func (b *ImageRPCBackend) WatchFinalized(ctx context.Context, s *ImageService) error {
	return b.WatchBlocks(ctx, s.FinalizedBlock)
}

// WatchBlocks exposes the same finalized subscription to a host that serializes
// block callbacks with image request completion.
func (b *ImageRPCBackend) WatchBlocks(ctx context.Context, finalized func(context.Context) error, ready ...func()) error {
	if e := b.rpc.Start(); e != nil {
		return e
	}
	defer b.rpc.Stop()
	events, e := b.rpc.Subscribe(ctx, "t2i-gateway", "tm.event='NewBlock'")
	if e != nil {
		return e
	}
	if e = finalized(ctx); e != nil {
		return e
	}
	for _, notify := range ready {
		notify()
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-events:
			if !ok {
				return errors.New("image block subscription closed")
			}
			if e = finalized(ctx); e != nil {
				return e
			}
		}
	}
}
