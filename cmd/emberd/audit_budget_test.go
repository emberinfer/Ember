package main

import (
	"testing"
	"time"
)

func TestAuditAttemptBudget(t *testing.T) {
	for _, c := range []struct {
		size        int64
		steps       int
		floor, want time.Duration
	}{
		{2250842603, 124, 10 * time.Minute, 11502808676666 * time.Nanosecond},
		{1, 1, time.Hour, time.Hour},
		{300000, 0, time.Second, 926 * time.Second},
		{1 << 60, 1, time.Second, 6 * time.Hour},
		{1, 1, 7 * time.Hour, 6 * time.Hour},
	} {
		if got := auditAttemptBudget(c.floor, c.size, c.steps); got != c.want {
			t.Fatalf("%+v got=%v", c, got)
		}
	}
}
