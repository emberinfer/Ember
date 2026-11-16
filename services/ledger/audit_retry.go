package ledger

import (
	"encoding/json"
	"sync"
	"time"
)

// AuditBackoff is deterministic and resets when finalized hold state changes.
type AuditBackoff struct {
	failures uint
	progress string
}

func (b *AuditBackoff) Next(progress string) time.Duration {
	if progress != b.progress {
		b.failures = 0
		b.progress = progress
	}
	d := time.Second
	for i := uint(0); i < b.failures && d < 30*time.Second; i++ {
		d *= 2
	}
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	if b.failures < 5 {
		b.failures++
	}
	return d
}

type auditRetryEntry struct {
	active  bool
	next    time.Time
	backoff AuditBackoff
}

// AuditRetries shares admission across receipt callbacks and spool recovery.
type AuditRetries struct {
	mu      sync.Mutex
	entries map[string]*auditRetryEntry
}

func (r *AuditRetries) Start(id string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = map[string]*auditRetryEntry{}
	}
	e := r.entries[id]
	if e == nil {
		e = &auditRetryEntry{}
		r.entries[id] = e
	}
	if e.active || now.Before(e.next) {
		return false
	}
	e.active = true
	return true
}
func (r *AuditRetries) Finish(id, progress string, done bool, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[id]
	if e == nil {
		return
	}
	if done {
		delete(r.entries, id)
		return
	}
	e.active = false
	e.next = now.Add(e.backoff.Next(progress))
}
func (l *ChainLedger) AuditProgress(id string) string {
	h, err := l.hold(id)
	if err != nil {
		return ""
	}
	// Audit assignment/reveal and adjudication changes all reset the retry delay.
	raw, _ := json.Marshal(h)
	return string(raw)
}
