package scheduler

import (
	"os"
	"strings"
	"testing"
)

// Scanned files: services/chain/audit_sampling.go, services/chain/assignment.go,
// services/chain/latency.go, services/chain/state.go, core-go/latency.go.
// No ReputationTable implementation or probation 0.45 exists in this baseline;
// state.go and core-go/latency.go contain the probation/graduation implementation.
func TestSpeedIronLaw(t *testing.T) {
	for _, p := range []string{"../../services/chain/audit_sampling.go", "../../services/chain/assignment.go", "../../services/chain/latency.go", "../../services/chain/state.go", "../../core-go/latency.go"} {
		b, e := os.ReadFile(p)
		if e != nil {
			t.Fatal(e)
		}
		if strings.Contains(string(b), "SpeedScore") {
			t.Errorf("speed leaked into trust/audit: %s", p)
		}
	}
}
