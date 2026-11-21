package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	core "ember/core-go"
	pb "ember/proto"
	chainapp "ember/services/chain"
	"ember/services/ledger"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"google.golang.org/protobuf/proto"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const MoeProfile = "ember-olmoe-1b-7b-moe-v1"

type MoeConfig struct {
	// ReceiptV2 compacts transport only; signatures and roots stay logical v1.
	ReceiptV2                                                          bool
	Python, WorkDir, Weights, FleetDir, EvidenceRoot, SchedulerKeyFile string
	// Owned here to keep queue state entirely in the MoE path. Key by Gateway
	// so even gateways sharing a configuration have independent fleet queues.
	queueMu sync.Mutex
	queues  map[*Gateway]*moeQueue
}
type moeLedger interface {
	NodeProfiles() ([]*pb.ResourceProfile, error)
	FreezeMoe(string, string, int64, []ledger.MoeAuthorization, uint32, uint32, []byte) error
	SubmitMoe(string, *pb.PipelineExecutionReceipt, int64) ([]ledger.StageShare, error)
	CancelMoe(string) error
	FinishMoe(string, string, time.Time, int64) error
	State(string) (string, error)
	ResolveAudit(string) (bool, error)
	RecordedVerdict(string) (*pb.AuditVerdict, error)
}

func (g *Gateway) tokenizeMoe(ctx context.Context, q request, prompt, requestID string) ([]byte, uint32, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, g.Moe.Python, "-m", "tokenize_prompt", g.Moe.Weights)
	c.Dir = g.Moe.WorkDir
	c.Env = append(os.Environ(), "PYTHONPATH=core-py:proto:node")
	c.Stdin = strings.NewReader(prompt)
	raw, err := c.Output()
	if err != nil {
		return nil, 0, err
	}
	var v struct {
		Input   uint32 `json:"input_tokens"`
		Version string `json:"tokenizer_version"`
	}
	if json.Unmarshal(raw, &v) != nil || v.Input == 0 || v.Version == "" {
		return nil, 0, errors.New("invalid MoE tokenization")
	}
	key, err := os.ReadFile(g.Moe.SchedulerKeyFile)
	if err != nil {
		return nil, 0, err
	}
	if len(key) == ed25519.SeedSize {
		key = ed25519.NewKeyFromSeed(key)
	}
	hash := sha256.Sum256([]byte(prompt))
	sampling := requestSamplingHash(q)
	commitment := &pb.ExecutionCommitment{RequestId: requestID, ChainId: q.Model, ProfileVer: g.Profile, InputCommitment: hash[:], SamplingParamsHash: sampling[:], Seed: q.Seed, InputTokens: v.Input, TokenizerVersion: v.Version, MaxOutputTokens: uint32(q.MaxTokens)}
	setReasoningCommitment(commitment, q)
	if err := core.Sign(commitment, ed25519.PrivateKey(key)); err != nil {
		return nil, 0, err
	}
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(commitment)
	return wire, v.Input, err
}
func (g *Gateway) moeAuthorizations(b moeLedger) ([]ledger.MoeAuthorization, error) {
	// Pin exactly the fleet used by the executor, never authorize unrelated nodes.
	raw, err := os.ReadFile(filepath.Join(g.Moe.FleetDir, "fleet.json"))
	if err != nil {
		return nil, err
	}
	var fleet map[string]struct {
		NodeID string `json:"node_id"`
	}
	if err = json.Unmarshal(raw, &fleet); err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	for _, n := range []string{"stage-a", "stage-b", "exp-even-1", "exp-odd-1", "exp-even-2", "exp-odd-2"} {
		v, ok := fleet[n]
		if !ok || v.NodeID == "" {
			return nil, errors.New("incomplete MoE fleet")
		}
		wanted[v.NodeID] = true
	}
	profiles, err := b.NodeProfiles()
	if err != nil {
		return nil, err
	}
	var auth []ledger.MoeAuthorization
	layers := make([]int, 16)
	units := map[[2]uint32]bool{}
	for _, p := range profiles {
		if !wanted[p.NodeId] || p.ChainId != g.ChainID {
			continue
		}
		a := ledger.MoeAuthorization{NodeID: p.NodeId, Kind: p.MoeUnitKind}
		switch a.Kind {
		case "attention":
			a.StartLayer = p.ShardStartLayer
			a.EndLayer = p.ShardEndLayer
			if a.StartLayer >= a.EndLayer || a.EndLayer > 16 {
				return nil, errors.New("invalid attention range")
			}
			for i := a.StartLayer; i < a.EndLayer; i++ {
				layers[i]++
			}
		case "expert":
			for _, u := range p.MoeUnits {
				if u.Layer >= 16 || u.Expert >= 64 {
					return nil, errors.New("invalid expert unit")
				}
				a.Units = append(a.Units, chainapp.MoeUnitRef{Layer: u.Layer, Expert: u.Expert})
				units[[2]uint32{u.Layer, u.Expert}] = true
			}
			sort.Slice(a.Units, func(i, j int) bool {
				if a.Units[i].Layer != a.Units[j].Layer {
					return a.Units[i].Layer < a.Units[j].Layer
				}
				return a.Units[i].Expert < a.Units[j].Expert
			})
		default:
			return nil, errors.New("missing MoE unit kind")
		}
		auth = append(auth, a)
	}
	if len(auth) != 6 || len(units) != 16*64 {
		return nil, errors.New("incomplete MoE authorization coverage")
	}
	for _, n := range layers {
		if n != 1 {
			return nil, errors.New("attention ranges do not tile layers")
		}
	}
	sort.Slice(auth, func(i, j int) bool { return auth[i].NodeID < auth[j].NodeID })
	return auth, nil
}
func (g *Gateway) finishMoe(b moeLedger, id string) {
	deadline := time.NewTimer(30 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		state, err := b.State(id)
		if err == nil {
			switch state {
			case "settled", "cancelled", "slashed":
				log.Printf("MOE SETTLED id=%s state=%s", id, state)
				return
			case "dispute":
				unaudited, e := b.ResolveAudit(id)
				outcome := ""
				if e == nil && unaudited {
					// Consensus decides whether this fate can settle, including
					// v15 early release. Keep retries on the existing ticker.
					outcome = "accept"
				} else if e == nil {
					if v, e := b.RecordedVerdict(id); e == nil && v != nil {
						outcome = v.Verdict
					} else {
						log.Printf("MOE AUDIT PENDING id=%s", id)
					}
				}
				if outcome != "" {
					if e := b.FinishMoe(id, outcome, time.Now(), 20); e == nil {
						log.Printf("MOE SETTLED id=%s outcome=%s", id, outcome)
						return
					} else {
						log.Printf("MOE settlement retry id=%s error=%v", id, e)
					}
				}
			}
		}
		select {
		case <-deadline.C:
			log.Printf("MOE finisher timeout id=%s", id)
			return
		case <-ticker.C:
		}
	}
}

// archiveMoeReceipt supplies the auditor's local receipt channel in addition
// to the executor's existing evidence index. It does not alter that index.
func (g *Gateway) archiveMoeReceipt(id string, receipt *pb.PipelineExecutionReceipt) error {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\\`) {
		return errors.New("invalid MoE receipt archive id")
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(receipt)
	if err != nil {
		return err
	}
	root := g.Moe.EvidenceRoot
	if root == "" {
		root = filepath.Join(g.Moe.WorkDir, "evidence")
	}
	dir := filepath.Join(root, id)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".receipt-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(dir, "receipt.pb")); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func moeExecutionTimeout(configured time.Duration) time.Duration {
	// Queue waiting has its own ten-minute budget; this is generation only.
	// Long-context generation needs a full hour even under gateway defaults.
	if configured < time.Hour {
		return time.Hour
	}
	return configured
}

func (g *Gateway) serveMoe(w http.ResponseWriter, r *http.Request, q request, prompt, account, id string) {
	b, ok := g.Ledger.(moeLedger)
	if !ok || g.ChainID != MoeProfile {
		http.Error(w, "MoE ledger unavailable", 503)
		return
	}
	release, ok := g.admit(w)
	if !ok {
		return
	}
	defer release()
	auth, err := g.moeAuthorizations(b)
	if err != nil {
		http.Error(w, err.Error(), 503)
		return
	}
	wire, input, err := g.tokenize(r.Context(), q, prompt, id, "")
	if err != nil {
		http.Error(w, "MoE tokenization failed", 503)
		return
	}
	if err := g.checkContext(input, q.MaxTokens); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	bound, _, _, err := g.quoteFee(input, uint32(q.MaxTokens))
	if err != nil {
		http.Error(w, err.Error(), 503)
		return
	}
	// Queue before FreezeMoe: waiting consumes neither the frozen hold lifetime
	// nor the SubmitMoe dispute/settlement window. The executor RequestTimeout
	// context is also created below, after freeze, so waiting cannot consume it.
	if moeSerializationEnabled() {
		releaseQueue, queueErr := g.moeGenerationQueue().acquire(r.Context())
		if queueErr != nil {
			msg := "MoE queue wait timeout, retry later"
			if errors.Is(queueErr, ErrMoeQueueFull) {
				msg = "MoE queue full, retry later"
			}
			w.Header().Set("Retry-After", "1")
			atomic.AddUint64(&g.rejected, 1)
			requestError(w, q.Stream, id, msg, http.StatusServiceUnavailable)
			return // No hold exists; CancelMoe must not run.
		}
		defer releaseQueue() // Includes executor cleanup, cancellation and SubmitMoe.
	}
	if err = b.FreezeMoe(id, account, bound.Total, auth, input, uint32(q.MaxTokens), wire); err != nil {
		http.Error(w, err.Error(), 402)
		return
	}
	var stderr stderrTail
	fail := func(e error) {
		atomic.AddUint64(&g.failed, 1)
		log.Printf("MOE FAILED id=%s error=%v stderr_path=%s", id, e, persistExecutorFailure(g.Moe.WorkDir, id, "moe", stderr.String()))
		requestError(w, q.Stream, id, "MoE execution failed", 502)
		if ce := b.CancelMoe(id); ce != nil {
			log.Printf("MOE cancel failed id=%s error=%v", id, ce)
		}
	}
	timeout := moeExecutionTimeout(g.RequestTimeout)
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	raw, _ := json.Marshal(map[string]any{"request_id": id, "prompt": prompt, "max_tokens": q.MaxTokens, "fleet_dir": g.Moe.FleetDir, "weights": g.Moe.Weights, "evidence_root": g.Moe.EvidenceRoot, "receipt_v2": g.Moe.ReceiptV2})
	c := exec.CommandContext(ctx, g.Moe.Python, "-m", "moe_gateway")
	c.Dir = g.Moe.WorkDir
	c.Env = append(os.Environ(), "PYTHONPATH=core-py:proto:node")
	c.Stdin = bytes.NewReader(raw)
	c.Stderr = &stderr
	stdout, err := c.StdoutPipe()
	if err != nil {
		fail(err)
		return
	}
	c.WaitDelay = time.Second
	if err = c.Start(); err != nil {
		fail(err)
		return
	}
	emit := func(v any) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		raw, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", raw)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	scanner := newEventReader(stdout)
	count := uint32(0)
	var output strings.Builder
	var receipt *pb.PipelineExecutionReceipt
	for scanner.Scan() {
		var e struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Receipt string `json:"receipt_b64"`
			Output  uint32 `json:"output_tokens"`
		}
		if err = json.Unmarshal(scanner.Bytes(), &e); err != nil {
			break
		}
		if receipt != nil {
			err = errors.New("event after result")
			break
		}
		switch e.Type {
		case "token":
			count++
			if count > uint32(q.MaxTokens) {
				err = errors.New("too many tokens")
				break
			}
			output.WriteString(e.Text)
			if q.Stream {
				emit(map[string]any{"id": id, "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": e.Text}}}})
			}
		case "result":
			var wire []byte
			wire, err = base64.StdEncoding.DecodeString(e.Receipt)
			if err == nil {
				receipt = &pb.PipelineExecutionReceipt{}
				err = proto.Unmarshal(wire, receipt)
			}
			if err == nil && e.Output != count {
				err = errors.New("result token mismatch")
			}
		default:
			err = errors.New("unknown MoE event")
		}
		if err != nil {
			break
		}
	}
	if err == nil {
		err = scanner.Err()
	}
	if err != nil {
		cancel()
		_ = c.Process.Kill()
		_ = stdout.Close()
		atomic.AddUint64(&g.failed, 1)
		requestError(w, q.Stream, id, "MoE execution failed", 502)
		if ce := b.CancelMoe(id); ce != nil {
			log.Printf("MOE cancel failed id=%s error=%v", id, ce)
		}
		cleanupErr := waitExecutor(c)
		log.Printf("MOE FAILED id=%s error=%v cleanup=%v stderr_path=%s", id, err, cleanupErr, persistExecutorFailure(g.Moe.WorkDir, id, "moe", stderr.String()))
		return
	}
	waitErr := waitExecutor(c)
	if err == nil {
		err = waitErr
	}
	if err == nil && (receipt == nil || receipt.RequestId != id || receipt.InputTokens != input || receipt.OutputTokens != count || count > uint32(q.MaxTokens) || receipt.Moe == nil) {
		err = errors.New("MoE receipt mismatch")
	}
	if err == nil {
		charge, e := g.settlementFee(id, input, count)
		if e != nil {
			log.Printf("MOE HoldFee fallback id=%s error=%v", id, e)
			charge, _, _, e = g.quoteFee(input, count)
		}
		err = e
		if err == nil {
			err = g.archiveMoeReceipt(id, receipt)
		}
		if err == nil {
			_, err = b.SubmitMoe(id, receipt, charge.Total)
		}
	}
	if err != nil {
		fail(err)
		return
	}
	go g.finishMoe(b, id)
	atomic.AddUint64(&g.succeeded, 1)
	log.Printf("MOE REQUEST id=%s input=%d output=%d", id, input, count)
	usage := map[string]uint32{"prompt_tokens": input, "completion_tokens": count, "total_tokens": input + count}
	if q.Stream {
		emit(map[string]any{"id": id, "object": "chat.completion.chunk", "choices": []any{}, "usage": usage})
		fmt.Fprint(w, "data: [DONE]\n\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"id": id, "object": "chat.completion", "model": q.Model, "choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": output.String()}, "finish_reason": "stop"}}, "usage": usage})
}
