package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/pprof"
	"syscall"
	"time"
)

// The optional capture runs independently of application and consensus locks.
func startGoroutineSignal(home string) func() {
	signals := make(chan os.Signal, 1)
	done := make(chan struct{})
	exited := make(chan struct{})
	signal.Notify(signals, syscall.SIGUSR1)
	go func() {
		defer close(exited)
		for {
			select {
			case <-done:
				return
			case <-signals:
				if err := writeGoroutineDump(home); err != nil {
					log.Printf("goroutine capture failed: %v", err)
				}
			}
		}
	}()
	return func() {
		signal.Stop(signals)
		close(done)
		<-exited
	}
}

func writeGoroutineDump(home string) error {
	dir := filepath.Join(home, "debug")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	path := filepath.Join(dir, fmt.Sprintf("goroutine-%d.txt", time.Now().UnixNano()))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	err = pprof.Lookup("goroutine").WriteTo(f, 2)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		log.Printf("goroutine capture saved: %s", path)
	}
	return err
}
