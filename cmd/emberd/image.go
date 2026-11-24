package main

import (
	"context"
	pb "ember/proto"
	"ember/services/gateway"
	"ember/services/ledger"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

func wireImages(api *gateway.Gateway, book *ledger.ChainLedger, enabled bool, database, spool, keyFile string) error {
	if !enabled {
		return nil
	}
	if book == nil {
		return errors.New("--image-enabled requires --ledger-backend=chain")
	}
	if spool == "" {
		spool = database + ".image-spool"
	}
	if keyFile == "" {
		keyFile = database + ".image.key"
	}
	key, err := loadAuditKey(keyFile)
	if err != nil {
		return err
	}
	store, err := gateway.OpenImageStore(spool, key)
	if err != nil {
		return err
	}
	rpc, domain, coordinator, accounts, mu := book.ImageTransport()
	backend, err := gateway.NewImageRPCBackendOn(rpc, domain, coordinator, accounts, mu)
	if err != nil {
		return err
	}
	api.Image = &gateway.ImageService{Backend: backend, Store: store, ChainID: domain}
	api.Image.Audit = func(ctx context.Context, node, hold string, job []byte) ([]byte, error) {
		var result []byte
		err := api.Tunnel.Audit(ctx, node, hold, fmt.Sprintf("image-audit-%s-%d", hold, time.Now().UnixNano()), job, func(frame *pb.TunnelFrame) error {
			if frame.Kind != "audit_result" {
				return errors.New("unexpected image audit frame")
			}
			result = append([]byte(nil), frame.Payload...)
			return nil
		})
		return result, err
	}
	return nil
}

var imageFinalization sync.Mutex

func watchImages(ctx context.Context, api *gateway.Gateway, ready func()) error {
	return api.Image.Backend.(*gateway.ImageRPCBackend).WatchBlocks(ctx, func(ctx context.Context) error {
		imageFinalization.Lock()
		defer imageFinalization.Unlock()
		// A failed hold must be retried on the next committed block, including after
		// restart; it must not kill the shared subscription or the text daemon.
		if err := api.Image.FinalizedBlock(ctx); err != nil {
			log.Printf("IMAGE FINALIZED: %v", err)
		}
		return nil
	}, ready)
}

func mountImages(mux *http.ServeMux, api *gateway.Gateway) {
	if api.Image != nil {
		mux.Handle("/v1/images/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Spool uploads must remain available while a generation is in flight.
			if strings.TrimPrefix(r.URL.Path, "/v1/images/") == "generations" {
				imageFinalization.Lock()
				defer imageFinalization.Unlock()
			}
			api.ServeHTTP(w, r)
		}))
	}
}
