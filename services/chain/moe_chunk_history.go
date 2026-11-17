package chain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
)

// ChunkBlockSource supplies public transactions in block order. Invalid block
// transactions may be included: only bytes matching the committed hold's anchors
// are used, so rejected uploads cannot poison recovery.
type ChunkBlockSource interface {
	BlockTransactions(context.Context, int64) ([][]byte, error)
}

// ReassembleMoeHistory recovers a receipt using only block history and the hold
// view (which may be archived). The hold pins both the hash list and full bytes;
// no gateway or validator spool is trusted. The source must be the same chain as
// the hold view. Bounds are inclusive; callers can checkpoint scans externally.
func ReassembleMoeHistory(ctx context.Context, h *Hold, first, last int64, source ChunkBlockSource) ([]byte, error) {
	if h == nil || h.Moe == nil || h.Moe.ChunkTotal < 1 || h.Moe.ChunkTotal > MaxChunkTotal || h.Moe.ChunkBytes < 1 || h.Moe.ChunkBytes > MaxChunkBytes || len(h.Moe.ChunkRoot) != sha256.Size || len(h.Moe.ChunkListHash) != sha256.Size || first < 1 || last < first {
		return nil, errors.New("invalid chunked hold or block range")
	}
	m := h.Moe
	var hashes [][]byte
	chunks := make([][]byte, m.ChunkTotal)
	for height := first; height <= last; height++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		txs, err := source.BlockTransactions(ctx, height)
		if err != nil {
			return nil, fmt.Errorf("block %d: %w", height, err)
		}
		for _, raw := range txs {
			env, _, err := DecodeAndVerifyVersion(raw, 13)
			if err != nil || env.Action.HoldID != h.ID {
				continue
			}
			a := env.Action
			switch a.Type {
			case "submit_moe_manifest":
				if a.ChunkTotal != m.ChunkTotal || a.ChunkBytes != m.ChunkBytes || a.Actual != m.ChunkActual || !bytes.Equal(a.ChunkRoot, m.ChunkRoot) || len(a.ChunkHashes) != m.ChunkTotal {
					continue
				}
				digest := sha256.New()
				valid := true
				for _, hash := range a.ChunkHashes {
					if len(hash) != sha256.Size {
						valid = false
						break
					}
					digest.Write(hash)
				}
				if valid && bytes.Equal(digest.Sum(nil), m.ChunkListHash) {
					hashes = a.ChunkHashes
				}
			case "submit_moe_chunk":
				if hashes == nil || a.ChunkIndex < 0 || a.ChunkIndex >= len(hashes) {
					continue
				}
				want := ChunkPayloadBytes
				if a.ChunkIndex == m.ChunkTotal-1 {
					want = m.ChunkBytes - ChunkPayloadBytes*(m.ChunkTotal-1)
				}
				if want < 1 || want > ChunkPayloadBytes || len(a.ReceiptChunk) != want {
					continue
				}
				sum := sha256.Sum256(a.ReceiptChunk)
				if bytes.Equal(sum[:], hashes[a.ChunkIndex]) {
					chunks[a.ChunkIndex] = a.ReceiptChunk
				}
			}
		}
	}
	if hashes == nil {
		return nil, errors.New("matching manifest missing from block history")
	}
	raw := make([]byte, 0, m.ChunkBytes)
	for i, chunk := range chunks {
		if chunk == nil {
			return nil, fmt.Errorf("chunk %d missing from block history", i)
		}
		raw = append(raw, chunk...)
	}
	sum := sha256.Sum256(raw)
	if len(raw) != m.ChunkBytes || !bytes.Equal(sum[:], m.ChunkRoot) {
		return nil, errors.New("block history receipt root or length mismatch")
	}
	return raw, nil
}
