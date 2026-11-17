package chain

import (
	"bytes"
	"context"
	abci "github.com/cometbft/cometbft/abci/types"
	"os"
	"path/filepath"
	"testing"
)

func TestBlockHashArchiveAndCrash(t *testing.T) {
	for _, v := range []uint64{10, 11} {
		s, _ := testState(t)
		s.Version = v
		s.Height = 5000
		s.PriceEpochStart = 5000
		s.BlockHashes[1] = bytes.Repeat([]byte{1}, 32)
		app := &Application{committed: s, path: filepath.Join(t.TempDir(), "state.json")}
		if e := SaveState(app.path, s); e != nil {
			t.Fatal(e)
		}
		if _, e := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: 5001}); e != nil {
			t.Fatal(e)
		}
		if v == 10 {
			if app.staged.BlockHashes[1] == nil {
				t.Fatal("legacy pruning")
			}
			continue
		}
		if app.staged.BlockHashes[1] != nil || len(app.staged.blockHash(1)) != 32 {
			t.Fatal("archive lookup")
		}
		// Crash between archive durability and consensus rename still loads old state.
		if e := persistArchive(app.path, app.staged, app.stagedBytes); e != nil {
			t.Fatal(e)
		}
		old, e := LoadState(app.path)
		if e != nil || old.Height != 5000 {
			t.Fatal("orphan selected", e)
		}
		if _, e = app.Commit(context.Background(), &abci.CommitRequest{}); e != nil {
			t.Fatal(e)
		}
		cold, e := LoadState(app.path)
		if e != nil || cold.BlockHashes[1] != nil || !bytes.Equal(cold.blockHash(1), s.BlockHashes[1]) {
			t.Fatal("cold archive", e)
		}
		// Missing committed archive is a startup error, never silent missing entropy.
		if e = os.Remove(archivePath(app.path, archiveKey(app.stagedBytes))); e == nil {
			t.Fatal("unexpected empty key file")
		}
		raw, _ := os.ReadFile(app.path)
		if e = os.Remove(archivePath(app.path, archiveKey(raw))); e != nil {
			t.Fatal(e)
		}
		if _, e = LoadState(app.path); e == nil {
			t.Fatal("missing archive accepted")
		}
	}
}
