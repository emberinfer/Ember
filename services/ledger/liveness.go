package ledger

import (
	"context"
	"crypto/ed25519"
	chain "ember/services/chain"
	"log"
	"time"
)

// LivenessDue uses a strict remaining-window boundary, including overdue proofs.
func LivenessDue(height, lastProof, window, threshold, minInterval int64) bool {
	return height >= lastProof && height-lastProof >= minInterval && window-(height-lastProof) < threshold
}

// GuardLiveness keeps the auditor identity whose private key this process hosts
// alive even when it is idle and no sampled requests arrive.
func (l *ChainLedger) GuardLiveness(ctx context.Context, threshold int64, poll time.Duration) {
	tick := time.NewTicker(poll)
	defer tick.Stop()
	id := chain.AccountID(l.auditor.Public().(ed25519.PublicKey))
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		var state chain.State
		if err := l.query("/state", &state); err != nil {
			log.Printf("LIVENESS query: %v", err)
		} else if r := state.Nodes[id]; r != nil && r.ExpiresHeight > state.Height {
			p := state.Parameters.Liveness
			if LivenessDue(state.Height, r.LastProofHeight, p.LivenessWindow, threshold, p.ProofMinInterval) {
				if err := l.submit(l.auditor, chain.Action{Type: "prove_liveness", Node: id}); err != nil {
					log.Printf("LIVENESS auditor=%s retry: %v", id, err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
