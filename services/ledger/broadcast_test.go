package ledger

import (
	"context"
	"crypto/sha256"
	"errors"
	abci "github.com/cometbft/cometbft/abci/types"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/cometbft/cometbft/types"
	"testing"
	"time"
)

type confirmStub struct {
	polls, broadcasts, delay int
	check, deliver           uint32
	duplicate                bool
	hash                     []byte
}

func (s *confirmStub) BroadcastTxSync(context.Context, types.Tx) (*coretypes.ResultBroadcastTx, error) {
	s.broadcasts++
	if s.duplicate {
		return nil, errors.New("tx already exists in cache")
	}
	return &coretypes.ResultBroadcastTx{Code: s.check, Log: "check rejected"}, nil
}
func (s *confirmStub) Tx(_ context.Context, hash []byte, _ bool) (*coretypes.ResultTx, error) {
	s.polls++
	s.hash = hash
	if s.polls <= s.delay {
		return nil, errors.New("not found")
	}
	return &coretypes.ResultTx{TxResult: abci.ExecTxResult{Code: s.deliver, Log: "execution failed"}}, nil
}
func TestBroadcastConfirm(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stub      confirmStub
		wantPolls int
		wantError bool
	}{
		{"fast", confirmStub{}, 1, false}, {"delayed", confirmStub{delay: 3}, 4, false},
		{"check rejection", confirmStub{check: 1}, 0, true}, {"execution failure", confirmStub{deliver: 1}, 1, true},
		{"duplicate", confirmStub{duplicate: true}, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := broadcastConfirmWithTiming(context.Background(), &tc.stub, []byte("signed"), time.Second, time.Millisecond)
			if (err != nil) != tc.wantError || tc.stub.polls != tc.wantPolls || tc.stub.broadcasts != 1 {
				t.Fatalf("err=%v stub=%+v", err, tc.stub)
			}
		})
	}
}
func TestBroadcastUnconfirmed(t *testing.T) {
	s := &confirmStub{delay: 10000}
	raw := []byte("signed")
	_, err := broadcastConfirmWithTiming(context.Background(), s, raw, 5*time.Millisecond, time.Millisecond)
	var unconfirmed *UnconfirmedError
	if !errors.As(err, &unconfirmed) || unconfirmed.Hash != sha256.Sum256(raw) || !errors.Is(err, context.DeadlineExceeded) || s.broadcasts != 1 {
		t.Fatalf("%v %+v", err, s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = broadcastConfirmWithTiming(ctx, s, raw, time.Second, time.Millisecond)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
