package main

import (
	"ember/services/gateway"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

func privacyGateway(args []string) {
	f := flag.NewFlagSet("privacy-gateway", flag.ExitOnError)
	listen := f.String("listen", "127.0.0.1:19095", "HTTP listen address; terminate TLS before public exposure")
	executor := f.String("executor", "", "registered P0 executor HTTP endpoint")
	captureDir := f.String("capture-dir", "", "optional ciphertext capture directory for diagnostics")
	_ = f.Parse(args)
	var capture func([]byte, []byte)
	if *captureDir != "" {
		if err := os.MkdirAll(*captureDir, 0700); err != nil {
			log.Fatal(err)
		}
		var sequence uint64
		capture = func(req, resp []byte) {
			n := atomic.AddUint64(&sequence, 1)
			for suffix, data := range map[string][]byte{"request": req, "response": resp} {
				if err := os.WriteFile(filepath.Join(*captureDir, fmt.Sprintf("%06d.%s", n, suffix)), data, 0600); err != nil {
					log.Print("ciphertext capture failed")
				}
			}
		}
	}
	handler, err := gateway.PrivacyProxy(*executor, capture)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 6 * time.Minute, MaxHeaderBytes: 16384}
	log.Fatal(server.ListenAndServe())
}
