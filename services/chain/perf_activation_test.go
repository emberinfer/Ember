package chain

import (
	"context"
	"fmt"
	abci "github.com/cometbft/cometbft/abci/types"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPerfActivation(t *testing.T) {
	p := os.Getenv("PERF_LIVE")
	if p == "" {
		t.Skip("PERF_LIVE unset")
	}
	s, e := LoadState(p)
	if e != nil {
		t.Fatal(e)
	}
	before, e := s.Bytes()
	if e != nil {
		t.Fatal(e)
	}
	holds, hashes := len(s.Holds), len(s.BlockHashes)
	s.Version = 11
	app := &Application{committed: s, path: filepath.Join(t.TempDir(), "state.json")}
	start := time.Now()
	_, e = app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: s.Height + 1, Time: time.Unix(s.Time+1, 0)})
	if e != nil {
		t.Fatal(e)
	}
	elapsed := time.Since(start)
	n := app.staged
	out := fmt.Sprintf("live activation H=%d->%d canonical_bytes=%d->%d holds=%d->%d block_hashes=%d->%d archived_holds=%d archived_hashes=%d Finalize=%s\n", s.Height, n.Height, len(before), len(app.stagedBytes), holds, len(n.Holds), hashes, len(n.BlockHashes), len(n.history.holds), len(n.history.hashes), elapsed)
	start = time.Now()
	if _, e = app.Commit(context.Background(), &abci.CommitRequest{}); e != nil {
		t.Fatal(e)
	}
	out += fmt.Sprintf("activation Commit (includes initial archive fsync)=%s\n", time.Since(start))
	cold, e := LoadState(app.path)
	if e != nil {
		t.Fatal(e)
	}
	for id, h := range s.Holds {
		if cold.LookupHold(id) == nil || cold.LookupHold(id).State != h.State {
			t.Fatal("activation lost hold", id)
		}
	}
	t.Log(out)
	if dest := os.Getenv("PERF_ACTIVATION_OUT"); dest != "" {
		if e = os.WriteFile(dest, []byte(out), 0600); e != nil {
			t.Fatal(e)
		}
	}
}
