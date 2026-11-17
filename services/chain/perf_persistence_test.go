package chain

import (
	"bytes"
	"context"
	"crypto/sha256"
	abci "github.com/cometbft/cometbft/abci/types"
	"os"
	"path/filepath"
	"testing"
)

func TestPerfMarshalOnce(t *testing.T) {
	s, _ := testState(t)
	s.Version = 11
	app := &Application{committed: s, path: filepath.Join(t.TempDir(), "state.json")}
	r, e := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: 1})
	if e != nil {
		t.Fatal(e)
	}
	raw, e := app.staged.Bytes()
	if e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256(raw)
	if !bytes.Equal(raw, app.stagedBytes) || !bytes.Equal(sum[:], r.AppHash) {
		t.Fatal("staged bytes/hash differ")
	}
	if _, e = app.Commit(context.Background(), &abci.CommitRequest{}); e != nil {
		t.Fatal(e)
	}
	saved, e := os.ReadFile(app.path)
	if e != nil || !bytes.Equal(saved, raw) {
		t.Fatal("persisted bytes differ", e)
	}
}
func TestPerfLiveRoundtrip(t *testing.T) {
	p := os.Getenv("PERF_LIVE")
	if p == "" {
		t.Skip("PERF_LIVE unset")
	}
	s, e := LoadState(p)
	if e != nil {
		t.Fatal(e)
	}
	want, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	dest := filepath.Join(t.TempDir(), "state.json")
	if e = SaveState(dest, s); e != nil {
		t.Fatal(e)
	}
	got, e := os.ReadFile(dest)
	if e != nil || !bytes.Equal(got, want) {
		t.Fatal("live roundtrip differs", e)
	}
}
