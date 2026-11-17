package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	core "ember/core-go"
	pb "ember/proto"
	"ember/services/evidence"
	"ember/services/ledger"
	"ember/services/scheduler"
	"ember/services/tunnel"
	"google.golang.org/protobuf/proto"
)

type Gateway struct {
	execExitedDone       chan struct{}
	ChainExecExited      func() (map[string]bool, error)
	execExitedMu         sync.Mutex
	execExitedSet        map[string]bool
	execExitedAt         time.Time
	execExitedRefreshing bool
	SpeedStats           *SpeedStats
	ExecExclude          map[string]bool
	SpeedEnabled         bool
	Privacy              http.Handler
	PriceInput           int64
	PriceContext         int64
	ChainID              string
	SchedulingPolicy     func() (*core.SchedulingPolicy, error)
	CheckModelTrust      func(string) error
	Ledger               ledger.Backend
	Tunnel               *tunnel.Server
	Candidates           func() []scheduler.Candidate
	Manifest             []byte
	Profile              string
	Price                int64
	Dispute              time.Duration
	OnReceipt            func(*pb.ExecutionReceipt, []byte, string, int64, int64)
	BeforeSubmit         func(*pb.ExecutionReceipt, []byte, string, int64, int64) error
	DemoBootstrap        bool
	Admission            chan struct{}
	RequestTimeout       time.Duration
	MaxBodyBytes         int64
	Moe                  *MoeConfig
	Pipeline             *PipelineConfig
	TotalLayers          uint32
	BeforePipelineSubmit func(*PipelineExecution, string, int64) error
	OnPipelineReceipt    func(*PipelineExecution)
	selection            uint64
	total                uint64
	rejected             uint64
	succeeded            uint64
	failed               uint64
	active               int64
	RateLimit            int
	RateWindow           time.Duration
	rateMu               sync.Mutex
	rates                map[string]rateEntry
}

type rateEntry struct {
	window time.Time
	count  int
}
type job struct {
	ThinkingEnabled        *bool  `json:"thinking_enabled,omitempty"`
	ThinkingBudget         uint32 `json:"thinking_budget,omitempty"`
	AnswerBudget           uint32 `json:"answer_budget,omitempty"`
	TokenizerVersion       string `json:"tokenizer_version,omitempty"`
	EvidenceMaxFrameTokens uint32 `json:"evidence_max_frame_tokens,omitempty"`
	Pricing                bool   `json:"pricing,omitempty"`
	Prompt                 string `json:"prompt"`
	MaxTokens              int    `json:"max_tokens"`
	Seed                   int64  `json:"seed"`
	Chain                  string `json:"chain"`
	Profile                string `json:"profile"`
}
type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type request struct {
	ThinkingEnabled *bool     `json:"thinking_enabled,omitempty"`
	ThinkingBudget  uint32    `json:"thinking_budget,omitempty"`
	AnswerBudget    uint32    `json:"answer_budget,omitempty"`
	Tier            string    `json:"tier,omitempty"`
	Model           string    `json:"model"`
	Messages        []message `json:"messages"`
	MaxTokens       int       `json:"max_tokens"`
	Stream          bool      `json:"stream"`
	Seed            int64     `json:"seed"`
	User            string    `json:"user"`
}

func id() string { b := make([]byte, 16); _, _ = rand.Read(b); return hex.EncodeToString(b) }
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v2/requests" {
		if g.Privacy == nil {
			http.Error(w, "privacy endpoint unavailable", 503)
			return
		}
		g.Privacy.ServeHTTP(w, r)
		return
	}
	consumerStarted := time.Now()
	if r.URL.Path == "/v1/pricing" {
		g.servePricing(w, r)
		return
	}
	if r.URL.Path != "/v1/chat/completions" && r.URL.Path != "/v1/quote" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", 405)
		return
	}
	atomic.AddUint64(&g.total, 1)
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		http.Error(w, "Bearer token required", 401)
		return
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	account, authErr := g.Ledger.TokenAccount(token)
	if authErr != nil {
		if !g.DemoBootstrap || token != "consumer" {
			http.Error(w, "invalid bearer token", 401)
			return
		}
		account = "consumer"
	}
	if !g.allowIdentity(account, time.Now()) {
		atomic.AddUint64(&g.rejected, 1)
		w.Header().Set("Retry-After", "60")
		http.Error(w, "identity rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	var q request
	maxBody := g.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = 1 << 20
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&q) != nil || len(q.Messages) == 0 || q.MaxTokens < 1 || q.MaxTokens > 4096 || !strings.HasPrefix(q.Model, "ember-") {
		http.Error(w, "invalid request", 400)
		return
	}
	if q.Tier == "P1" {
		http.Error(w, "TIER_UNAVAILABLE", 503)
		return
	}
	if q.Tier != "" && q.Tier != "P0" {
		http.Error(w, "invalid tier", 400)
		return
	}
	q.Tier = "P0"
	if g.CheckModelTrust != nil {
		if err := g.CheckModelTrust(q.Model); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	if !validReasoning(q) || (q.ThinkingEnabled != nil && g.Pipeline != nil) {
		http.Error(w, "invalid reasoning mode or budgets", http.StatusBadRequest)
		return
	}
	if g.Price <= 0 || int64(q.MaxTokens) > (1<<62)/g.Price {
		http.Error(w, "invalid price", 500)
		return
	}
	prompt, err := promptFor(q.Model, q.Messages)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if g.ChainID != "" && q.Model != g.ChainID {
		http.Error(w, "unknown pricing profile", 400)
		return
	}
	if r.URL.Path == "/v1/quote" {
		g.serveQuote(w, r, q, prompt)
		return
	}
	// Fast rejection never acquires a slot or invokes unavailable routing.
	if g.Admission != nil && len(g.Admission) >= cap(g.Admission) {
		atomic.AddUint64(&g.rejected, 1)
		w.Header().Set("Retry-After", "1")
		http.Error(w, "inference capacity exhausted", http.StatusTooManyRequests)
		return
	}
	requestID := id()
	if g.Moe != nil {
		g.serveMoe(w, r, q, prompt, account, requestID)
		return
	}
	if g.Pipeline != nil {
		g.servePipeline(w, r, q, prompt, account, requestID)
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
	selection := atomic.AddUint64(&g.selection, 1) - 1
	excluded := map[string]bool{}
	// Fallback resend state (coordinator ruling, 2026-11-15): a receipt
	// marked thinking-truncated (whole budget consumed, no answer text)
	// triggers exactly one independent resend with thinking off and the
	// same total budget. The resend is a separate committed request —
	// own hold, commitment, settlement and receipt — never a merge and
	// never a loop: resent latches after the first trigger.
	resent := false
	firstID := ""
	tries := 0
	for tries < 2 {
		tries++
		selectionRequest := scheduler.Request{Chain: q.Model, Manifest: g.Manifest, Profile: g.Profile, Exclude: excluded, TieBreak: selection, LatencyPolicy: policy, SpeedEnabled: g.SpeedEnabled}
		chosen, err := scheduler.Choose(selectionRequest, g.liveCandidates())
		if err != nil {
			chosen, err = scheduler.Choose(selectionRequest, g.liveCandidates())
		}
		if err != nil {
			requestError(w, q.Stream, requestID, err.Error(), 503)
			return
		}
		excluded[chosen.NodeID] = true
		attempt := id()
		holdID := requestID + "-" + attempt
		var commitment []byte
		var inputTokens uint32
		commitment, inputTokens, err = g.tokenize(r.Context(), q, prompt, holdID, chosen.NodeID)
		if err != nil {
			http.Error(w, err.Error(), 503)
			return
		}
		pinned := &pb.ExecutionCommitment{}
		_ = proto.Unmarshal(commitment, pinned)
		// Historical output-only encodings retain their empty commitment.
		if !g.priced() && q.ThinkingEnabled == nil {
			commitment = nil
		}
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
		if err := g.freeze(holdID, account, chosen.NodeID, bound.Total, commitment, ppm, priceHeight); err != nil {
			if strings.Contains(err.Error(), "contribution unavailable") {
				continue
			}
			http.Error(w, err.Error(), 402)
			return
		}
		limit, limitErr := g.Tunnel.FrameLimit()
		if limitErr != nil || limit == 0 {
			reason := "evidence parameter unavailable"
			if cancelErr := g.Ledger.Cancel(holdID); cancelErr != nil {
				reason += "; freeze release failed: " + cancelErr.Error()
				log.Printf("CANCEL FAILED id=%s reason=%s", holdID, reason)
			}
			requestError(w, q.Stream, requestID, reason, 503)
			return
		}
		payload, _ := json.Marshal(job{ThinkingEnabled: q.ThinkingEnabled, ThinkingBudget: q.ThinkingBudget, AnswerBudget: q.AnswerBudget, TokenizerVersion: pinned.TokenizerVersion, EvidenceMaxFrameTokens: limit, Pricing: g.priced(), Prompt: prompt, MaxTokens: q.MaxTokens, Seed: q.Seed, Chain: q.Model, Profile: g.Profile})
		var output strings.Builder
		var receipt *pb.ExecutionReceipt
		var evidenceBytes []byte
		var received []*pb.EvidenceChunk
		var receivedHashes [][]byte
		var firstTokenFrame, lastGenerationFrame time.Time
		streamed := false
		tokenCount := 0
		traceForward := os.Getenv("EMBER_FORWARD_TRACE") == "1"
		var traceFirst, traceLast time.Time
		var traceWrite time.Duration
		ttft := int64(-1)
		timeout := g.RequestTimeout
		if timeout <= 0 {
			timeout = 2 * time.Minute
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		runStart := time.Now()
		err = g.Tunnel.Run(ctx, chosen.NodeID, holdID, attempt, payload, func(f *pb.TunnelFrame) error {
			if !firstTokenFrame.IsZero() {
				lastGenerationFrame = time.Now()
			}
			switch f.Kind {
			case "token":
				if firstTokenFrame.IsZero() {
					firstTokenFrame = time.Now()
				}
				lastGenerationFrame = time.Now()
				var traceToken time.Time
				if traceForward {
					traceToken = time.Now()
					if traceFirst.IsZero() {
						traceFirst = traceToken
					}
					traceLast = traceToken
				}
				tokenCount++
				output.Write(f.Payload)
				if q.Stream {
					if !streamed {
						w.Header().Set("Content-Type", "text/event-stream")
						w.Header().Set("Cache-Control", "no-cache")
						streamed = true
					}
					raw, _ := json.Marshal(map[string]any{"id": requestID, "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": string(f.Payload)}, "finish_reason": nil}}})
					fmt.Fprintf(w, "data: %s\n\n", raw)
					if flush, ok := w.(http.Flusher); ok {
						flush.Flush()
					}
				}
				if traceForward {
					traceWrite += time.Since(traceToken)
				}
			case "receipt":
				receipt = &pb.ExecutionReceipt{}
				if err := proto.Unmarshal(f.Payload, receipt); err != nil {
					return err
				}
			case "evidence":
				if evidenceBytes != nil || len(received) > 0 {
					return errors.New("duplicate evidence")
				}
				var tokens []json.RawMessage
				if len(f.Payload) > evidence.MaxFrameBytes || json.Unmarshal(f.Payload, &tokens) != nil || uint64(len(tokens)) > uint64(limit) {
					return errors.New("evidence exceeds frame parameter")
				}
				evidenceBytes = append([]byte(nil), f.Payload...)
			case "evidence_chunk":
				chunk := &pb.EvidenceChunk{}
				if evidenceBytes != nil || proto.Unmarshal(f.Payload, chunk) != nil || chunk.Index != uint32(len(received)) || len(chunk.ManifestRoot) != 32 || g.Tunnel.Evidence == nil {
					return errors.New("invalid evidence chunk sequence")
				}
				var tokens []json.RawMessage
				if len(chunk.Data) > evidence.MaxFrameBytes || json.Unmarshal(chunk.Data, &tokens) != nil || len(tokens) == 0 || uint64(len(tokens)) > uint64(limit) || len(received) >= q.MaxTokens {
					return errors.New("invalid evidence frame size")
				}
				h, err := g.Tunnel.Evidence.Put(chunk.Data)
				if err != nil {
					return err
				}
				receivedHashes = append(receivedHashes, h)
				// Keep only transport metadata; the bytes are durable off chain.
				chunk.Data = nil
				received = append(received, chunk)
			case "timing":
				ttft, _ = strconv.ParseInt(string(f.Payload), 10, 64)
			case "unavailable":
				return errors.New("contribution unavailable: " + string(f.Payload))
			case "error":
				return errors.New(string(f.Payload))
			default:
				return fmt.Errorf("unexpected frame %q", f.Kind)
			}
			return nil
		})
		cancel()
		if traceForward {
			log.Printf("FORWARD_TRACE hop=gateway request=%s attempt=%s tokens=%d first_unix_ns=%d last_unix_ns=%d elapsed_ms=%.3f ingress_ms_per_token=%.3f write_flush_ms_per_token=%.3f error=%v", requestID, attempt, tokenCount, traceFirst.UnixNano(), traceLast.UnixNano(), float64(time.Since(consumerStarted))/float64(time.Millisecond), float64(traceLast.Sub(traceFirst))/float64(time.Millisecond)/float64(max(1, tokenCount-1)), float64(traceWrite)/float64(time.Millisecond)/float64(max(1, tokenCount)), err)
		}
		consumerElapsed := time.Since(consumerStarted)
		if err == nil && receipt != nil && receipt.RequestId == holdID && receipt.NodeId == chosen.NodeID && (len(chosen.Signer) == 0 || string(chosen.Signer) == string(receipt.Signer)) && int(receipt.OutputTokens) == tokenCount && tokenCount <= q.MaxTokens {
			if receipt.EvidenceManifest != nil {
				m := receipt.EvidenceManifest
				err = evidence.Validate(m, receipt.OutputTokens, limit)
				if err == nil && (evidenceBytes != nil || len(received) != int(m.ChunkCount)) {
					err = errors.New("missing evidence chunk")
				}
				for i, chunk := range received {
					if err != nil {
						break
					}
					if !bytes.Equal(chunk.ManifestRoot, m.MerkleRoot) || !bytes.Equal(receivedHashes[i], m.ChunkHashes[i]) {
						err = errors.New("evidence chunk root or hash mismatch")
						break
					}
					_, err = g.Tunnel.Evidence.Fetch(m, uint32(i), limit)
				}
				if err == nil {
					_, err = g.Tunnel.Evidence.Assemble(m, limit)
				}
			} else if len(received) > 0 || receipt.OutputTokens > limit {
				err = errors.New("missing evidence manifest")
			}
			if g.priced() && receipt.InputTokens != inputTokens {
				err = errors.New("input token count mismatch")
			}
			if err == nil && g.BeforeSubmit != nil {
				err = g.BeforeSubmit(receipt, evidenceBytes, prompt, q.Seed, ttft)
			}
			if err == nil {
				charge, feeErr := g.settlementFee(holdID, inputTokens, receipt.OutputTokens)
				err = feeErr
				if err == nil {
					err = g.Ledger.Submit(holdID, receipt, charge.Total, g.Dispute)
				}
			}
			if observer, ok := g.Ledger.(ledger.ConsumerLatencyRecorder); err == nil && ok {
				var ref []byte
				ref, err = core.Digest(receipt)
				if err == nil {
					err = observer.ObserveLatency(holdID, ref, consumerElapsed, nil)
				}
			}
			if err == nil && g.SpeedStats != nil && !firstTokenFrame.IsZero() {
				g.SpeedStats.Record(chosen.NodeID, CohortKey(g.Profile, false, 0, 0), int(receipt.OutputTokens), lastGenerationFrame.Sub(firstTokenFrame), time.Now())
				g.SpeedStats.RecordTTFT(chosen.NodeID, CohortKey(g.Profile, false, 0, 0), int(receipt.OutputTokens), firstTokenFrame.Sub(runStart), time.Now())
			}
			if err == nil && g.OnReceipt != nil {
				g.OnReceipt(receipt, evidenceBytes, prompt, q.Seed, ttft)
			}
		} else if err == nil {
			err = errors.New("receipt or gateway token count mismatch")
		}
		if err == nil && receipt == nil {
			err = errors.New("missing receipt")
		}
		if err != nil {
			atomic.AddUint64(&g.failed, 1)
			log.Printf("ATTEMPT FAILED id=%s node=%s reason=%v", requestID, chosen.NodeID, err)
			if cancelErr := g.Ledger.Cancel(holdID); cancelErr != nil {
				err = fmt.Errorf("%v; freeze release failed: %w", err, cancelErr)
				log.Printf("CANCEL FAILED id=%s reason=%v", holdID, err)
				status := http.StatusBadGateway
				if streamed {
					status = http.StatusOK
				}
				requestError(w, q.Stream, requestID, err.Error(), status)
				return
			}
			if streamed {
				requestError(w, true, requestID, err.Error(), http.StatusOK)
				return
			}
			release()
			continue
		}
		log.Printf("REQUEST id=%s node=%s attempt=%s tokens=%d", requestID, chosen.NodeID, attempt, tokenCount)
		atomic.AddUint64(&g.succeeded, 1)
		if receipt.OutputCategory == "thinking-truncated" && !resent && !q.Stream {
			// The truncated request above has already settled on its
			// own hold and stays visible with its marker; dispatch the
			// single fallback as a fresh request. Streaming requests
			// are exempt: their frames are already on the wire.
			resent = true
			firstID = requestID
			requestID = id()
			off := false
			q.ThinkingEnabled = &off
			q.ThinkingBudget = 0
			q.AnswerBudget = uint32(q.MaxTokens)
			excluded = map[string]bool{}
			tries = 0
			log.Printf("RESEND new_id=%s first_id=%s reason=thinking-truncated mode=thinking-off answer_budget=%d", requestID, firstID, q.AnswerBudget)
			continue
		}
		if q.Stream {
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		finishReason := "stop"
		if receipt.OutputCategory == "thinking-truncated" {
			finishReason = "length"
		}
		resp := map[string]any{"id": requestID, "object": "chat.completion", "model": q.Model, "choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": output.String()}, "finish_reason": finishReason}}, "usage": map[string]any{"completion_tokens": int(receipt.OutputTokens), "reasoning_tokens": int(receipt.ReasoningTokens), "content_tokens": int(receipt.ContentTokens), "prompt_tokens": int(receipt.InputTokens), "total_tokens": int(receipt.InputTokens) + int(receipt.OutputTokens), "completion_tokens_details": map[string]int{"reasoning_tokens": int(receipt.ReasoningTokens)}}}
		if resent {
			resp["ember_fallback_resend"] = true
			resp["ember_first_request_id"] = firstID
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	requestError(w, q.Stream, requestID, "all nodes failed", 502)
}

func (g *Gateway) allowIdentity(account string, now time.Time) bool {
	if g.RateLimit <= 0 {
		return true
	}
	window := g.RateWindow
	if window <= 0 {
		window = time.Minute
	}
	g.rateMu.Lock()
	defer g.rateMu.Unlock()
	if g.rates == nil {
		g.rates = map[string]rateEntry{}
	}
	entry := g.rates[account]
	if entry.window.IsZero() || now.Sub(entry.window) >= window {
		entry = rateEntry{window: now}
	}
	if entry.count >= g.RateLimit {
		return false
	}
	entry.count++
	g.rates[account] = entry
	return true
}

func (g *Gateway) Metrics() string {
	metrics := fmt.Sprintf("ember_gateway_requests_total %d\nember_gateway_requests_succeeded_total %d\nember_gateway_attempts_failed_total %d\nember_gateway_requests_rejected_total %d\nember_gateway_active_requests %d\n",
		atomic.LoadUint64(&g.total), atomic.LoadUint64(&g.succeeded), atomic.LoadUint64(&g.failed),
		atomic.LoadUint64(&g.rejected), atomic.LoadInt64(&g.active))
	for _, v := range g.SpeedStats.Snapshot(time.Now()) {
		marker := ""
		if v.Degraded {
			marker = " # DEGRADED"
		}
		metrics += fmt.Sprintf("ember_gateway_speed_score{cohort=%q,node=%q} %g%s\n", v.Cohort, v.NodeID, v.Score, marker)
	}
	return metrics
}

func promptFor(model string, messages []message) (string, error) {
	var prompt strings.Builder
	// ChatML (<|im_start|>) rendering must cover every served profile
	// whose model ships a ChatML chat template. The Qwen3.6-35B
	// profiles were missing here and fell into the plain-transcript
	// fallback below: the engine then receives "user: ..." /
	// "assistant: ..." lines through the completions endpoint and
	// continues the transcript as a document (fabricated extra turns,
	// inline <think> blocks, no reliable stop) instead of answering
	// the last message (production chat bug, 2026-11-16). The
	// fallback stays for mock/test profiles, whose tests pin its
	// exact shape.
	isQwen := strings.HasPrefix(model, "ember-qwen2.5-1.5b") || strings.HasPrefix(model, "ember-qwen3.6-35b")
	isSmolLM := strings.HasPrefix(model, "ember-smollm2-360m")
	if isQwen && messages[0].Role != "system" {
		prompt.WriteString("<|im_start|>system\nYou are Qwen, created by Alibaba Cloud. You are a helpful assistant.<|im_end|>\n")
	}
	if isSmolLM && messages[0].Role != "system" {
		prompt.WriteString("<|im_start|>system\nYou are a helpful AI assistant named SmolLM, trained by Hugging Face<|im_end|>\n")
	}
	for _, m := range messages {
		if m.Role != "system" && m.Role != "user" && m.Role != "assistant" {
			return "", fmt.Errorf("unsupported message role %q", m.Role)
		}
		if isQwen || isSmolLM {
			prompt.WriteString("<|im_start|>")
			prompt.WriteString(m.Role)
			prompt.WriteByte('\n')
			prompt.WriteString(m.Content)
			prompt.WriteString("<|im_end|>\n")
		} else {
			prompt.WriteString(m.Role)
			prompt.WriteString(": ")
			prompt.WriteString(m.Content)
			prompt.WriteByte('\n')
		}
	}
	if isQwen || isSmolLM {
		prompt.WriteString("<|im_start|>assistant\n")
	}
	return prompt.String(), nil
}

// Tokenization and quotes never hold an inference admission slot.
func (g *Gateway) admit(w http.ResponseWriter) (func(), bool) {
	if g.Admission == nil {
		return func() {}, true
	}
	select {
	case g.Admission <- struct{}{}:
		atomic.AddInt64(&g.active, 1)
		var once sync.Once
		return func() { once.Do(func() { <-g.Admission; atomic.AddInt64(&g.active, -1) }) }, true
	default:
		atomic.AddUint64(&g.rejected, 1)
		w.Header().Set("Retry-After", "1")
		http.Error(w, "inference capacity exhausted", http.StatusTooManyRequests)
		return nil, false
	}
}
