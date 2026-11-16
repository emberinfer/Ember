package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

// Admission is configurable (including unlimited). Two exclusive slots preserve
// concurrent progress while bounding resident model/tokenizer processes to two.
// Requests beyond two wait for a free slot, respecting their context deadline.
type pipelineDaemon struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	scan *bufio.Scanner
}
type pipelineDaemonPool struct{ slots chan *pipelineDaemon }

var pipelinePools sync.Map

func pipelineCommand(c PipelineConfig, ctx context.Context, resident bool) *exec.Cmd {
	args := []string{"-m", "pipeline_client"}
	if resident {
		args = append(args, "--serve")
	}
	cmd := exec.CommandContext(ctx, c.Python, args...)
	cmd.Dir = c.WorkDir
	cmd.Env = append(os.Environ(), "PYTHONPATH=core-py:proto:node")
	return cmd
}

func legacyPipeline(ctx context.Context, c PipelineConfig, raw []byte, stderr *bytes.Buffer) (io.ReadCloser, func() error, error) {
	cmd := pipelineCommand(c, ctx, false)
	cmd.Stdin = bytes.NewReader(raw)
	cmd.Stderr = stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, nil, err
	}
	return out, cmd.Wait, nil
}

func startPipeline(ctx context.Context, c PipelineConfig, raw []byte, stderr *bytes.Buffer) (io.ReadCloser, func() error, error) {
	if os.Getenv("EMBER_PIPELINE_DAEMON") == "0" {
		return legacyPipeline(ctx, c, raw, stderr)
	}
	key := c.Python + "\x00" + c.WorkDir
	fresh := &pipelineDaemonPool{slots: make(chan *pipelineDaemon, 2)}
	fresh.slots <- nil
	fresh.slots <- nil
	value, _ := pipelinePools.LoadOrStore(key, fresh)
	pool := value.(*pipelineDaemonPool)
	var d *pipelineDaemon
	select {
	case d = <-pool.slots:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	if d == nil {
		cmd := pipelineCommand(c, context.Background(), true)
		cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
		// Never write resident stderr into a buffer belonging to a completed request.
		cmd.Stderr = os.Stderr
		in, err := cmd.StdinPipe()
		if err != nil {
			pool.slots <- nil
			return legacyPipeline(ctx, c, raw, stderr)
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			in.Close()
			pool.slots <- nil
			return legacyPipeline(ctx, c, raw, stderr)
		}
		if err = cmd.Start(); err != nil {
			in.Close()
			out.Close()
			pool.slots <- nil
			return legacyPipeline(ctx, c, raw, stderr)
		}
		scan := bufio.NewScanner(out)
		scan.Buffer(make([]byte, 65536), 128*1024*1024)
		d = &pipelineDaemon{cmd, in, scan}
	}
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	finished := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			_ = d.cmd.Process.Kill()
			_ = reader.CloseWithError(ctx.Err())
		case <-finished:
		}
	}()
	go func() {
		healthy := false
		var jobErr error
		defer func() {
			close(finished)
			<-watcherDone
			healthy = healthy && ctx.Err() == nil
			if !healthy {
				_ = d.cmd.Process.Kill()
				_ = d.in.Close()
				_ = d.cmd.Wait()
				pool.slots <- nil
			} else {
				pool.slots <- d
			}
			_ = writer.CloseWithError(jobErr)
			done <- jobErr
		}()
		if _, jobErr = d.in.Write(append(append([]byte{}, raw...), '\n')); jobErr != nil {
			return
		}
		for d.scan.Scan() {
			line := append([]byte{}, d.scan.Bytes()...)
			var end struct {
				Type  string `json:"type"`
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			}
			if jobErr = json.Unmarshal(line, &end); jobErr != nil {
				return
			}
			if end.Type == "job_end" {
				if !end.OK {
					jobErr = fmt.Errorf("pipeline daemon job: %s", end.Error)
				}
				healthy = ctx.Err() == nil
				return
			}
			if _, jobErr = writer.Write(append(line, '\n')); jobErr != nil {
				return
			}
		}
		jobErr = d.scan.Err()
		if jobErr == nil {
			jobErr = fmt.Errorf("pipeline daemon exited mid-job")
		}
	}()
	return reader, func() error { _ = reader.Close(); return <-done }, nil
}
