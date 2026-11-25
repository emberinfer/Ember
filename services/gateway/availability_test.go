package gateway

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "ember/proto"
	"ember/services/ledger"
	"ember/services/scheduler"
	"ember/services/tunnel"
)

type availabilityLedger struct {
	ledger.Backend
	freezes, cancels int
	afterFreeze      func()
	cancelErr        error
	beforeCancel     func()
}

func (b *availabilityLedger) FreezePipeline(id, payer string, amount int64, auth []ledger.StageAuthorization, layers uint32) error {
	b.freezes++
	if err := b.Backend.FreezePipeline(id, payer, amount, auth, layers); err != nil {
		return err
	}
	if b.afterFreeze != nil {
		b.afterFreeze()
	}
	return nil
}
func (b *availabilityLedger) CancelPipeline(id string) error {
	b.cancels++
	if b.beforeCancel != nil {
		b.beforeCancel()
	}
	if b.cancelErr != nil {
		return b.cancelErr
	}
	return b.Backend.CancelPipeline(id)
}

func availabilityFixture(t *testing.T) (*Gateway, *availabilityLedger, *pb.ResourceProfile) {
	t.Helper()
	dir := t.TempDir()
	book, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	for _, id := range []string{"payer", "stage"} {
		if err := book.Create(id, 100000); err != nil {
			t.Fatal(err)
		}
	}
	if err := book.Stake("stage", 50000); err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "key")
	if err := os.WriteFile(keyPath, key, 0600); err != nil {
		t.Fatal(err)
	}
	// An isolated subprocess supplies tokenizer output and a stage-unavailable
	// failure, without model loading, network access or a running stage.
	python := filepath.Join(dir, "python")
	script := `#!/bin/sh
if [ "$2" = tokenize_prompt ]; then
 printf '%s\n' '{"input_tokens":1,"tokenizer_version":"test"}'
else
 echo 'RuntimeError: stage 0 unavailable: no available candidates' >&2
 exit 1
fi
`
	if err := os.WriteFile(python, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	b := &availabilityLedger{Backend: book}
	p := &pb.ResourceProfile{NodeId: "stage", ChainId: "model", ExecutionProfileVer: "v", ManifestRoot: []byte{1}, ShardDynamicRange: true, ShardTotalLayers: 28, ShardStartLayer: 0, ShardEndLayer: 28, ShardHasEmbedding: true, ShardHasLmHead: true, ExpiresUnix: time.Now().Add(time.Minute).Unix()}
	g := &Gateway{Ledger: b, Price: 98, Profile: "v", Manifest: []byte{1}, TotalLayers: 28, Pipeline: &PipelineConfig{Python: python, ModelPath: "unused", SchedulerKeyFile: keyPath, WorkDir: dir, DynamicRanges: true, MinStageLayers: 1}}
	g.Candidates = func() []scheduler.Candidate { return []scheduler.Candidate{{Profile: p, Online: true, Stake: 50000}} }
	return g, b, p
}

func TestPipelineCandidateFastFail(t *testing.T) {
	for _, scenario := range []string{"empty", "expired", "lost_before_freeze", "expired_during_freeze", "offline_during_freeze", "subprocess_before_token", "subprocess_after_token", "daemon_job_failure_before_token", "daemon_job_failure_after_token", "cancel_failure", "refresh_recovers"} {
		t.Run(scenario, func(t *testing.T) {
			if scenario == "subprocess_before_token" || scenario == "subprocess_after_token" {
				t.Setenv("EMBER_PIPELINE_DAEMON", "0")
			}
			g, b, p := availabilityFixture(t)
			calls := 0
			original := g.Candidates
			g.Candidates = func() []scheduler.Candidate {
				calls++
				if scenario == "empty" || (scenario == "lost_before_freeze" && calls > 1) || (scenario == "refresh_recovers" && calls == 1) {
					return nil
				}
				return original()
			}
			if scenario == "expired" {
				p.ExpiresUnix = time.Now().Add(-time.Second).Unix()
			}
			if scenario == "expired_during_freeze" {
				// Cross a real wall-clock expiry while the ledger freeze is in flight.
				p.ExpiresUnix = time.Now().Unix() + 1
				b.afterFreeze = func() { time.Sleep(time.Until(time.Unix(p.ExpiresUnix, 0))) }
			}
			if scenario == "offline_during_freeze" {
				b.afterFreeze = func() { g.Tunnel = tunnel.New() }
			}
			if scenario == "subprocess_after_token" {
				script := `#!/bin/sh
if [ "$2" = tokenize_prompt ]; then
 echo '{"input_tokens":1,"tokenizer_version":"test"}'
else
 echo '{"type":"token","text":"hello"}'
 echo 'RuntimeError: stage 0 unavailable: no available candidates' >&2
 exit 1
fi
`
				if err := os.WriteFile(g.Pipeline.Python, []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasPrefix(scenario, "daemon_job_failure") {
				t.Setenv("EMBER_PIPELINE_DAEMON", "1")
				token := ""
				if scenario == "daemon_job_failure_after_token" {
					token = `echo '{"type":"token","text":"hello"}'`
				}
				script := `#!/bin/sh
if [ "$2" = tokenize_prompt ]; then
 echo '{"input_tokens":1,"tokenizer_version":"test"}'
elif [ "$3" = --serve ]; then
 while IFS= read -r job; do
 ` + token + `
 echo '{"type":"job_end","ok":false,"error":"stage 0 unavailable: no available candidates"}'
 done
else
 exit 2
fi
`
				if err := os.WriteFile(g.Pipeline.Python, []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "cancel_failure" {
				b.cancelErr = errors.New("backend unavailable")
			}
			w := httptest.NewRecorder()
			b.beforeCancel = func() {
				if !w.Flushed || !strings.Contains(w.Body.String(), "event: error") {
					t.Fatal("failure not flushed before cancellation")
				}
			}
			start := time.Now()
			g.servePipeline(w, httptest.NewRequest("POST", "/", nil), request{Model: "model", MaxTokens: 1, Stream: true}, "prompt", "payer", "request-test")
			elapsed := time.Since(start)
			if elapsed > 2*time.Second {
				t.Fatalf("slow failure: %s", elapsed)
			}
			body := w.Body.String()
			if !strings.Contains(body, "event: error\n") || !strings.Contains(body, "id: request-test\n") || !strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") {
				t.Fatal(w.Code, body, w.Header())
			}
			balance, _, err := b.Balance("payer")
			if err != nil {
				t.Fatal(err)
			}
			wantFreeze := 1
			if scenario == "empty" || scenario == "expired" || scenario == "lost_before_freeze" {
				wantFreeze = 0
			}
			if b.freezes != wantFreeze || b.cancels != wantFreeze {
				t.Fatal("freeze/cancel", b.freezes, b.cancels)
			}
			wantBalance := int64(100000)
			if scenario == "cancel_failure" {
				wantBalance -= 98
				if !strings.Contains(body, "freeze release failed: backend unavailable") {
					t.Fatal(body)
				}
			}
			if balance != wantBalance {
				t.Fatalf("balance %d want %d", balance, wantBalance)
			}
			if strings.HasPrefix(scenario, "subprocess") && !strings.Contains(body, "no available candidates") {
				t.Fatal(body)
			}
			if strings.HasPrefix(scenario, "daemon_job_failure") && !strings.Contains(body, "stage 0 unavailable: no available candidates") {
				t.Fatal(body)
			}
			t.Logf("elapsed=%s freeze=%d cancel=%d balance=%d status=%d frame=%s", elapsed, b.freezes, b.cancels, balance, w.Code, strings.TrimSpace(body))
		})
	}
}

func TestWholeModelNoCandidatesErrorFrame(t *testing.T) {
	for _, expired := range []bool{false, true} {
		g, b, p := availabilityFixture(t)
		g.Pipeline = nil
		p.ChainId = "ember-model"
		p.ShardTotalLayers, p.ShardEndLayer = 0, 0
		p.ShardHasEmbedding, p.ShardHasLmHead = false, false
		if err := b.IssueToken("consumer", 100000, strings.Repeat("t", 32)); err != nil {
			t.Fatal(err)
		}
		p.ExpiresUnix = time.Now().Unix() - 1
		if !expired {
			g.Candidates = func() []scheduler.Candidate { return nil }
		}
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"ember-model","messages":[{"role":"user","content":"hello"}],"max_tokens":1,"stream":true}`))
		r.Header.Set("Authorization", "Bearer "+strings.Repeat("t", 32))
		w := httptest.NewRecorder()
		start := time.Now()
		g.ServeHTTP(w, r)
		if w.Code != 503 || !strings.Contains(w.Body.String(), "event: error") || w.Header().Get("X-Request-ID") == "" {
			t.Fatal(w.Code, w.Body.String())
		}
		balance, _, err := b.Balance("consumer")
		if err != nil || balance != 100000 || b.freezes != 0 {
			t.Fatal(balance, err, b.freezes)
		}
		t.Logf("expired=%v elapsed=%s balance=%d", expired, time.Since(start), balance)
	}
}

func TestCandidateTrackerLeaseSupersedesPinnedProfileExpiry(t *testing.T) {
	now := time.Now()
	p := &pb.ResourceProfile{ExpiresUnix: now.Unix() - 1}
	c := scheduler.Candidate{Profile: p, LiveUntilUnix: now.Unix() + 120}
	if len(unexpiredCandidates([]scheduler.Candidate{c}, now)) != 1 {
		t.Fatal("renewed tracker lease rejected")
	}
	p.ExpiresUnix = now.Unix() + 120
	c.LiveUntilUnix = now.Unix() - 1
	if len(unexpiredCandidates([]scheduler.Candidate{c}, now)) != 0 {
		t.Fatal("expired tracker lease accepted")
	}
}

func TestWorkerCatalogEligibility(t *testing.T) {
	g, _, p := availabilityFixture(t)
	g.ChainID = "model"
	if ready, _, reason := g.availability(); !ready {
		t.Fatal("complete pipeline rejected", reason)
	}
	p.ShardEndLayer = 14
	p.ShardHasLmHead = false
	if ready, _, reason := g.availability(); ready || reason == "" {
		t.Fatal("partial shard cover advertised ready")
	}
	g.Pipeline = nil
	p.ShardTotalLayers = 0
	p.ShardEndLayer = 0
	p.ShardHasEmbedding = false
	if ready, n, reason := g.availability(); !ready || n != 1 {
		t.Fatal(ready, n, reason)
	}
	p.ExpiresUnix = time.Now().Add(-time.Minute).Unix()
	if ready, _, reason := g.availability(); ready || reason == "" {
		t.Fatal("expired executor ready")
	}
	p.ExpiresUnix = time.Now().Add(time.Minute).Unix()
	g.ExecExclude = map[string]bool{p.NodeId: true}
	if ready, _, _ := g.availability(); ready {
		t.Fatal("excluded executor ready")
	}
	g.ExecExclude = nil
	g.CheckModelTrust = func(string) error { return errors.New("publication invalid") }
	if ready, _, reason := g.availability(); ready || reason != "publication invalid" {
		t.Fatal(ready, reason)
	}
	g.CheckModelTrust = nil
	g.Manifest = nil
	if ready, _, reason := g.availability(); ready || reason != "profile context incomplete" {
		t.Fatal(ready, reason)
	}
}
