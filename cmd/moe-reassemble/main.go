// moe-reassemble recovers a chunked MoE receipt from public block history.
// Usage: go run ./cmd/moe-reassemble --rpc http://localhost:26657 --hold ID
// --from 1 --to HEIGHT --out receipt.pb
// The /hold view pins ChunkListHash and ChunkRoot. Use a trusted chain RPC;
// this utility performs no transactions and never reads a validator spool.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	chain "ember/services/chain"
	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
)

type rpcBlocks struct{ client *rpchttp.HTTP }

func (s rpcBlocks) BlockTransactions(ctx context.Context, height int64) ([][]byte, error) {
	call, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	b, err := s.client.Block(call, &height)
	if err != nil {
		return nil, err
	}
	txs := make([][]byte, len(b.Block.Data.Txs))
	for i, tx := range b.Block.Data.Txs {
		txs[i] = tx
	}
	return txs, nil
}
func run() error {
	rpc := flag.String("rpc", "", "chain RPC URL")
	id := flag.String("hold", "", "hold ID")
	first := flag.Int64("from", 1, "first block, inclusive")
	last := flag.Int64("to", 0, "last block, inclusive (0: latest)")
	out := flag.String("out", "", "output receipt proto file (created mode 0600)")
	flag.Parse()
	if *rpc == "" || *id == "" || *out == "" {
		return errors.New("--rpc, --hold and --out required")
	}
	client, err := rpchttp.New(*rpc)
	if err != nil {
		return err
	}
	ctx := context.Background()
	call, cancel := context.WithTimeout(ctx, 60*time.Second)
	view, err := client.ABCIQuery(call, "/hold/"+*id, nil)
	cancel()
	if err != nil {
		return err
	}
	if view.Response.Code != 0 {
		return fmt.Errorf("hold query: %s", view.Response.Log)
	}
	var h chain.Hold
	if err = json.Unmarshal(view.Response.Value, &h); err != nil {
		return err
	}
	if h.ID != *id {
		return errors.New("hold query ID mismatch")
	}
	if *last == 0 {
		call, cancel = context.WithTimeout(ctx, 60*time.Second)
		status, e := client.Status(call)
		cancel()
		if e != nil {
			return e
		}
		*last = status.SyncInfo.LatestBlockHeight
	}
	raw, err := chain.ReassembleMoeHistory(ctx, &h, *first, *last, rpcBlocks{client})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.Write(raw); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	fmt.Printf("hold=%s bytes=%d chunks=%d output=%s\n", h.ID, len(raw), h.Moe.ChunkTotal, *out)
	return f.Close()
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
