package gateway

import (
	pb "ember/proto"
	"ember/services/scheduler"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestChainExecExitedFiltering(t *testing.T) {
	g := &Gateway{Candidates: func() []scheduler.Candidate {
		return []scheduler.Candidate{{Profile: &pb.ResourceProfile{NodeId: "local"}}, {Profile: &pb.ResourceProfile{NodeId: "chain"}}, {Profile: &pb.ResourceProfile{NodeId: "both"}}, {Profile: &pb.ResourceProfile{NodeId: "pending"}}}
	}, ExecExclude: map[string]bool{"local": true, "both": true}}
	ids := func() []string {
		out := []string{}
		for _, c := range g.liveCandidates() {
			out = append(out, c.Profile.NodeId)
		}
		return out
	}
	if !reflect.DeepEqual(ids(), []string{"chain", "pending"}) {
		t.Fatal("nil callback baseline")
	}
	fail := false
	g.ChainExecExited = func() (map[string]bool, error) {
		if fail {
			return nil, errors.New("offline")
		}
		return map[string]bool{"chain": true, "both": true}, nil
	}
	g.RefreshChainExecExited()
	if !reflect.DeepEqual(ids(), []string{"pending"}) {
		t.Fatal("union/pending", ids())
	}
	fail = true
	g.RefreshChainExecExited()
	if !reflect.DeepEqual(ids(), []string{"pending"}) {
		t.Fatal("failed refresh discarded snapshot")
	}
	g.execExitedSet = nil
	g.RefreshChainExecExited()
	if !reflect.DeepEqual(ids(), []string{"chain", "pending"}) {
		t.Fatal("failure baseline")
	}
}
func TestChainExecExitedAsyncSingleRefresh(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	g := &Gateway{Candidates: func() []scheduler.Candidate { return nil }, ChainExecExited: func() (map[string]bool, error) {
		started <- struct{}{}
		<-release
		return map[string]bool{"done": true}, nil
	}}
	defer close(release)
	returned := make(chan struct{})
	go func() { g.liveCandidates(); g.liveCandidates(); close(returned) }()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("selection blocked on query")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("refresh not started")
	}
	select {
	case <-started:
		t.Fatal("concurrent refresh")
	default:
	}
}

func TestChainExecExitedStaleSnapshot(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	g := &Gateway{execExitedSet: map[string]bool{"old": true}, execExitedAt: time.Now().Add(-execExitedTTL),
		ChainExecExited: func() (map[string]bool, error) { close(started); <-release; return map[string]bool{"new": true}, nil }}
	if snapshot := g.chainExecExited(); !snapshot["old"] || snapshot["new"] {
		t.Fatal("lost stale snapshot")
	}
	<-started
	g.execExitedMu.Lock()
	done := g.execExitedDone
	g.execExitedMu.Unlock()
	close(release)
	<-done
	if snapshot := g.chainExecExited(); snapshot["old"] || !snapshot["new"] {
		t.Fatal("refresh not published")
	}
}
