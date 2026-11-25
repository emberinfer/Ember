package supervisor

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

func TestWorkerListenerChild(t *testing.T) {
	if os.Getenv(workerListenersEnv) != "1" {
		return
	}
	args := os.Args[len(os.Args)-4:]
	httpListener, tunnelListener, err := AdoptWorkerListeners(args[1], args[3])
	if err != nil {
		t.Fatal(err)
	}
	defer httpListener.Close()
	defer tunnelListener.Close()
	for _, listener := range []net.Listener{httpListener, tunnelListener} {
		conn, err := listener.Accept()
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.WriteString(conn, listener.Addr().String())
		conn.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestConcurrentWorkerListeners(t *testing.T) {
	const slots = 32
	start := make(chan struct{})
	results := make(chan []string, slots)
	var done sync.WaitGroup
	defer done.Wait()
	release := make(chan struct{})
	defer close(release)
	for range slots {
		done.Add(1)
		go func() {
			defer done.Done()
			var addresses []string
			reported := false
			defer func() {
				if !reported {
					results <- nil
				}
			}()
			cmd := exec.Command(os.Args[0], "-test.run=^TestWorkerListenerChild$", "--")
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Error(err)
				return
			}
			<-start
			httpAddress, tunnelAddress, err := startWithWorkerListeners(cmd)
			if err != nil {
				stdin.Close()
				t.Error(err)
				return
			}
			defer func() {
				stdin.Close()
				if err := cmd.Wait(); err != nil {
					t.Errorf("worker exit: %v\n%s", err, output.String())
				}
			}()
			for _, address := range []string{httpAddress, tunnelAddress} {
				if err := verifyWorkerListener(address); err != nil {
					_ = cmd.Process.Kill()
					t.Error(err)
					return
				}
				addresses = append(addresses, address)
			}
			results <- addresses
			reported = true
			<-release
		}()
	}
	close(start)
	seen := make(map[string]bool)
	for range slots {
		for _, address := range <-results {
			if seen[address] {
				t.Errorf("duplicate worker port: %s", address)
			}
			seen[address] = true
		}
	}
	if len(seen) != slots*2 {
		t.Errorf("got %d unique worker ports, want %d", len(seen), slots*2)
	}
}

func verifyWorkerListener(address string) error {
	conn, err := net.DialTimeout("tcp", address, 10*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	reply, err := io.ReadAll(conn)
	if err != nil {
		return err
	}
	if string(reply) != address {
		return fmt.Errorf("worker at %s replied %q", address, reply)
	}
	return nil
}
