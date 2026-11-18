package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// eventReader bounds line framing memory independently of result length. Large
// lines and all result frames live on the configured working-directory disk.
type eventReader struct {
	r       *bufio.Reader
	line    []byte
	err     error
	file    *os.File
	workDir string
	spool   bool
}

func newEventReader(r io.Reader, dir ...string) *eventReader {
	workDir := "."
	if len(dir) > 0 && dir[0] != "" {
		workDir = dir[0]
	}
	return &eventReader{r: bufio.NewReaderSize(r, 64<<10), workDir: workDir, spool: len(dir) > 0}
}
func (r *eventReader) Close() {
	if r.file != nil {
		name := r.file.Name()
		r.file.Close()
		os.Remove(name)
		r.file = nil
	}
}
func (r *eventReader) Scan() bool {
	r.Close()
	if !r.spool {
		if r.err != nil {
			return false
		}
		r.line, r.err = r.r.ReadBytes('\n')
		if r.err == io.EOF && len(r.line) > 0 {
			r.err = io.ErrUnexpectedEOF
		}
		return r.err == nil
	}
	r.line = nil
	if r.err != nil {
		return false
	}
	for {
		chunk, err := r.r.ReadSlice('\n')
		if r.file == nil && (err == bufio.ErrBufferFull) {
			r.file, r.err = createPipelineTemp(r.workDir, ".pipeline-result-*")
			if r.err != nil {
				return false
			}
		}
		if r.file != nil {
			if _, r.err = r.file.Write(chunk); r.err != nil {
				return false
			}
		} else {
			r.line = append(r.line, chunk...)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF && (len(chunk) > 0 || r.file != nil) {
			err = io.ErrUnexpectedEOF
		}
		r.err = err
		if err != nil {
			return false
		}
		if r.file == nil {
			var event struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(r.line, &event) == nil && event.Type == "result" {
				r.file, r.err = createPipelineTemp(r.workDir, ".pipeline-result-*")
				if r.err != nil {
					return false
				}
				_, r.err = io.Copy(r.file, bytes.NewReader(r.line))
				r.line = nil
			}
		}
		return r.err == nil
	}
}
func (r *eventReader) Bytes() []byte { return r.line }
func (r *eventReader) Err() error {
	if r.err == io.EOF {
		return nil
	}
	return r.err
}

// Wait never owns request liveness. On errors callers cancel first; even a
// child that closes stdout but stays alive gets killed and reaped here.
func waitExecutor(cmd *exec.Cmd) error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Second):
	}
	_ = cmd.Process.Kill()
	select {
	case err := <-done:
		return fmt.Errorf("executor completion timeout: %v", err)
	case <-time.After(time.Second):
		return fmt.Errorf("executor reap timeout")
	}
}

type stderrTail struct {
	mu   sync.Mutex
	data []byte
}

func (s *stderrTail) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = append(s.data, p...)
	if len(s.data) > 4096 {
		s.data = append([]byte(nil), s.data[len(s.data)-4096:]...)
	}
	return len(p), nil
}
func (s *stderrTail) String() string { s.mu.Lock(); defer s.mu.Unlock(); return string(s.data) }

func persistExecutorFailure(workDir, id, kind string, stderr string) string {
	dir := os.Getenv("EMBER_GATEWAY_FAILURE_DIR")
	if dir == "" {
		dir = filepath.Join(workDir, "gateway-failures")
	}
	// IDs are external input; never allow them to escape the failure directory.
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, id)
	if len(safe) > 160 {
		safe = safe[:160]
	}
	if len(stderr) > 4096 {
		stderr = stderr[len(stderr)-4096:]
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		log.Printf("failure persistence id=%s error=%v", id, err)
		return "unavailable"
	}
	f, err := os.CreateTemp(dir, safe+"-"+kind+"-*.stderr")
	if err == nil {
		_, err = f.WriteString(stderr)
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
	}
	if err != nil {
		log.Printf("failure persistence id=%s error=%v", id, err)
		return "unavailable"
	}
	return f.Name()
}

func createPipelineTemp(dir, pattern string) (*os.File, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	// Unlink immediately: evidence retention is an explicit copy into the durable
	// audit queue, and a killed gateway must not leave multi-GB scratch files.
	if err = os.Remove(f.Name()); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
