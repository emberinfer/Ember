package ledger

import (
	"sync"
	"testing"
	"time"
)

func TestAuditBackoff(t *testing.T) {
	b := AuditBackoff{}
	for _, want := range []int{1, 2, 4, 8, 16, 30, 30} {
		if got := b.Next("assigned"); got != time.Duration(want)*time.Second {
			t.Fatal(got, want)
		}
	}
	if b.Next("revealed") != time.Second {
		t.Fatal("progress did not reset")
	}
}
func TestAuditRetryDedup(t *testing.T) {
	r := &AuditRetries{}
	now := time.Now()
	var wg sync.WaitGroup
	wins := make(chan bool, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); wins <- r.Start("hold", now) }()
	}
	wg.Wait()
	close(wins)
	n := 0
	for win := range wins {
		if win {
			n++
		}
	}
	if n != 1 {
		t.Fatal(n)
	}
	r.Finish("hold", "assigned", false, now)
	if r.Start("hold", now) {
		t.Fatal("ignored backoff")
	}
	if !r.Start("hold", now.Add(time.Second)) {
		t.Fatal("did not retry")
	}
	r.Finish("hold", "done", true, now)
	if !r.Start("hold", now) {
		t.Fatal("terminal entry retained")
	}
}
