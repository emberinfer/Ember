package scheduler

import (
	"os"
	"path/filepath"
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
		for _, symbol := range []string{"SpeedScore", "ExecExclude", "RecordTTFT", "TTFTScore", "ttftSamples", "MedianTTFTSeconds"} {
			if strings.Contains(string(b), symbol) {
				t.Errorf("%s leaked into trust/audit: %s", symbol, p)
			}
		}
	}
	err := filepath.WalkDir("../../services/chain", func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, symbol := range []string{"ExecExclude", "exec_exclude", "exec-exclude"} {
			if strings.Contains(string(b), symbol) {
				t.Errorf("blacklist leaked into chain: %s", p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
