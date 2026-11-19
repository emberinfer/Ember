package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	pb "ember/proto"
	"ember/services/auditqueue"
	chain "ember/services/chain"
	"ember/services/gateway"
	"ember/services/jsonspool"
	"ember/services/ledger"
	"ember/services/tunnel"
	"google.golang.org/protobuf/proto"
)

func processPipelineSpot(id, profile string, book *ledger.ChainLedger, q *auditqueue.Queue, store *gateway.SpotStore, relay *tunnel.Server) (bool, error) {
	h, height, version, err := book.SpotRuntimeState(id)
	if err != nil {
		return true, err
	}
	if version < 14 || h.Pipeline == nil || h.Pipeline.SpotStepCount == 0 {
		return false, nil
	}
	if store == nil {
		return true, errors.New("registered spot spool unavailable")
	}

	if h.State == "dispute" && h.Sampling != nil && h.Unaudited {
		if err = book.FinishPipeline(id, "accept", time.Now(), 20); err != nil {
			return true, err
		}
		h, _, _, err = book.SpotRuntimeState(id)
		if err != nil {
			return true, err
		}
	}
	if removed, err := store.Cleanup(q, h); err != nil || removed {
		return true, err
	}
	if h.State != "dispute" {
		return true, nil
	}
	if h.AuditVerdict != "" {
		err = book.FinishPipeline(id, h.AuditVerdict, time.Now(), 20)
		return true, err
	}
	if _, err = book.ResolveAudit(id); err != nil {
		return true, err
	}
	h, _, _, err = book.SpotRuntimeState(id)
	if err != nil {
		return true, err
	}
	if h.State == "dispute" && h.Sampling != nil && h.Unaudited {
		if err = book.FinishPipeline(id, "accept", time.Now(), 20); err != nil {
			return true, err
		}
		h, _, _, err = book.SpotRuntimeState(id)
		if err != nil {
			return true, err
		}
	}
	if removed, err := store.Cleanup(q, h); err != nil || removed {
		return true, err
	}
	h, _, _, err = book.AuditRoute(id)
	if err != nil {
		return true, err
	}
	registered, err := book.NodeProfile(store.GatewayID())
	if err != nil || !chain.HasRole(registered, "gateway") {
		return true, errors.New("spool signer is not a registered gateway")
	}
	c := h.Pipeline.Spot
	if c == nil {
		return true, errors.New("spot assignment not finalized")
	}
	f, err := q.OpenJob(id)
	if err != nil {
		return true, err
	}
	defer f.Close()
	source, err := jsonspool.Index(f)
	if err != nil {
		return true, err
	}
	receipt := new(pb.PipelineExecutionReceipt)
	if err = proto.Unmarshal(h.Pipeline.Receipt, receipt); err != nil {
		return true, err
	}
	execution := &gateway.PipelineExecution{Source: source, Receipt: receipt}
	m, err := gateway.BuildPipelineSpotCommitment(execution, id, profile)
	if err != nil {
		return true, err
	}
	if m.CommitmentRoot != hex.EncodeToString(h.Pipeline.CommitmentRoot) || m.SpotStepCount != h.Pipeline.SpotStepCount {
		return true, errors.New("durable commitment differs from hold")
	}
	// Height is refreshed after assignment, so no opening predates its challenge.
	_, height, _, err = book.SpotRuntimeState(id)
	if err != nil {
		return true, err
	}
	r, err := store.Prepare(execution, m, c, height)
	if err != nil {
		return true, err
	}
	if store.Delivered(r, c.Auditor) {
		return true, nil
	}
	mode := "normal"
	if c.Round == "esc" {
		mode = "escalation"
	} else if c.Substitute {
		mode = "substitute"
	}
	// Same reader/transfer protocol and stable package bytes across retries.
	packageFile, err := os.CreateTemp("", "ember-spot-*")
	if err != nil {
		return true, err
	}
	defer os.Remove(packageFile.Name())
	defer packageFile.Close()
	if err = gateway.WritePipelineSpotAudit(packageFile, source, r, mode); err != nil {
		return true, err
	}
	if _, err = packageFile.Seek(0, io.SeekStart); err != nil {
		return true, err
	}
	stat, err := packageFile.Stat()
	if err != nil {
		return true, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), auditAttemptBudget(5*time.Minute, stat.Size(), len(c.StepSet)))
	defer cancel()
	var envelope []byte
	err = relay.AuditReader(ctx, c.Auditor, id, fmt.Sprintf("spot-%s-%d", id, c.ChallengeHeight), packageFile, stat.Size(), func(frame *pb.TunnelFrame) error {
		if frame.Kind != "audit_result" {
			return fmt.Errorf("spot audit response %s: %s", frame.Kind, frame.Payload)
		}
		var result struct {
			Envelope string `json:"envelope"`
		}
		if err := json.Unmarshal(frame.Payload, &result); err != nil {
			return err
		}
		var err error
		envelope, err = base64.StdEncoding.DecodeString(result.Envelope)
		return err
	})
	if err != nil {
		return true, err
	}
	if err = book.RecordRemoteAudit(id, envelope); err != nil {
		return true, err
	}
	return true, store.MarkDelivered(r, c.Auditor)
}

// Queries expose only signed spool facts, never execution records or tokens.
func spotQuery(book *ledger.ChainLedger, store *gateway.SpotStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET required", 405)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/spot/"), "/")
		if len(parts) != 2 || book == nil || store == nil {
			http.NotFound(w, r)
			return
		}
		h, height, version, err := book.SpotRuntimeState(parts[0])
		if err != nil || version < 14 || h.State != "dispute" || h.Pipeline == nil || h.Pipeline.Spot == nil {
			http.Error(w, "finalized spot challenge unavailable", 404)
			return
		}
		registered, registrationErr := book.NodeProfile(store.GatewayID())
		if registrationErr != nil || !chain.HasRole(registered, "gateway") {
			http.Error(w, "registered gateway signer unavailable", 503)
			return
		}
		c := h.Pipeline.Spot
		var value any
		switch parts[1] {
		case "receipt":
			value, err = store.Receipt(h.ID, c.ChallengeHeight)
		case "absence":
			value, err = store.Absence(c, height)
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	})
}

func openRuntimeSpotStore(dir string, relay *tunnel.Server, book *ledger.ChainLedger) *gateway.SpotStore {
	if book == nil {
		return nil
	}
	store, err := gateway.OpenSpotStore(dir, relay.SchedulerKey)
	if err != nil {
		log.Printf("SPOT_SPOOL unavailable: %v", err)
		return nil
	}
	return store
}
