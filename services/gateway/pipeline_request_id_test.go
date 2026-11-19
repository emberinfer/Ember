package gateway

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHTTPPipelineRequestFitsSpotIdentity(t *testing.T) {
	t.Setenv("EMBER_PIPELINE_DAEMON", "0")
	g, b, profile := availabilityFixture(t)
	profile.ChainId = "ember-model"
	g.DemoBootstrap = true
	if err := b.Create("consumer", 100000); err != nil {
		t.Fatal(err)
	}
	captured := filepath.Join(g.Pipeline.WorkDir, "executor-job.json")
	script := fmt.Sprintf(`#!/usr/bin/env python3
import json,sys
if sys.argv[2] == 'tokenize_prompt':
 print(json.dumps(dict(input_tokens=1,tokenizer_version='test')))
else:
 job=json.load(sys.stdin)
 with open(%q,'w') as f: json.dump(job,f)
 if len(bytes.fromhex(job['request_id'])) != 32:
  raise ValueError('request ID must be 32 bytes')
 raise RuntimeError('stage 0 unavailable: spot identity accepted')
`, captured)
	if err := os.WriteFile(g.Pipeline.Python, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"ember-model","messages":[{"role":"user","content":"hello"}],"max_tokens":1}`))
	req.Header.Set("Authorization", "Bearer consumer")
	response := httptest.NewRecorder()
	g.ServeHTTP(response, req)
	if response.Code != 502 || !strings.Contains(response.Body.String(), "spot identity accepted") {
		t.Fatal(response.Code, response.Body.String())
	}
	raw, err := os.ReadFile(captured)
	if err != nil {
		t.Fatal(err)
	}
	var job pipelineInput
	if err := json.Unmarshal(raw, &job); err != nil {
		t.Fatal(err)
	}
	identity, err := hex.DecodeString(job.RequestID)
	if err != nil || len(identity) != 32 {
		t.Fatalf("spot identity: %q: %v", job.RequestID, err)
	}
	balance, _, err := b.Balance("consumer")
	if err != nil || balance != 100000 || b.freezes != 1 || b.cancels != 1 {
		t.Fatal(balance, b.freezes, b.cancels, err)
	}
}
