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

func perfSnapshot(b *testing.B) *State {
	b.Helper()
	p := os.Getenv("PERF_SNAP")
	if p == "" {
		b.Skip("PERF_SNAP unset")
	}
	s, e := LoadState(p)
	if e != nil {
		b.Fatal(e)
	}
	if os.Getenv("PERF_V11") == "1" {
		s.Version = 11
		if os.Getenv("PERF_WARM") == "1" {
			app := &Application{committed: s, path: filepath.Join(b.TempDir(), "warm.json")}
			if _, e := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: s.Height + 1, Time: time.Unix(s.Time+1, 0)}); e != nil {
				b.Fatal(e)
			}
			if _, e := app.Commit(context.Background(), &abci.CommitRequest{}); e != nil {
				b.Fatal(e)
			}
			s = app.committed
		}
	}
	return s
}
func BenchmarkPerf(b *testing.B) {
	s := perfSnapshot(b)
	b.Run("Clone", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, e := s.Clone(); e != nil {
				b.Fatal(e)
			}
		}
	})
	b.Run("Hash", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, e := s.Hash(); e != nil {
				b.Fatal(e)
			}
		}
	})
	b.Run("Save", func(b *testing.B) {
		p := filepath.Join(b.TempDir(), "state.json")
		if e := SaveState(p, s); e != nil {
			b.Fatal(e)
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if e := SaveState(p, s); e != nil {
				b.Fatal(e)
			}
		}
	})
	for _, n := range []int{0, 5} {
		b.Run(fmt.Sprintf("Finalize%d", n), func(b *testing.B) {
			a := newActor(201)
			z := newActor(202)
			s.Accounts[a.id] = &Account{Balance: 1000000}
			s.Accounts[z.id] = &Account{}
			var txs [][]byte
			for j := 1; j <= n; j++ {
				raw, e := Sign(s.Parameters.ChainID, a.key, uint64(j), Action{Type: "transfer", To: z.id, Amount: 1})
				if e != nil {
					b.Fatal(e)
				}
				txs = append(txs, raw)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				app := &Application{committed: s}
				r, e := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: s.Height + 1, Time: time.Unix(s.Time+1, 0), Txs: txs})
				if e != nil {
					b.Fatal(e)
				}
				for _, r := range r.TxResults {
					if r.Code != 0 {
						b.Fatal(r.Log)
					}
				}
			}
		})
	}
}

func TestPerfV10Oracle(t *testing.T) {
	p := os.Getenv("PERF_ORACLE")
	if p == "" {
		t.Skip("PERF_ORACLE unset")
	}
	s, a := testState(t)
	s.Version = 10
	app := &Application{committed: s}
	out := ""
	for h := int64(1); h <= 8; h++ {
		raw, e := Sign(s.Parameters.ChainID, a["payer"].key, uint64(h), Action{Type: "transfer", To: a["node"].id, Amount: h})
		if e != nil {
			t.Fatal(e)
		}
		r, e := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: h, Txs: [][]byte{raw, []byte("invalid")}})
		if e != nil {
			t.Fatal(e)
		}
		out += fmt.Sprintf("%d %x\n", h, r.AppHash)
		if _, e = app.Commit(context.Background(), &abci.CommitRequest{}); e != nil {
			t.Fatal(e)
		}
	}
	if os.Getenv("PERF_RECORD") == "1" {
		if e := os.WriteFile(p, []byte(out), 0600); e != nil {
			t.Fatal(e)
		}
	} else {
		want, e := os.ReadFile(p)
		if e != nil || string(want) != out {
			t.Fatalf("oracle mismatch: %v\n%s", e, out)
		}
	}
}

// Commit isolates normal durable persistence from Finalize and archive export.
func BenchmarkPerfCommit(b *testing.B) {
	s := perfSnapshot(b)
	p := filepath.Join(b.TempDir(), "state.json")
	if e := SaveState(p, s); e != nil {
		b.Fatal(e)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		app := &Application{committed: s, path: p}
		if _, e := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: s.Height + 1, Time: time.Unix(s.Time+1, 0)}); e != nil {
			b.Fatal(e)
		}
		b.StartTimer()
		if _, e := app.Commit(context.Background(), &abci.CommitRequest{}); e != nil {
			b.Fatal(e)
		}
	}
}
