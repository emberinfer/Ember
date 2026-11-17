package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func waitMoeWaiters(t *testing.T, q *moeQueue, n int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		q.mu.Lock()
		count := len(q.waiters)
		q.mu.Unlock()
		if count == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("queue did not reach %d waiters", n)
}
func TestMoeQueueFIFO(t *testing.T) {
	q := &moeQueue{maxDepth: 8, maxWait: time.Second * 5}
	release, _ := q.acquire(context.Background())
	type held struct {
		index   int
		release func()
	}
	got := make(chan held, 5)
	for i := 0; i < 5; i++ {
		go func(i int) {
			r, e := q.acquire(context.Background())
			if e != nil {
				got <- held{index: -1}
				return
			}
			got <- held{i, r}
		}(i)
		waitMoeWaiters(t, q, i+1)
	}
	release()
	release() // A duplicate release must not admit a second holder.
	for i := 0; i < 5; i++ {
		select {
		case h := <-got:
			if h.index != i {
				t.Fatalf("got %d want %d", h.index, i)
			}
			h.release()
		case <-time.After(time.Second):
			t.Fatal("handoff stalled")
		}
	}
}
func TestMoeQueueFull(t *testing.T) {
	q := &moeQueue{maxDepth: 2, maxWait: time.Second * 5}
	release, _ := q.acquire(context.Background())
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			r, _ := q.acquire(ctx)
			if r != nil {
				r()
			}
			done <- struct{}{}
		}()
	}
	waitMoeWaiters(t, q, 2)
	start := time.Now()
	_, err := q.acquire(context.Background())
	if !errors.Is(err, ErrMoeQueueFull) || time.Since(start) > time.Second {
		t.Fatalf("full: %v", err)
	}
	cancel()
	<-done
	<-done
}
func TestMoeQueueTimeout(t *testing.T) {
	q := &moeQueue{maxDepth: 1, maxWait: 100 * time.Millisecond}
	release, _ := q.acquire(context.Background())
	defer release()
	start := time.Now()
	_, err := q.acquire(context.Background())
	if !errors.Is(err, ErrMoeQueueTimeout) || time.Since(start) > time.Second || time.Since(start) < 90*time.Millisecond {
		t.Fatalf("timeout: %v after %s", err, time.Since(start))
	}
	waitMoeWaiters(t, q, 0)
}
func TestMoeQueueContextCancel(t *testing.T) {
	q := &moeQueue{maxDepth: 1, maxWait: time.Second}
	release, _ := q.acquire(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := q.acquire(ctx); done <- err }()
	waitMoeWaiters(t, q, 1)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	waitMoeWaiters(t, q, 0)
	release()
	r, err := q.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r()
}
func TestMoeQueueDisabled(t *testing.T) {
	t.Setenv("EMBER_MOE_SERIALIZE", "0")
	if moeSerializationEnabled() {
		t.Fatal("enabled")
	}
}
func TestMoeQueueSettings(t *testing.T) {
	t.Setenv("EMBER_MOE_QUEUE_DEPTH", "bad")
	t.Setenv("EMBER_MOE_QUEUE_WAIT", "-1s")
	d, w := moeQueueSettings()
	if d != 8 || w != 10*time.Minute {
		t.Fatalf("%d %s", d, w)
	}
	t.Setenv("EMBER_MOE_QUEUE_DEPTH", "2")
	t.Setenv("EMBER_MOE_QUEUE_WAIT", "100ms")
	d, w = moeQueueSettings()
	if d != 2 || w != 100*time.Millisecond {
		t.Fatalf("%d %s", d, w)
	}
}
func TestMoeQueueDoesNotBlockNonMoeDispatch(t *testing.T) {
	fx := startResendFixture(t, []scriptedRun{{tokens: []string{"ok"}, content: 1}})
	g := fx.gateway
	g.Moe = &MoeConfig{}
	q := g.moeGenerationQueue()
	q.maxDepth = 0
	release, _ := q.acquire(context.Background())
	defer release()
	// Retain the saturated queue owned by this Gateway, but select whole-model
	// dispatch, just as an instance configured without MoE does.
	g.Moe = nil
	code, body := fx.chat(t, `{"model":"ember-mock","messages":[{"role":"user","content":"hello"}],"max_tokens":1}`)
	if code != 200 || body["choices"] == nil {
		t.Fatalf("whole-model: %d %v", code, body)
	}
}

func TestMoeQueueConcurrentInitialization(t *testing.T) {
	g := &Gateway{Moe: &MoeConfig{}}
	var wg sync.WaitGroup
	queues := make(chan *moeQueue, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); queues <- g.moeGenerationQueue() }()
	}
	wg.Wait()
	close(queues)
	first := <-queues
	for q := range queues {
		if q != first {
			t.Fatal("multiple queues")
		}
	}
	other := &Gateway{Moe: g.Moe}
	if other.moeGenerationQueue() == first {
		t.Fatal("gateways share queue")
	}
}

func TestMoeQueueRejectsBeforeFreeze(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, full := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/full=%v", stream, full), func(t *testing.T) {
				t.Setenv("EMBER_MOE_SERIALIZE", "1")
				g, f := moeFixture(t)
				path := filepath.Join(g.Moe.WorkDir, "tokenize-only")
				if err := os.WriteFile(path, []byte("#!/bin/sh\necho '{\"input_tokens\":3,\"tokenizer_version\":\"test\"}'\n"), 0700); err != nil {
					t.Fatal(err)
				}
				g.Moe.Python = path
				q := g.moeGenerationQueue()
				q.maxDepth = 1
				q.maxWait = 10 * time.Millisecond
				if full {
					q.maxDepth = 0
				}
				release, _ := q.acquire(context.Background())
				defer release()
				w := httptest.NewRecorder()
				g.serveMoe(w, httptest.NewRequest("POST", "/", nil), request{Model: MoeProfile, MaxTokens: 1, Stream: stream}, "prompt", "payer", "rejected")
				msg := "MoE queue wait timeout, retry later"
				if full {
					msg = "MoE queue full, retry later"
				}
				if w.Code != 503 || w.Header().Get("Retry-After") == "" || !strings.Contains(w.Body.String(), msg) || g.rejected != 1 {
					t.Fatalf("%d %s", w.Code, w.Body.String())
				}
				if f.frozen || f.cancelled || f.submitted {
					t.Fatal("queue rejection touched hold")
				}
				if stream && !strings.Contains(w.Body.String(), "event: error") {
					t.Fatal("missing SSE framing")
				}
			})
		}
	}
}
