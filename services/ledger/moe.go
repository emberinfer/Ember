package ledger

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	pb "ember/proto"
	chainapp "ember/services/chain"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/cometbft/cometbft/types"
	"google.golang.org/protobuf/proto"
)

// ChainLedger MoE client surface, mirroring the pipeline methods. The SQL
// Ledger predates profile-typed holds and does not serve MoE profiles;
// MoE runs against the chain ledger only.

type MoeAuthorization struct {
	NodeID     string
	Kind       string // "attention" | "expert"
	StartLayer uint32
	EndLayer   uint32
	Units      []chainapp.MoeUnitRef
}

func (l *ChainLedger) FreezeMoe(id, payer string, amount int64, authorizations []MoeAuthorization, inputTokens, maxTokens uint32, commitment []byte) error {
	auth := make([]chainapp.MoeAuthorization, len(authorizations))
	for i, v := range authorizations {
		auth[i] = chainapp.MoeAuthorization{NodeID: v.NodeID, Kind: v.Kind,
			StartLayer: v.StartLayer, EndLayer: v.EndLayer, Units: v.Units}
	}
	return l.withAccount(payer, chainapp.Action{Type: "freeze_moe", HoldID: id,
		Payer: payer, Amount: amount, MoeAuthorizations: auth,
		MoeInputTokens: inputTokens, MoeMaxTokens: maxTokens, PricingCommitment: commitment})
}

func (l *ChainLedger) SubmitMoe(id string, r *pb.PipelineExecutionReceipt, actual int64) ([]StageShare, error) {
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(r)
	if err != nil {
		return nil, err
	}
	if len(raw) > chainapp.ChunkPayloadBytes {
		return l.chunkedSubmitMoe(id, raw, actual)
	}
	if err = l.submit(l.coordinator, chainapp.Action{Type: "submit_moe", HoldID: id, Actual: actual, Receipt: raw}); err != nil {
		return nil, err
	}
	return l.MoeCredits(id)
}

// ChunkedSubmitMoe can be retried with the same receipt after interruption.
// Only chunks absent from the consensus bitmap are sent again.
func (l *ChainLedger) ChunkedSubmitMoe(id string, r *pb.PipelineExecutionReceipt, actual int64) ([]StageShare, error) {
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(r)
	if err != nil {
		return nil, err
	}
	return l.chunkedSubmitMoe(id, raw, actual)
}

func (l *ChainLedger) chunkedSubmitMoe(id string, raw []byte, actual int64) ([]StageShare, error) {
	return sendMoeChunks(id, raw, actual, l.submitMoeChunk, l.hold)
}

func sendMoeChunks(id string, raw []byte, actual int64, send func(chainapp.Action) error, hold func(string) (*chainapp.Hold, error)) ([]StageShare, error) {
	if len(raw) == 0 || len(raw) > chainapp.MaxChunkBytes {
		return nil, errors.New("invalid chunked MoE receipt size")
	}
	root := sha256.Sum256(raw)
	total := (len(raw) + chainapp.ChunkPayloadBytes - 1) / chainapp.ChunkPayloadBytes
	hashes := make([][]byte, total)
	list := sha256.New()
	for i := range hashes {
		end := min((i+1)*chainapp.ChunkPayloadBytes, len(raw))
		hash := sha256.Sum256(raw[i*chainapp.ChunkPayloadBytes : end])
		hashes[i] = hash[:]
		list.Write(hash[:])
	}
	listHash := list.Sum(nil)
	refHash := sha256.New()
	refHash.Write([]byte("ember/pipeline-receipt/v1\x00"))
	refHash.Write(raw)
	receiptRef := hex.EncodeToString(refHash.Sum(nil))
	matches := func(h *chainapp.Hold) bool {
		return h != nil && h.Moe != nil && bytes.Equal(h.Moe.ChunkRoot, root[:]) && bytes.Equal(h.Moe.ChunkListHash, listHash) && h.Moe.ChunkBytes == len(raw) && h.Moe.ChunkTotal == total && h.Moe.ChunkActual == actual
	}
	finalized := func(h *chainapp.Hold) bool {
		return matches(h) && (h.State == "dispute" || h.State == "settled" || h.State == "slashed") && h.ReceiptRef == receiptRef
	}
	h, err := hold(id)
	if err != nil {
		return nil, err
	}
	if finalized(h) {
		return h.Moe.Credits, nil
	}
	if h == nil || h.Moe == nil || h.State != "frozen" {
		return nil, errors.New("MoE hold is not frozen")
	}
	if h.Moe.ChunkTotal > 0 && !matches(h) {
		return nil, errors.New("MoE hold has a different chunk manifest")
	}
	if err = send(chainapp.Action{Type: "submit_moe_manifest", HoldID: id, Actual: actual, ChunkRoot: root[:], ChunkBytes: len(raw), ChunkTotal: total, ChunkHashes: hashes}); err != nil {
		return nil, err
	}
	for i := range hashes {
		h, err = hold(id)
		if err != nil {
			return nil, err
		}
		if !matches(h) || h.State != "frozen" {
			return nil, errors.New("MoE chunk manifest changed during upload")
		}
		if i/8 < len(h.Moe.ChunkBitmap) && h.Moe.ChunkBitmap[i/8]&(1<<uint(i%8)) != 0 {
			continue
		}
		end := min((i+1)*chainapp.ChunkPayloadBytes, len(raw))
		if err = send(chainapp.Action{Type: "submit_moe_chunk", HoldID: id, ChunkIndex: i, ReceiptChunk: raw[i*chainapp.ChunkPayloadBytes : end]}); err != nil {
			return nil, err
		}
	}
	finalizeErr := send(chainapp.Action{Type: "submit_moe_finalize", HoldID: id})
	// A lost final broadcast response is reconciled by the hold, never guessed
	// from the transaction receipt alone.
	h, err = hold(id)
	if err != nil {
		return nil, err
	}
	if finalized(h) {
		return h.Moe.Credits, nil
	}
	if finalizeErr != nil {
		return nil, finalizeErr
	}
	return nil, errors.New("MoE finalize did not enter dispute")
}

type moeChunkRPC interface {
	BroadcastTxCommit(context.Context, types.Tx) (*coretypes.ResultBroadcastTxCommit, error)
	Tx(context.Context, []byte, bool) (*coretypes.ResultTx, error)
}

// Chunk broadcasts use the design's 60-second commit wait. The legacy small
// receipt path continues to use submit and its existing sync/poll behavior.
func (l *ChainLedger) submitMoeChunk(action chainapp.Action) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	id := chainapp.AccountID(l.coordinator.Public().(ed25519.PublicKey))
	var account chainapp.Account
	if err := l.query("/account/"+id, &account); err != nil {
		return err
	}
	raw, err := chainapp.Sign(l.chainID, l.coordinator, account.Nonce+1, action)
	if err != nil {
		return err
	}
	tx, err := broadcastMoeChunk(context.Background(), l.rpc, raw, 60*time.Second, ConfirmationTimeout, confirmationPollInterval)
	if err != nil {
		return err
	}
	log.Printf("CHAIN TX type=%s height=%d tx_hash=%X", action.Type, tx.Height, tx.Hash)
	return nil
}

func broadcastMoeChunk(ctx context.Context, rpc moeChunkRPC, raw []byte, broadcastWait, pollWait, interval time.Duration) (*coretypes.ResultTx, error) {
	hash := sha256.Sum256(raw)
	wait, cancel := context.WithTimeout(ctx, broadcastWait)
	result, broadcastErr := rpc.BroadcastTxCommit(wait, raw)
	cancel()
	duplicate := func(s string) bool {
		s = strings.ToLower(s)
		return strings.Contains(s, "already exists in cache") || strings.Contains(s, "tx already exists") || strings.Contains(s, "duplicate")
	}
	if broadcastErr == nil && result != nil {
		if result.CheckTx.Code != 0 && !duplicate(result.CheckTx.Log) {
			return nil, fmt.Errorf("chain CheckTx: %s", result.CheckTx.Log)
		}
		if result.CheckTx.Code == 0 && result.TxResult.Code != 0 {
			return nil, fmt.Errorf("chain transaction: %s", result.TxResult.Log)
		}
	}
	// Even transport errors can follow admission. Poll the exact signed hash
	// before permitting the caller to reconcile its bitmap and nonce.
	poll, stop := context.WithTimeout(ctx, pollWait)
	defer stop()
	for {
		tx, err := rpc.Tx(poll, hash[:], false)
		if err == nil && tx != nil {
			if tx.TxResult.Code != 0 {
				return nil, fmt.Errorf("chain transaction: %s", tx.TxResult.Log)
			}
			return tx, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-poll.Done():
			timer.Stop()
			return nil, &UnconfirmedError{Hash: hash, Cause: poll.Err()}
		case <-timer.C:
		}
	}
}

func (l *ChainLedger) FinishMoe(id, outcome string, _ time.Time, multiplier int64) error {
	if multiplier != 20 {
		return errors.New("chain slash multiplier is fixed by genesis")
	}
	h, err := l.hold(id)
	if err != nil {
		return err
	}
	if h.AuditVerdict == "service_failure" {
		outcome = h.AuditVerdict
	}
	receipts, err := chainapp.ExpectedAuditReceipts(h, outcome)
	if err != nil {
		return err
	}
	return l.submit(l.coordinator, chainapp.Action{Type: "settle", HoldID: id, Outcome: outcome, AuditReceipts: receipts})
}

func (l *ChainLedger) CancelMoe(id string) error { return l.Cancel(id) }

func (l *ChainLedger) MoeCredits(id string) ([]StageShare, error) {
	h, err := l.hold(id)
	if err != nil {
		return nil, err
	}
	if h.Moe == nil {
		return nil, errors.New("not a MoE hold")
	}
	return h.Moe.Credits, nil
}

// MoeSettlementReady reads consensus height through the existing query client.
func (l *ChainLedger) MoeSettlementReady(id string) (bool, error) {
	h, err := l.hold(id)
	if err != nil {
		return false, err
	}
	var s chainapp.State
	if err = l.query("/state", &s); err != nil {
		return false, err
	}
	return s.Height >= h.SettleHeight, nil
}
