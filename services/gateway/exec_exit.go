package gateway

import (
	"log"
	"time"
)

const execExitedTTL = 15 * time.Second

// Returned snapshots are immutable; refresh replaces rather than edits the map.
func (g *Gateway) chainExecExited() map[string]bool {
	if g.ChainExecExited == nil {
		return nil
	}
	g.execExitedMu.Lock()
	defer g.execExitedMu.Unlock()
	if !g.execExitedRefreshing && time.Since(g.execExitedAt) >= execExitedTTL {
		g.execExitedRefreshing = true
		g.execExitedDone = make(chan struct{})
		go g.refreshChainExecExited()
	}
	return g.execExitedSet
}

// RefreshChainExecExited synchronously refreshes, joining any running refresh.
// Call before dispatch starts when an initial chain snapshot is desired.
func (g *Gateway) RefreshChainExecExited() {
	if g.ChainExecExited == nil {
		return
	}
	g.execExitedMu.Lock()
	if g.execExitedRefreshing {
		done := g.execExitedDone
		g.execExitedMu.Unlock()
		<-done
		return
	}
	g.execExitedRefreshing = true
	g.execExitedDone = make(chan struct{})
	g.execExitedMu.Unlock()
	g.refreshChainExecExited()
}

func (g *Gateway) refreshChainExecExited() {
	set, err := g.ChainExecExited()
	snapshot := make(map[string]bool, len(set))
	if err == nil {
		for id, exited := range set {
			snapshot[id] = exited
		}
	} else {
		log.Printf("chain execution exits refresh failed: %v", err)
	}
	g.execExitedMu.Lock()
	defer g.execExitedMu.Unlock()
	if err == nil {
		g.execExitedSet = snapshot
	}
	g.execExitedAt = time.Now()
	g.execExitedRefreshing = false
	close(g.execExitedDone)
}
