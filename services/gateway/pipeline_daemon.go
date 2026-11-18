package gateway

import (
	"bytes"
	"context"
	"ember/services/jsonspool"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Admission is configurable (including unlimited). Two exclusive slots preserve
// concurrent progress while bounding resident model/tokenizer processes to two.
// Requests beyond two wait for a free slot, respecting their context deadline.
type pipelineDaemon struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	scan *eventReader
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

func legacyPipeline(ctx context.Context, c PipelineConfig, raw []byte, stderr io.Writer) (io.ReadCloser, func() error, error) {
	cmd := pipelineCommand(c, ctx, false)
	cmd.Stdin = bytes.NewReader(raw)
	cmd.Stderr = stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	cmd.WaitDelay = time.Second
	if err = cmd.Start(); err != nil {
		return nil, nil, err
	}
	return out, func() error {
		if ctx.Err() != nil {
			_ = cmd.Process.Kill()
		}
		return waitExecutor(cmd)
	}, nil
}

func startPipeline(ctx context.Context, c PipelineConfig, raw []byte, stderr io.Writer) (io.ReadCloser, func() error, error) {
	// Deprecated: resident mode is disabled by default; production must use 0.
	if os.Getenv("EMBER_PIPELINE_DAEMON") != "1" {
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
		cmd.WaitDelay = time.Second
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
		scan := newEventReader(out, c.WorkDir)
		d = &pipelineDaemon{cmd, in, scan}
	}
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	acknowledged := make(chan struct{})
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
			_ = writer.CloseWithError(jobErr)
			// Keep exclusive ownership until the consumer has accepted the frames.
			<-acknowledged
			close(finished)
			<-watcherDone
			healthy = healthy && ctx.Err() == nil
			if !healthy {
				d.scan.Close()
				_ = d.cmd.Process.Kill()
				_ = d.in.Close()
				_ = waitExecutor(d.cmd)
				pool.slots <- nil
			} else {
				pool.slots <- d
			}
			done <- jobErr
		}()
		if _, jobErr = d.in.Write(append(append([]byte{}, raw...), '\n')); jobErr != nil {
			return
		}
		for d.scan.Scan() {
			if d.scan.file != nil {
				source, e := jsonspool.Index(d.scan.file)
				if e != nil {
					jobErr = e
					return
				}
				var kind string
				if jobErr = source.Decode("type", &kind); jobErr != nil {
					return
				}
				if _, jobErr = io.Copy(writer, source.Reader(jsonspool.Span{Start: 0, End: func() int64 { st, _ := d.scan.file.Stat(); return st.Size() }()})); jobErr != nil {
					return
				}
				d.scan.Close()
				continue
			}
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
				healthy = end.OK && ctx.Err() == nil
				return
			}
			if _, jobErr = writer.Write(line); jobErr != nil {
				return
			}
		}
		jobErr = d.scan.Err()
		if jobErr == nil {
			jobErr = fmt.Errorf("pipeline daemon exited mid-job")
		}
	}()
	return reader, func() error {
		_ = reader.Close()
		close(acknowledged)
		select {
		case err := <-done:
			return err
		case <-time.After(3 * time.Second):
			_ = d.cmd.Process.Kill()
			return fmt.Errorf("pipeline daemon cleanup timeout")
		}
	}, nil
}
