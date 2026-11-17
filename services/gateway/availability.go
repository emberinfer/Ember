package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"ember/services/scheduler"
)

// Candidates is a live snapshot, not a cache. Keep legacy unsigned demo
// profiles (expiry zero), but never dispatch an explicitly expired lease.
// A renewed tracker lease supersedes the older signed chain profile expiry.
func (g *Gateway) liveCandidates() []scheduler.Candidate {
	candidates := unexpiredCandidates(g.Candidates(), time.Now())
	exited := g.chainExecExited()
	out := candidates[:0]
	for _, c := range candidates {
		if !g.ExecExclude[c.Profile.NodeId] && !exited[c.Profile.NodeId] {
			out = append(out, c)
		}
	}
	return out
}

func unexpiredCandidates(candidates []scheduler.Candidate, now time.Time) []scheduler.Candidate {
	out := make([]scheduler.Candidate, 0, len(candidates))
	for _, c := range candidates {
		expiry := c.LiveUntilUnix
		if expiry == 0 && c.Profile != nil {
			expiry = c.Profile.ExpiresUnix
		}
		if c.Profile != nil && (expiry == 0 || expiry > now.Unix()) {
			out = append(out, c)
		}
	}
	return out
}

// Check the already authorized plan; do not silently replace its nodes/ranges.
func (g *Gateway) pipelineAvailable(plan scheduler.PipelinePlan, req scheduler.Request, candidates []scheduler.Candidate) error {
	req.Shards = true
	eligible := map[string]bool{}
	live := unexpiredCandidates(candidates, time.Now())
	for i := range live {
		if g.Tunnel != nil {
			live[i].Online = live[i].Online && g.Tunnel.Online(live[i].Profile.NodeId)
		}
	}
	for _, n := range scheduler.Rank(req, live) {
		eligible[n.NodeID] = true
	}
	for i, stage := range plan.Stages {
		available := false
		for _, n := range stage.Nodes {
			available = available || eligible[n.NodeID]
		}
		if !available {
			return fmt.Errorf("pipeline stage %d has no available candidates (offline, expired or ineligible)", i)
		}
	}
	return nil
}

// Use the existing SSE error event even when dispatch failed before token one.
func requestError(w http.ResponseWriter, stream bool, requestID, reason string, status int) {
	w.Header().Set("X-Request-ID", requestID)
	if !stream {
		http.Error(w, reason, status)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	raw, _ := json.Marshal(reason)
	fmt.Fprintf(w, "id: %s\nevent: error\ndata: %s\n\n", requestID, raw)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// Flush the dispatch failure before waiting for chain cancellation confirmation.
// A release failure adds a second error event instead of hiding the held funds.
func (g *Gateway) failPipeline(w http.ResponseWriter, stream bool, id string, cause error, status int) error {
	if stream {
		requestError(w, true, id, cause.Error(), status)
	}
	cancelErr := g.Ledger.CancelPipeline(id)
	if cancelErr != nil {
		cause = fmt.Errorf("%v; freeze release failed: %w", cause, cancelErr)
	}
	if !stream || cancelErr != nil {
		if stream {
			status = http.StatusOK
		}
		requestError(w, stream, id, cause.Error(), status)
	}
	return cause
}
