package ledger

import "testing"

func TestLivenessDue(t *testing.T) {
	for _, c := range []struct {
		name   string
		height int64
		want   bool
	}{
		{"remaining below threshold", 761, true},
		{"remaining equals threshold", 760, false},
		{"remaining above threshold", 759, false},
		{"overdue", 1200, true},
		{"recent proof", 101, false},
		{"future proof", 99, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := LivenessDue(c.height, 100, 960, 300, 60); got != c.want {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}
