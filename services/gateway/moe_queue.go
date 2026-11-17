package gateway

import (
	"context"
	"errors"
	"log"
	"os"
	"strconv"
	"sync"
	"time"
)

var (
	ErrMoeQueueFull    = errors.New("MoE queue full")
	ErrMoeQueueTimeout = errors.New("MoE queue wait timeout")
)

const defaultMoeQueueDepth = 8
const defaultMoeQueueWait = 10 * time.Minute

// moeQueue serializes a whole fleet: moe_gateway builds stage-a (layers 0-7)
// and stage-b (8-15), and MoeAssembler.generate traverses all 16 layers.
// moeAuthorizations pins exactly those six fleet nodes. Every generation thus
// shares both stages; disjoint stage subsets cannot provide safe concurrency.
// Explicit waiter handoff supplies FIFO fairness, unlike mutex acquisition.
type moeQueue struct {
	mu       sync.Mutex
	busy     bool
	waiters  []chan struct{}
	maxDepth int
	maxWait  time.Duration
}

func moeSerializationEnabled() bool { return os.Getenv("EMBER_MOE_SERIALIZE") != "0" }

// Called once per lazily created gateway queue; invalid settings log once.
func moeQueueSettings() (int, time.Duration) {
	depth, wait := defaultMoeQueueDepth, defaultMoeQueueWait
	if s := os.Getenv("EMBER_MOE_QUEUE_DEPTH"); s != "" {
		if n, e := strconv.Atoi(s); e == nil && n >= 0 {
			depth = n
		} else {
			log.Printf("invalid EMBER_MOE_QUEUE_DEPTH; using %d", depth)
		}
	}
	if s := os.Getenv("EMBER_MOE_QUEUE_WAIT"); s != "" {
		if d, e := time.ParseDuration(s); e == nil && d > 0 {
			wait = d
		} else {
			log.Printf("invalid EMBER_MOE_QUEUE_WAIT; using %s", wait)
		}
	}
	return depth, wait
}

func (g *Gateway) moeGenerationQueue() *moeQueue {
	c := g.Moe
	c.queueMu.Lock()
	defer c.queueMu.Unlock()
	if c.queues == nil {
		c.queues = make(map[*Gateway]*moeQueue)
	}
	if c.queues[g] == nil {
		depth, wait := moeQueueSettings()
		c.queues[g] = &moeQueue{maxDepth: depth, maxWait: wait}
	}
	return c.queues[g]
}

func (q *moeQueue) acquire(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q.mu.Lock()
	if !q.busy && len(q.waiters) == 0 {
		q.busy = true
		q.mu.Unlock()
		return q.releaser(), nil
	}
	if len(q.waiters) >= q.maxDepth {
		q.mu.Unlock()
		return nil, ErrMoeQueueFull
	}
	ch := make(chan struct{})
	q.waiters = append(q.waiters, ch)
	q.mu.Unlock()
	timer := time.NewTimer(q.maxWait)
	defer timer.Stop()
	var err error
	select {
	case <-ch:
		return q.releaser(), nil
	case <-ctx.Done():
		err = ctx.Err()
	case <-timer.C:
		err = ErrMoeQueueTimeout
	}
	q.mu.Lock()
	for i, waiter := range q.waiters {
		if waiter == ch {
			q.waiters = append(q.waiters[:i], q.waiters[i+1:]...)
			q.mu.Unlock()
			return nil, err
		}
	}
	// Handoff won the race with cancellation/timeout. Relinquish the slot
	// rather than leaking ownership or letting a cancelled waiter block FIFO.
	q.releaseLocked()
	q.mu.Unlock()
	return nil, err
}

func (q *moeQueue) releaser() func() {
	var once sync.Once
	return func() { once.Do(func() { q.mu.Lock(); defer q.mu.Unlock(); q.releaseLocked() }) }
}

func (q *moeQueue) releaseLocked() {
	if len(q.waiters) == 0 {
		q.busy = false
		return
	}
	ch := q.waiters[0]
	q.waiters[0] = nil
	q.waiters = q.waiters[1:]
	close(ch)
}
