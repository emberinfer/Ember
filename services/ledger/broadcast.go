package ledger

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/cometbft/cometbft/types"
)

const ConfirmationTimeout = 120 * time.Second
const confirmationPollInterval = 500 * time.Millisecond

type confirmationRPC interface {
	BroadcastTxSync(context.Context, types.Tx) (*coretypes.ResultBroadcastTx, error)
	Tx(context.Context, []byte, bool) (*coretypes.ResultTx, error)
}

// UnconfirmedError identifies a transaction whose inclusion is still unknown.
// Reconcile Hash before attempting another signed transaction.
type UnconfirmedError struct {
	Hash  [32]byte
	Cause error
}

func (e *UnconfirmedError) Error() string {
	return fmt.Sprintf("transaction unconfirmed hash=%X: %v", e.Hash, e.Cause)
}
func (e *UnconfirmedError) Unwrap() error { return e.Cause }

func broadcastConfirm(ctx context.Context, rpc confirmationRPC, raw []byte) (*coretypes.ResultTx, error) {
	return broadcastConfirmWithTiming(ctx, rpc, raw, ConfirmationTimeout, confirmationPollInterval)
}
func broadcastConfirmWithTiming(ctx context.Context, rpc confirmationRPC, raw []byte, timeout, interval time.Duration) (*coretypes.ResultTx, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	hash := sha256.Sum256(raw)
	duplicate := func(s string) bool {
		s = strings.ToLower(s)
		return strings.Contains(s, "already exists in cache") || strings.Contains(s, "tx already exists") || strings.Contains(s, "duplicate")
	}
	result, err := rpc.BroadcastTxSync(ctx, raw)
	if err != nil && !duplicate(err.Error()) {
		// A transport error can occur after admission; retain the hash for reconciliation.
		return nil, &UnconfirmedError{hash, err}
	}
	if err == nil && result.Code != 0 && !duplicate(result.Log) {
		return nil, fmt.Errorf("chain CheckTx: %s", result.Log)
	}
	for {
		tx, err := rpc.Tx(ctx, hash[:], false)
		if err == nil && tx != nil {
			if tx.TxResult.Code != 0 {
				return nil, fmt.Errorf("chain transaction: %s", tx.TxResult.Log)
			}
			return tx, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, &UnconfirmedError{hash, ctx.Err()}
		case <-timer.C:
		}
	}
}
