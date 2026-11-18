package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestGoroutineSignal(t *testing.T) {
	home := t.TempDir()
	stop := startGoroutineSignal(home)
	defer stop()
	if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		files, _ := filepath.Glob(filepath.Join(home, "debug", "goroutine-*.txt"))
		if len(files) == 1 {
			raw, err := os.ReadFile(files[0])
			if err == nil && strings.Contains(string(raw), "TestGoroutineSignal") {
				info, err := os.Stat(files[0])
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatalf("dump permissions: %v, %v", info, err)
				}
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("signal did not produce a goroutine dump")
}
