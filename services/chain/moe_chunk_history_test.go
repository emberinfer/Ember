package chain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"
)

type drillChunkBlocks map[int64][][]byte

func (b drillChunkBlocks) BlockTransactions(_ context.Context, height int64) ([][]byte, error) {
	return b[height], nil
}

func TestMoeChunkHistoryDrill(t *testing.T) {
	s, h, want, history := runMoeChunkDrill(t)
	var last int64
	for height := range history {
		if height > last {
			last = height
		}
	}
	got, err := ReassembleMoeHistory(context.Background(), h, 1, last, drillChunkBlocks(history))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("history differs from receipt", err)
	}
	spool, err := s.moeReceipt(h)
	if err != nil || !bytes.Equal(got, spool) {
		t.Fatal("history differs from spool", err)
	}
	hash := sha256.Sum256(got)
	if !bytes.Equal(hash[:], h.Moe.ChunkRoot) {
		t.Fatal("history root mismatch")
	}
	t.Logf("block-history receipt=%d bytes chunks=%d sha256=%x", len(got), h.Moe.ChunkTotal, hash)
	// Anchor tampering, truncated history, missing source data and cancellation
	// all fail closed; wrong-hash/duplicate transactions are in the drill history.
	changed := *h
	metadata := *h.Moe
	changed.Moe = &metadata
	metadata.ChunkListHash = bytes.Repeat([]byte{0xff}, 32)
	if _, err = ReassembleMoeHistory(context.Background(), &changed, 1, last, drillChunkBlocks(history)); err == nil {
		t.Fatal("wrong list anchor accepted")
	}
	metadata = *h.Moe
	metadata.ChunkRoot = bytes.Repeat([]byte{0xff}, 32)
	if _, err = ReassembleMoeHistory(context.Background(), &changed, 1, last, drillChunkBlocks(history)); err == nil {
		t.Fatal("wrong root accepted")
	}
	if _, err = ReassembleMoeHistory(context.Background(), h, 1, last, drillChunkBlocks{}); err == nil {
		t.Fatal("missing blocks accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = ReassembleMoeHistory(ctx, h, 1, last, drillChunkBlocks(history)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
