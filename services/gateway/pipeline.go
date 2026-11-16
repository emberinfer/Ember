package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	core "ember/core-go"
	pb "ember/proto"
	"ember/services/ledger"
	"ember/services/scheduler"
	"ember/services/verifier"
	"google.golang.org/protobuf/proto"
)

type PipelineConfig struct {
	DynamicRanges    bool
	MinStageLayers   uint32
	AllowedCuts      []uint32
	WitnessKeyFile   string
	Python           string
	WorkDir          string
	ModelPath        string
	SchedulerKeyFile string
	Timeout          time.Duration
}

type PipelineExecution struct {
	Receipt     *pb.PipelineExecutionReceipt
	Invocations []verifier.PipelineInvocation
	Openings    json.RawMessage
	Failovers   json.RawMessage
	Prompt      string
	Seed        int64
	TTFT        int64
}

type pipelineStageInput struct {
	StartLayer uint32   `json:"start_layer"`
	EndLayer   uint32   `json:"end_layer"`
	Profiles   []string `json:"profiles"`
}

type pipelineInput struct {
	TokenizerVersion string               `json:"tokenizer_version,omitempty"`
	WitnessKeyFile   string               `json:"witness_key_file,omitempty"`
	Prompt           string               `json:"prompt"`
	Seed             int64                `json:"seed"`
	MaxTokens        int                  `json:"max_tokens"`
	RequestID        string               `json:"request_id"`
	Stream           bool                 `json:"stream"`
	ModelPath        string               `json:"model_path"`
	SchedulerKeyFile string               `json:"scheduler_key_file"`
	Timeout          float64              `json:"timeout"`
	Stages           []pipelineStageInput `json:"stages"`
}

type pipelineEvent struct {
	StageReceiptRef string          `json:"stage_receipt_ref"`
	Type            string          `json:"type"`
	Text            string          `json:"text"`
	Receipt         string          `json:"receipt"`
	Openings        json.RawMessage `json:"openings"`
	Failovers       json.RawMessage `json:"failovers"`
	Invocations     []struct {
		Request  string  `json:"request"`
		Response string  `json:"response"`
		Profile  string  `json:"profile"`
		StageMS  float64 `json:"stage_ms"`
	} `json:"invocations"`
}

func validatePrivateKeyFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0077 != 0 {
		return errors.New("pipeline scheduler key must have mode 0600")
	}
	if info.Size() != 32 && info.Size() != 64 {
		return errors.New("pipeline scheduler key must be a raw Ed25519 key")
	}
	return nil
}

func (g *Gateway) servePipeline(w http.ResponseWriter, r *http.Request, q request, prompt, account, requestID string) {
	consumerStarted := time.Now()
	if g.TotalLayers == 0 || g.Pipeline.Python == "" || g.Pipeline.ModelPath == "" || g.Pipeline.SchedulerKeyFile == "" {
		http.Error(w, "dynamic pipeline is not configured", http.StatusServiceUnavailable)
		return
	}
	var policy *core.SchedulingPolicy
	if g.SchedulingPolicy != nil {
		var e error
		policy, e = g.SchedulingPolicy()
		if e != nil {
			http.Error(w, "scheduling policy unavailable", 503)
			return
		}
	}
	candidates := g.liveCandidates()
	selectionRequest := scheduler.Request{Chain: q.Model, Manifest: g.Manifest,
		Profile: g.Profile, LatencyPolicy: policy, DynamicRanges: g.Pipeline.DynamicRanges, MinStageLayers: g.Pipeline.MinStageLayers, AllowedCuts: g.Pipeline.AllowedCuts, TieBreak: atomic.AddUint64(&g.selection, 1) - 1, SpeedEnabled: g.SpeedEnabled}
	plan, err := scheduler.PlanPipeline(selectionRequest, candidates, g.TotalLayers)
	if err != nil {
		// Re-read once so a concurrent re-registration can heal this request.
		candidates = g.liveCandidates()
		plan, err = scheduler.PlanPipeline(selectionRequest, candidates, g.TotalLayers)
	}
	if err != nil {
		requestError(w, q.Stream, requestID, err.Error(), http.StatusServiceUnavailable)
		return
	}
	profiles := make(map[string]*pb.ResourceProfile)
	for _, candidate := range candidates {
		if candidate.Profile != nil {
			profiles[candidate.Profile.NodeId] = candidate.Profile
		}
	}
	if err := validatePrivateKeyFile(g.Pipeline.SchedulerKeyFile); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	input := pipelineInput{Prompt: prompt, Seed: q.Seed, MaxTokens: q.MaxTokens,
		RequestID: requestID, Stream: q.Stream, ModelPath: g.Pipeline.ModelPath,
		WitnessKeyFile: g.Pipeline.WitnessKeyFile, SchedulerKeyFile: g.Pipeline.SchedulerKeyFile, Timeout: g.Pipeline.Timeout.Seconds()}
	if input.Timeout <= 0 {
		input.Timeout = 300
	}
	var authorizations []ledger.StageAuthorization
	for stageIndex, stage := range plan.Stages {
		chosen := scheduler.SelectStage(selectionRequest, stage.Nodes, stageIndex)
		ordered := append([]scheduler.Ranked{chosen}, stage.Nodes...)
		stage.Nodes = nil
		for i, n := range ordered {
			if i == 0 || n.NodeID != chosen.NodeID {
				stage.Nodes = append(stage.Nodes, n)
			}
		}
		value := pipelineStageInput{StartLayer: stage.StartLayer, EndLayer: stage.EndLayer}
		for _, node := range stage.Nodes {
			profile := profiles[node.NodeID]
			if profile == nil {
				http.Error(w, "selected shard profile disappeared", http.StatusServiceUnavailable)
				return
			}
			raw, marshalErr := proto.MarshalOptions{Deterministic: true}.Marshal(profile)
			if marshalErr != nil {
				http.Error(w, marshalErr.Error(), http.StatusInternalServerError)
				return
			}
			value.Profiles = append(value.Profiles, hex.EncodeToString(raw))
			authorizations = append(authorizations, ledger.StageAuthorization{NodeID: node.NodeID,
				StartLayer: stage.StartLayer, EndLayer: stage.EndLayer})
		}
		input.Stages = append(input.Stages, value)
	}
	holdID := requestID
	var commitment []byte
	var inputTokens uint32
	commitment, inputTokens, err = g.tokenize(r.Context(), q, prompt, holdID, "")
	if err != nil {
		http.Error(w, err.Error(), 503)
		return
	}
	pinned := &pb.ExecutionCommitment{}
	_ = proto.Unmarshal(commitment, pinned)
	input.TokenizerVersion = pinned.TokenizerVersion
	release, ok := g.admit(w)
	if !ok {
		return
	}
	defer release()
	bound, ppm, priceHeight, err := g.quoteFee(inputTokens, uint32(q.MaxTokens))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err = g.pipelineAvailable(plan, selectionRequest, candidates); err != nil {
		requestError(w, q.Stream, requestID, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if err = g.pipelineAvailable(plan, selectionRequest, g.liveCandidates()); err != nil {
		requestError(w, q.Stream, requestID, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if g.priced() {
		if b, ok := g.Ledger.(ledger.QuotedFreezer); ok {
			err = b.FreezePipelineQuoted(holdID, account, bound.Total, authorizations, g.TotalLayers, commitment, ppm, priceHeight)
		} else if b, ok := g.Ledger.(ledger.PricedBackend); ok {
			err = b.FreezePipelinePriced(holdID, account, bound.Total, authorizations, g.TotalLayers, commitment)
		} else {
			err = errors.New("token pricing requires consensus backend")
		}
	} else {
		err = g.Ledger.FreezePipeline(holdID, account, bound.Total, authorizations, g.TotalLayers)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusPaymentRequired)
		return
	}
	// Freeze can cross a lease expiry or disconnect. Recheck live tunnels and
	// pinned leases, not fresh
	// scheduling capacity: this hold itself may have consumed the node's cap.
	if err = g.pipelineAvailable(plan, selectionRequest, candidates); err != nil {
		err = g.failPipeline(w, q.Stream, holdID, err, http.StatusServiceUnavailable)
		log.Printf("PIPELINE FAILED id=%s reason=%v", requestID, err)
		return
	}
	if len(commitment) > 0 {
		c := &pb.ExecutionCommitment{}
		_ = proto.Unmarshal(commitment, c)
		input.TokenizerVersion = c.TokenizerVersion
	}
	rawInput, _ := json.Marshal(input)
	timeout := g.RequestTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	var stderr bytes.Buffer
	stdout, waitPipeline, err := startPipeline(ctx, *g.Pipeline, rawInput, &stderr)
	if err != nil {
		err = g.failPipeline(w, q.Stream, holdID, err, http.StatusBadGateway)
		log.Printf("PIPELINE FAILED id=%s reason=%v", requestID, err)
		return
	}
	started := time.Now()
	ttft := int64(-1)
	var output strings.Builder
	var result *pipelineEvent
	streamed := false
	tokenCount := 0
	var stageStarted time.Time
	var consumerStages []ledger.ConsumerStageObservation
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 128*1024*1024)
	for scanner.Scan() {
		var event pipelineEvent
		if err = json.Unmarshal(scanner.Bytes(), &event); err != nil {
			break
		}
		switch event.Type {
		case "stage_start":
			if !stageStarted.IsZero() {
				err = errors.New("overlapping consumer stage observations")
				break
			}
			stageStarted = time.Now()
		case "stage_failed":
			stageStarted = time.Time{}
		case "stage_end":
			if stageStarted.IsZero() {
				err = errors.New("consumer stage observation has no start")
				break
			}
			consumerStages = append(consumerStages, ledger.ConsumerStageObservation{StageReceiptRef: event.StageReceiptRef, ObservedNS: uint64(time.Since(stageStarted).Nanoseconds())})
			stageStarted = time.Time{}
		case "token":
			if ttft < 0 {
				ttft = time.Since(started).Milliseconds()
			}
			tokenCount++
			output.WriteString(event.Text)
			if q.Stream {
				if !streamed {
					w.Header().Set("Content-Type", "text/event-stream")
					w.Header().Set("Cache-Control", "no-cache")
					streamed = true
				}
				raw, _ := json.Marshal(map[string]any{"id": requestID, "object": "chat.completion.chunk",
					"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": event.Text}, "finish_reason": nil}}})
				fmt.Fprintf(w, "data: %s\n\n", raw)
				if flush, ok := w.(http.Flusher); ok {
					flush.Flush()
				}
			}
		case "result":
			copy := event
			result = &copy
		default:
			err = fmt.Errorf("unexpected pipeline event %q", event.Type)
		}
		if err != nil {
			break
		}
	}
	if scanErr := scanner.Err(); err == nil {
		err = scanErr
	}
	waitErr := waitPipeline()
	if err == nil {
		err = waitErr
		if err != nil {
			reason := strings.TrimSpace(stderr.String())
			if len(reason) > 2048 {
				reason = reason[len(reason)-2048:]
			}
			if reason != "" {
				err = fmt.Errorf("%w: %s", err, reason)
			}
		}
	}
	if err == nil && result == nil {
		err = errors.New("pipeline client returned no receipt")
	}
	consumerElapsed := time.Since(consumerStarted)
	var execution PipelineExecution
	if err == nil {
		receiptBytes, decodeErr := hex.DecodeString(result.Receipt)
		if decodeErr != nil {
			err = decodeErr
		} else {
			execution.Receipt = &pb.PipelineExecutionReceipt{}
			err = proto.Unmarshal(receiptBytes, execution.Receipt)
		}
		execution.Openings, execution.Failovers = result.Openings, result.Failovers
		execution.Prompt, execution.Seed, execution.TTFT = prompt, q.Seed, ttft
		for _, invocation := range result.Invocations {
			requestWire, requestErr := hex.DecodeString(invocation.Request)
			responseWire, responseErr := hex.DecodeString(invocation.Response)
			profileWire, profileErr := hex.DecodeString(invocation.Profile)
			if requestErr != nil || responseErr != nil || profileErr != nil {
				err = errors.New("invalid pipeline invocation encoding")
				break
			}
			execution.Invocations = append(execution.Invocations, verifier.PipelineInvocation{
				Request: requestWire, Response: responseWire, Profile: profileWire, StageMS: invocation.StageMS})
		}
	}
	if err == nil && (execution.Receipt.RequestId != holdID || int(execution.Receipt.OutputTokens) != tokenCount || tokenCount > q.MaxTokens) {
		err = errors.New("pipeline receipt or token count mismatch")
	}
	if err == nil && g.priced() && execution.Receipt.InputTokens != inputTokens {
		err = errors.New("pipeline input token count mismatch")
	}
	if err == nil {
		_, err = verifier.VerifyPipelineExecution(execution.Receipt, execution.Invocations,
			q.Model, g.Manifest, g.Profile, g.TotalLayers)
	}
	if err == nil && g.BeforePipelineSubmit != nil {
		err = g.BeforePipelineSubmit(&execution, prompt, q.Seed)
	}
	if err == nil {
		charge, feeErr := g.settlementFee(holdID, inputTokens, execution.Receipt.OutputTokens)
		err = feeErr
		if err == nil {
			_, err = g.Ledger.SubmitPipeline(holdID, execution.Receipt, charge.Total, g.TotalLayers, g.Dispute)
		}
	}
	if observer, ok := g.Ledger.(ledger.ConsumerLatencyRecorder); err == nil && ok {
		var ref []byte
		ref, err = core.PipelineReceiptRef(execution.Receipt)
		if err == nil {
			err = observer.ObserveLatency(holdID, ref, consumerElapsed, consumerStages)
		}
	}
	if err == nil {
		g.recordPipelineSpeed(&execution, time.Now())
	}
	if err == nil && g.OnPipelineReceipt != nil {
		g.OnPipelineReceipt(&execution)
	}
	if err != nil {
		atomic.AddUint64(&g.failed, 1)
		status := http.StatusBadGateway
		if streamed {
			status = http.StatusOK
		}
		err = g.failPipeline(w, q.Stream, holdID, fmt.Errorf("pipeline failed: %w", err), status)
		log.Printf("PIPELINE FAILED id=%s reason=%v stderr=%s", requestID, err, strings.TrimSpace(stderr.String()))
		return
	}
	atomic.AddUint64(&g.succeeded, 1)
	log.Printf("PIPELINE REQUEST id=%s tokens=%d failovers=%s", requestID, tokenCount, execution.Failovers)
	if q.Stream {
		fmt.Fprint(w, "data: [DONE]\n\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": requestID, "object": "chat.completion", "model": q.Model,
		"choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": output.String()}, "finish_reason": "stop"}},
		"usage":   map[string]int{"completion_tokens": tokenCount}})
}

// StageMS is measured by the gateway pipeline subprocess around each forward RPC.
func (g *Gateway) recordPipelineSpeed(e *PipelineExecution, now time.Time) {
	if g.SpeedStats == nil {
		return
	}
	type key struct{ node, cohort string }
	type aggregate struct {
		tokens   int
		duration time.Duration
	}
	samples := map[key]aggregate{}
	for _, inv := range e.Invocations {
		p := &pb.ResourceProfile{}
		if proto.Unmarshal(inv.Profile, p) != nil || inv.StageMS <= 0 {
			continue
		}
		k := key{p.NodeId, CohortKey(p.ExecutionProfileVer, true, p.ShardStartLayer, p.ShardEndLayer)}
		v := samples[k]
		v.tokens++
		v.duration += time.Duration(inv.StageMS * float64(time.Millisecond))
		samples[k] = v
	}
	for k, v := range samples {
		g.SpeedStats.Record(k.node, k.cohort, v.tokens, v.duration, now)
	}
}
