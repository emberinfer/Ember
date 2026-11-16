package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPipelineDaemonLifecycle(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-python")
	err := os.WriteFile(script, []byte(`#!/usr/bin/env python3
import json,os,sys,time
resident = '--serve' in sys.argv
jobs = sys.stdin if resident else [sys.stdin.read()]
for line in jobs:
 job = json.loads(line)
 if job.get('crash'): os._exit(7)
 if job.get('sleep'): time.sleep(10)
 if not job.get('fail'):
  print(json.dumps({'type':'stage_start'}),flush=True)
  print(json.dumps({'type':'result','text':str(os.getpid())}),flush=True)
 if resident: print(json.dumps({'type':'job_end','ok':not job.get('fail'),'error':'test failure'}),flush=True)
`), 0700)
	if err != nil {
		t.Fatal(err)
	}
	c := PipelineConfig{Python: script, WorkDir: dir}
	t.Cleanup(func() {
		if value, ok := pipelinePools.LoadAndDelete(c.Python + "\x00" + c.WorkDir); ok {
			pool := value.(*pipelineDaemonPool)
			for i := 0; i < 2; i++ {
				if d := <-pool.slots; d != nil {
					_ = d.cmd.Process.Kill()
					_ = d.in.Close()
					_ = d.cmd.Wait()
				}
			}
		}
	})
	run := func(ctx context.Context, job string) (string, error) {
		t.Helper()
		var stderr bytes.Buffer
		reader, wait, err := startPipeline(ctx, c, []byte(job), &stderr)
		if err != nil {
			return "", err
		}
		scanner := bufio.NewScanner(reader)
		pid := ""
		for scanner.Scan() {
			var e pipelineEvent
			if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
				t.Fatal(err)
			}
			if e.Type == "job_end" {
				t.Fatal("end marker leaked")
			}
			if e.Type == "result" {
				pid = e.Text
			}
		}
		return pid, wait()
	}
	// Round-robin free slots: warm both, then verify each is reused.
	first, err := run(context.Background(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	second, err := run(context.Background(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	third, err := run(context.Background(), `{}`)
	if err != nil || first != third {
		t.Fatalf("reuse %s %s %v", first, third, err)
	}
	fourth, err := run(context.Background(), `{}`)
	if err != nil || second != fourth {
		t.Fatal("second slot not reused", err)
	}
	if _, err = run(context.Background(), `{"fail":true}`); err == nil {
		t.Fatal("failure accepted")
	}
	if _, err = run(context.Background(), `{}`); err != nil {
		t.Fatal(err)
	}
	if _, err = run(context.Background(), `{"crash":true}`); err == nil {
		t.Fatal("crash accepted")
	}
	if _, err = run(context.Background(), `{}`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err = run(ctx, `{"sleep":true}`); err == nil {
		t.Fatal("timeout accepted")
	}
	if _, err = run(context.Background(), `{}`); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EMBER_PIPELINE_DAEMON", "0")
	if _, err = run(context.Background(), `{}`); err != nil {
		t.Fatal(err)
	}
}
