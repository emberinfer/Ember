package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestPipelineBlockedWriter(t *testing.T) {
	t.Setenv("EMBER_PIPELINE_DAEMON", "0")
	t.Setenv("EMBER_GATEWAY_FAILURE_DIR", t.TempDir())
	g, b, _ := availabilityFixture(t)
	pidfile := filepath.Join(g.Pipeline.WorkDir, "pid")
	script := fmt.Sprintf(`#!/usr/bin/env python3
import sys,os,json
if sys.argv[2]=='tokenize_prompt':
 print('{"input_tokens":1,"tokenizer_version":"test"}')
else:
 open(%q,'w').write(str(os.getpid()))
 print('{"type":"token","text":"hello"}',flush=True)
 print('not json',flush=True)
 while True: os.write(1,b'x'*65536)
`, pidfile)
	os.WriteFile(g.Pipeline.Python, []byte(script), 0700)
	// Safety deadline cleans up even on the original wedged implementation.
	g.RequestTimeout = 3 * time.Second
	w := &lockedRecorder{ResponseRecorder: httptest.NewRecorder()}
	started := time.Now()
	done := make(chan struct{})
	go func() {
		g.servePipeline(w, httptest.NewRequest("POST", "/", nil), request{Model: "model", MaxTokens: 1, Stream: true}, "prompt", "payer", "blocked")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		raw, _ := os.ReadFile(pidfile)
		pid, _ := strconv.Atoi(string(raw))
		t.Logf("pending at 200ms: error_frame=%v child_alive=%v pid=%d", strings.Contains(w.snapshot(), "event: error"), pid > 0 && syscall.Kill(pid, 0) == nil, pid)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("handler did not terminate")
		}
	}
	elapsed := time.Since(started)
	raw, _ := os.ReadFile(pidfile)
	pid, _ := strconv.Atoi(string(raw))
	if pid == 0 || syscall.Kill(pid, 0) == nil {
		t.Fatal("child not reaped", pid)
	}
	if b.cancels != 1 || !strings.Contains(w.Body.String(), "event: error") || !strings.Contains(w.Body.String(), "id: blocked") {
		t.Fatal(b.cancels, w.Body.String())
	}
	t.Logf("terminal error; Cancel=%d; pid=%d reaped; elapsed=%s", b.cancels, pid, elapsed)
	if elapsed > 2*time.Second {
		t.Fatalf("wedged until safety deadline: %s", elapsed)
	}
}

func TestExecutorFailureMarker(t *testing.T) {
	for _, kind := range []string{"pipeline", "moe"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("EMBER_GATEWAY_FAILURE_DIR", dir)
			t.Setenv("EMBER_PIPELINE_DAEMON", "0")
			marker := "GWROBUST_UNIQUE_STDERR_" + kind
			script := "#!/bin/sh\nif [ \"$2\" = tokenize_prompt ]; then\n echo '{\"input_tokens\":3,\"tokenizer_version\":\"test\"}'\nelse\n printf '%05000d' 0 >&2\n echo '" + marker + "' >&2\n exit 1\nfi\n"
			w := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/", nil)
			if kind == "pipeline" {
				g, b, _ := availabilityFixture(t)
				os.WriteFile(g.Pipeline.Python, []byte(script), 0700)
				g.servePipeline(w, r, request{Model: "model", MaxTokens: 1, Stream: true}, "prompt", "payer", "marker-pipeline")
				if b.cancels != 1 {
					t.Fatal("not cancelled")
				}
			} else {
				g, b := moeFixture(t)
				g.Moe.Python = filepath.Join(g.Moe.WorkDir, "executor")
				os.WriteFile(g.Moe.Python, []byte(script), 0700)
				g.serveMoe(w, r, request{Model: MoeProfile, MaxTokens: 1, Stream: true}, "prompt", "payer", "marker-moe")
				if !b.cancelled {
					t.Fatal("not cancelled")
				}
			}
			files, _ := filepath.Glob(filepath.Join(dir, "marker-"+kind+"-"+kind+"-*.stderr"))
			if len(files) != 1 {
				t.Fatal(files)
			}
			raw, err := os.ReadFile(files[0])
			if err != nil || len(raw) != 4096 || !strings.Contains(string(raw), marker) {
				t.Fatal(string(raw), err)
			}
			st, _ := os.Stat(files[0])
			if st.Mode().Perm() != 0600 {
				t.Fatal(st.Mode())
			}
			t.Logf("request-id lookup %s retrieved %s", files[0], marker)
		})
	}
}

func TestPipelineProtocolTerminalOutcomes(t *testing.T) {
	for _, mode := range []string{"0", "1"} {
		for _, scenario := range []string{"short", "long", "no-result", "crash", "garbage", "unknown"} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				t.Setenv("EMBER_PIPELINE_DAEMON", mode)
				dir := t.TempDir()
				script := filepath.Join(dir, "executor")
				source := "#!/usr/bin/env python3\nimport sys,json,os\nresident='--serve' in sys.argv\njobs=sys.stdin if resident else [sys.stdin.read()]\nfor job in jobs:\n mode=json.loads(job)['scenario']\n print(json.dumps({'type':'stage_start'}),flush=True)\n if mode=='crash': os._exit(7)\n if mode in ('garbage','unknown'):\n  print('bad json' if mode=='garbage' else '{\"type\":\"surprise\"}',flush=True)\n  while True: os.write(1,b'x'*65536)\n if mode!='no-result':\n  print(json.dumps({'type':'result','text':'x'*(4*1024*1024 if mode=='long' else 1)}),flush=True)\n if resident: print(json.dumps({'type':'job_end','ok':True}),flush=True)\n"
				if err := os.WriteFile(script, []byte(source), 0700); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
				defer cancel()
				c := PipelineConfig{Python: script, WorkDir: dir}
				var stderr bytes.Buffer
				reader, wait, err := startPipeline(ctx, c, []byte(fmt.Sprintf("{\"scenario\":%q}", scenario)), &stderr)
				if err != nil {
					t.Fatal(err)
				}
				scan := newEventReader(reader)
				result := false
				length := 0
				for scan.Scan() {
					var e pipelineEvent
					err = json.Unmarshal(scan.Bytes(), &e)
					if err != nil {
						break
					}
					if e.Type == "result" {
						result = true
						length = len(e.Text)
					} else if e.Type != "stage_start" {
						err = fmt.Errorf("unexpected %s", e.Type)
						break
					}
				}
				if err == nil {
					err = scan.Err()
				}
				if err != nil {
					cancel()
					reader.Close()
				}
				waitErr := wait()
				if err == nil {
					err = waitErr
				}
				if err == nil && !result {
					err = fmt.Errorf("no result")
				}
				if scenario == "short" || scenario == "long" {
					if err != nil {
						t.Fatal(err)
					}
					want := 1
					if scenario == "long" {
						want = 4 * 1024 * 1024
					}
					if length != want {
						t.Fatal(length, want)
					}
				} else if err == nil {
					t.Fatal("missing terminal failure")
				}
				if mode == "1" {
					if v, ok := pipelinePools.LoadAndDelete(script + "\x00" + dir); ok {
						pool := v.(*pipelineDaemonPool)
						for i := 0; i < 2; i++ {
							select {
							case d := <-pool.slots:
								if d != nil {
									d.cmd.Process.Kill()
									d.in.Close()
									if e := waitExecutor(d.cmd); e == nil {
										t.Fatal("expected killed daemon")
									}
								}
							case <-time.After(time.Second):
								t.Fatal("slot not released")
							}
						}
					}
				}
				t.Logf("terminal result=%v text_bytes=%d error=%v", result, length, err)
			})
		}
	}
}

func TestPipelineReadFailuresReleaseFreeze(t *testing.T) {
	for _, mode := range []string{"0", "1"} {
		for _, scenario := range []string{"oversized", "unknown", "overlap", "truncated", "no-result", "crash"} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				t.Setenv("EMBER_PIPELINE_DAEMON", mode)
				t.Setenv("EMBER_GATEWAY_FAILURE_DIR", t.TempDir())
				g, b, _ := availabilityFixture(t)
				g.RequestTimeout = 4 * time.Second
				pidfile := filepath.Join(g.Pipeline.WorkDir, "pid")
				script := fmt.Sprintf("#!/usr/bin/env python3\nimport sys,os,time\nif sys.argv[2]=='tokenize_prompt':\n print('{\"input_tokens\":1,\"tokenizer_version\":\"test\"}')\nelse:\n open(%q,'w').write(str(os.getpid()))\n print('{\"type\":\"token\",\"text\":\"hello\"}',flush=True)\n mode=%q\n if mode=='crash': os.kill(os.getpid(),9)\n if mode=='no-result': sys.exit(0)\n if mode=='truncated':\n  os.write(1,b'{\"type\":'); sys.exit(0)\n if mode=='oversized': print('x'*(4*1024*1024),flush=True)\n if mode=='unknown': print('{\"type\":\"surprise\"}',flush=True)\n if mode=='overlap':\n  print('{\"type\":\"stage_start\"}',flush=True)\n  print('{\"type\":\"stage_start\"}',flush=True)\n while True: os.write(1,b'x'*65536)\n", pidfile, scenario)
				if err := os.WriteFile(g.Pipeline.Python, []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				w := httptest.NewRecorder()
				started := time.Now()
				g.servePipeline(w, httptest.NewRequest("POST", "/", nil), request{Model: "model", MaxTokens: 1, Stream: true}, "prompt", "payer", "read-failure")
				elapsed := time.Since(started)
				raw, _ := os.ReadFile(pidfile)
				pid, _ := strconv.Atoi(string(raw))
				if pid == 0 || syscall.Kill(pid, 0) != syscall.ESRCH {
					t.Fatal("not reaped", pid)
				}
				if elapsed > 2*time.Second || b.cancels != 1 || !strings.Contains(w.Body.String(), "event: error") || !strings.Contains(w.Body.String(), "id: read-failure") {
					t.Fatal(elapsed, b.cancels, w.Body.String())
				}
				if mode == "1" {
					v, _ := pipelinePools.LoadAndDelete(g.Pipeline.Python + "\x00" + g.Pipeline.WorkDir)
					pool := v.(*pipelineDaemonPool)
					for i := 0; i < 2; i++ {
						select {
						case d := <-pool.slots:
							if d != nil {
								d.cmd.Process.Kill()
								waitExecutor(d.cmd)
								t.Fatal("poisoned daemon reused")
							}
						case <-time.After(time.Second):
							t.Fatal("slot leaked")
						}
					}
				}
				t.Logf("error frame + Cancel=1; pid=%d reaped; elapsed=%s", pid, elapsed)
			})
		}
	}
}

func TestMoeBlockedWriter(t *testing.T) {
	t.Setenv("EMBER_GATEWAY_FAILURE_DIR", t.TempDir())
	g, b := moeFixture(t)
	g.RequestTimeout = 4 * time.Second
	pidfile := filepath.Join(g.Moe.WorkDir, "pid")
	g.Moe.Python = filepath.Join(g.Moe.WorkDir, "executor")
	script := fmt.Sprintf("#!/usr/bin/env python3\nimport os,sys\nif sys.argv[2]=='tokenize_prompt':\n print('{\"input_tokens\":3,\"tokenizer_version\":\"test\"}')\nelse:\n open(%q,'w').write(str(os.getpid()))\n print('{\"type\":\"token\",\"text\":\"hello\"}',flush=True)\n print('x'*(4*1024*1024),flush=True)\n while True: os.write(1,b'x'*65536)\n", pidfile)
	os.WriteFile(g.Moe.Python, []byte(script), 0700)
	w := httptest.NewRecorder()
	started := time.Now()
	g.serveMoe(w, httptest.NewRequest("POST", "/", nil), request{Model: MoeProfile, MaxTokens: 1, Stream: true}, "prompt", "payer", "moe-blocked")
	raw, _ := os.ReadFile(pidfile)
	pid, _ := strconv.Atoi(string(raw))
	if pid == 0 || syscall.Kill(pid, 0) != syscall.ESRCH {
		t.Fatal("not reaped", pid)
	}
	if time.Since(started) > 2*time.Second || !b.cancelled || !strings.Contains(w.Body.String(), "event: error") || !strings.Contains(w.Body.String(), "id: moe-blocked") {
		t.Fatal(w.Body.String())
	}
	t.Logf("MoE error frame + CancelMoe; pid=%d reaped; elapsed=%s", pid, time.Since(started))
}

func TestPipelineClosedStdoutReapBound(t *testing.T) {
	t.Setenv("EMBER_PIPELINE_DAEMON", "0")
	dir := t.TempDir()
	script := filepath.Join(dir, "executor")
	os.WriteFile(script, []byte("#!/usr/bin/env python3\nimport json,os,time\nprint(json.dumps({'type':'result','text':str(os.getpid())}),flush=True)\nos.close(1)\ntime.sleep(30)\n"), 0700)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	reader, wait, err := startPipeline(ctx, PipelineConfig{Python: script, WorkDir: dir}, []byte("{}"), &stderr)
	if err != nil {
		t.Fatal(err)
	}
	scan := newEventReader(reader)
	if !scan.Scan() {
		t.Fatal(scan.Err())
	}
	var event pipelineEvent
	if err = json.Unmarshal(scan.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if scan.Scan() || scan.Err() != nil {
		t.Fatal("expected clean stdout EOF", scan.Err())
	}
	started := time.Now()
	err = wait()
	elapsed := time.Since(started)
	pid, _ := strconv.Atoi(event.Text)
	if err == nil || elapsed > 2500*time.Millisecond || syscall.Kill(pid, 0) != syscall.ESRCH {
		t.Fatal(err, elapsed, pid)
	}
	t.Logf("stdout EOF but child stayed alive: bounded wait=%s; pid=%d reaped; error=%v", elapsed, pid, err)
}

// Allow the bounded repro to inspect response progress while the old handler is wedged.
type lockedRecorder struct {
	*httptest.ResponseRecorder
	mu sync.Mutex
}

func (w *lockedRecorder) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ResponseRecorder.Write(p)
}
func (w *lockedRecorder) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ResponseRecorder.WriteHeader(code)
}
func (w *lockedRecorder) Flush()           { w.mu.Lock(); defer w.mu.Unlock(); w.ResponseRecorder.Flush() }
func (w *lockedRecorder) snapshot() string { w.mu.Lock(); defer w.mu.Unlock(); return w.Body.String() }

var _ http.ResponseWriter = (*lockedRecorder)(nil)
