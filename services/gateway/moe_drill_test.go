package gateway

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	pb "ember/proto"
	"ember/services/ledger"
	"google.golang.org/protobuf/proto"
)

// This drill intentionally uses only production symbols present before the fix.
// The fixture's immutable profiles are reused; all mutable ledger state is
// replaced here so simultaneous generations are safe to test under -race.
type drillMoeLedger struct {
	*fakeMoeLedger
	mu            sync.Mutex
	submissions   map[string]bool
	cancellations map[string]bool
}

func (f *drillMoeLedger) FreezeMoe(string, string, int64, []ledger.MoeAuthorization, uint32, uint32, []byte) error {
	return nil
}
func (f *drillMoeLedger) SubmitMoe(id string, r *pb.PipelineExecutionReceipt, _ int64) ([]ledger.StageShare, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submissions[id] = r.RequestId == id
	return nil, nil
}
func (f *drillMoeLedger) CancelMoe(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancellations[id] = true
	return nil
}
func (f *drillMoeLedger) State(string) (string, error) { return "settled", nil }

func TestMoeGenerationCollisionDrill(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		name := "serialized"
		if disabled {
			name = "disabled"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("EMBER_MOE_SERIALIZE", "")
			t.Setenv("EMBER_MOE_QUEUE_DEPTH", "8")
			t.Setenv("EMBER_MOE_QUEUE_WAIT", "10m")
			if disabled {
				t.Setenv("EMBER_MOE_SERIALIZE", "0")
			}
			g, fixture := moeFixture(t)
			f := &drillMoeLedger{fakeMoeLedger: fixture, submissions: map[string]bool{}, cancellations: map[string]bool{}}
			g.Ledger = f
			receipts := map[string]string{}
			for _, id := range []string{"drill-a", "drill-b"} {
				raw, err := proto.Marshal(&pb.PipelineExecutionReceipt{RequestId: id, InputTokens: 3, OutputTokens: 1, Moe: &pb.MoeExecutionData{}})
				if err != nil {
					t.Fatal(err)
				}
				receipts[id] = base64.StdEncoding.EncodeToString(raw)
			}
			raw, _ := json.Marshal(receipts)
			script := fmt.Sprintf(`#!/usr/bin/env python3
import json, sys, time
from pathlib import Path
if sys.argv[2] == 'tokenize_prompt':
    print('{"input_tokens":3,"tokenizer_version":"test"}')
    sys.exit(0)
request_id = json.load(sys.stdin)['request_id']
state = Path('stage-state')
state.write_text(request_id)
time.sleep(0.5)
if state.read_text() != request_id:
    # Corrupt receipt binding poisons both participants, like a shared stage.
    Path('collision').touch()
time.sleep(0.2)
if Path('collision').exists():
    sys.exit(1)
receipts = %s
print(json.dumps({'type':'token','text':'Paris'}))
print(json.dumps({'type':'result','output_tokens':1,'receipt_b64':receipts[request_id]}))
`, raw)
			path := filepath.Join(g.Moe.WorkDir, "collision-executor")
			if err := os.WriteFile(path, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			g.Moe.Python = path
			var wg sync.WaitGroup
			start := make(chan struct{})
			responses := make([]*httptest.ResponseRecorder, 2)
			for i, id := range []string{"drill-a", "drill-b"} {
				responses[i] = httptest.NewRecorder()
				wg.Add(1)
				go func(i int, id string) {
					defer wg.Done()
					<-start
					g.serveMoe(responses[i], httptest.NewRequest("POST", "/v1/chat/completions", nil), request{Model: MoeProfile, MaxTokens: 2, Stream: true}, "prompt", "payer", id)
				}(i, id)
			}
			close(start)
			wg.Wait()
			f.mu.Lock()
			defer f.mu.Unlock()
			t.Logf("%s: submitted=%d cancelled=%d (two generations)", name, len(f.submissions), len(f.cancellations))
			for i, id := range []string{"drill-a", "drill-b"} {
				body := responses[i].Body.String()
				if disabled {
					if f.submissions[id] || !f.cancellations[id] || !strings.Contains(body, "event: error") || strings.Contains(body, "[DONE]") {
						t.Fatalf("%s should fail: %s", id, body)
					}
				} else if !f.submissions[id] || !strings.Contains(body, `"content":"Paris"`) || !strings.Contains(body, "data: [DONE]") {
					t.Fatalf("%s should succeed: %s", id, body)
				}
			}
		})
	}
}
