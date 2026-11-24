package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	pb "ember/proto"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	core "ember/core-go"
	chain "ember/services/chain"
	"ember/services/scheduler"
)

// ImageBackend is the host's finalized-chain transaction channel. Freeze must
// sign as payer (the chain derives RequestID from the signed transaction), Submit must broadcast the original executor
// envelope. Apply signs coordinator actions and confirms their inclusion.
// Finalized must be called on each committed block and after audit verdicts;
// it never starts a timer or polls for fate.
type ImageBackend interface {
	Model(context.Context, string) (chain.Model, error)
	Hold(context.Context, string) (*chain.Hold, int64, error)
	Quote(context.Context, string, chain.ImageRequest) (int64, error)
	Freeze(context.Context, chain.Action) error
	Submit(context.Context, []byte) error
	Apply(context.Context, chain.Action) error
}
type ImageRunner func(context.Context, string, string, []byte) ([]byte, error)
type ImageService struct {
	Backend ImageBackend
	Store   *ImageStore
	// Run uses T2's run tunnel operation and returns its JSON envelope result.
	Run     ImageRunner
	Audit   ImageRunner
	auditMu sync.Mutex
	audits  map[string]*imageAuditAttempt
	// ChainID is the transaction domain, distinct from the model ID.
	ChainID string
}
type ImageRequest struct {
	Kind    string             `json:"kind"`
	Model   string             `json:"model"`
	Request chain.ImageRequest `json:"request"`
}

func imageCohort(model, profile string) string {
	return "text-to-image|" + model + "|" + profile + "|step"
}

// ChooseImage deliberately constructs a fresh scheduler request: no prefix,
// shard flag, or text receipt latency policy can enter this branch.
func (g *Gateway) ChooseImage(m chain.Model, tie uint64) (scheduler.Ranked, error) {
	if m.EffectiveKind() != "text-to-image" || m.ImageProfile == nil {
		return scheduler.Ranked{}, errors.New("not an image model")
	}
	var candidates []scheduler.Candidate
	for _, c := range g.liveCandidates() {
		p := c.Profile
		if p.ShardTotalLayers != 0 || p.MoeUnitKind != "" {
			continue
		}
		c.SpeedScore, _, _, _ = g.SpeedStats.ImageScore(p.NodeId, imageCohort(m.ID, m.ImageProfile.ExecutionProfile), time.Now())
		candidates = append(candidates, c)
	}
	return scheduler.Choose(scheduler.Request{Chain: m.ID, Profile: m.ImageProfile.ExecutionProfile, Manifest: g.Manifest, TieBreak: tie, SpeedEnabled: g.SpeedEnabled}, candidates)
}
func ValidateImageBudget(m chain.Model, h *chain.Hold) error {
	if h == nil || h.Image == nil || m.EffectiveKind() != "text-to-image" || h.ModelID != m.ID {
		return errors.New("invalid image hold")
	}
	actual, e := core.ImageActual(m.ImageProfile, h.Image.Request.Resolution, h.Image.Request.Steps, h.Image.MultiplierPPM)
	if e != nil {
		return e
	}
	if actual != h.Amount || actual != h.Actual {
		return fmt.Errorf("image Actual %d differs from frozen amount/Actual %d/%d", actual, h.Amount, h.Actual)
	}
	return nil
}

// SubmitImage checks the signed executor action and durable delivery before
// forwarding. It never changes a signed action to attach a spool receipt.
func (s *ImageService) SubmitImage(ctx context.Context, id string, raw []byte) error {
	h, _, e := s.Backend.Hold(ctx, id)
	if e != nil {
		return e
	}
	m, e := s.Backend.Model(ctx, h.ModelID)
	if e != nil {
		return e
	}
	if e = ValidateImageBudget(m, h); e != nil {
		return e
	}
	env, signer, e := chain.DecodeAndVerifyVersion(raw, 16)
	if e != nil {
		return e
	}
	a := env.Action
	if env.ChainID != s.ChainID || signer != h.Node || a.Type != "submit_image" || a.HoldID != id || a.Actual != 0 || a.ImageSubmission == nil {
		return errors.New("invalid image executor envelope")
	}
	d, e := s.Store.Get(id)
	if e != nil {
		return e
	}
	sub := a.ImageSubmission
	if !bytes.Equal(chain.ImageDeliveryBinding(sub), d.Receipt.StepSetHash) || sub.ImageSHA256 != d.Submission.ImageSHA256 || sub.ImageBytes != d.Submission.ImageBytes || sub.SpoolReceiptHash != chain.SpoolReceiptHash(d.Receipt) {
		return errors.New("image submission differs from durable receipt")
	}
	if e = s.Backend.Submit(ctx, raw); e != nil {
		return e
	}
	// Delivery evidence is a separate authorized coordinator/payer channel action.
	// T1 allows payer or executor to claim receipt; host Apply must use that signer.
	if e = s.Backend.Apply(ctx, chain.Action{Type: "image_received", HoldID: id, Spool: &d.Receipt}); e != nil {
		return e
	}
	return s.Finalized(ctx, id)
}
func (s *ImageService) Finalized(ctx context.Context, id string) error {
	h, height, e := s.Backend.Hold(ctx, id)
	if e != nil {
		return e
	}
	if h.Image == nil || h.State != "dispute" {
		return nil
	}
	if h.Image.Delivery != nil && !h.Image.Delivery.Received {
		d, e := s.Store.Get(id)
		if e != nil {
			return e
		}
		if e = s.Backend.Apply(ctx, chain.Action{Type: "image_received", HoldID: id, Spool: &d.Receipt}); e != nil {
			return e
		}
		h, height, e = s.Backend.Hold(ctx, id)
		if e != nil {
			return e
		}
	}
	if height <= h.SubmitHeight {
		return nil
	}
	if !h.Unaudited && h.Auditor == "" && !h.NoAuditor && h.AuditVerdict == "" {
		if e = s.Backend.Apply(ctx, chain.Action{Type: "assign_audit", HoldID: id}); e != nil {
			return e
		}
		h, height, e = s.Backend.Hold(ctx, id)
		if e != nil {
			return e
		}
	}
	outcome := h.AuditVerdict
	if h.Unaudited {
		outcome = "accept"
	}
	if outcome == "" {
		s.dispatchAudit(ctx, id, h, height)
		return nil
	}
	if h.Image.Delivery == nil || !h.Image.Delivery.Received {
		return errors.New("image fate ready but delivery unconfirmed")
	}
	receipts, e := chain.ExpectedAuditReceipts(h, outcome)
	if e != nil {
		return e
	}
	return s.Backend.Apply(ctx, chain.Action{Type: "settle", HoldID: id, Outcome: outcome, AuditReceipts: receipts})
}
func (g *Gateway) serveImage(w http.ResponseWriter, r *http.Request) {
	s := g.Image
	if s == nil || s.Backend == nil || s.Store == nil {
		http.Error(w, "image service unavailable", 503)
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == r.Header.Get("Authorization") || g.Ledger == nil {
		http.Error(w, "Bearer token required", 401)
		return
	}
	account, e := g.Ledger.TokenAccount(token)
	if e != nil {
		http.Error(w, "invalid bearer token", 401)
		return
	}
	fail := func(e error) { http.Error(w, e.Error(), 400) }
	write := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }
	path := strings.TrimPrefix(r.URL.Path, "/v1/images/")
	if path == "generations" || path == "quote" {
		if r.Method != "POST" {
			http.Error(w, "POST required", 405)
			return
		}
		if path == "generations" && !tokenCanSign(g.Ledger, token) {
			http.Error(w, "bearer has no signing capability", http.StatusForbidden)
			return
		}
		if !g.allowIdentity(account, time.Now()) {
			http.Error(w, "identity rate limit exceeded", 429)
			return
		}
		var q ImageRequest
		if e = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&q); e != nil {
			fail(e)
			return
		}
		if q.Kind != "text-to-image" {
			fail(errors.New("kind=text-to-image required"))
			return
		}
		m, e := s.Backend.Model(r.Context(), q.Model)
		if e != nil {
			fail(e)
			return
		}
		if m.EffectiveKind() != "text-to-image" || m.ImageProfile == nil {
			fail(errors.New("not an image model"))
			return
		}
		if g.CheckModelTrust != nil {
			if e = g.CheckModelTrust(m.ID); e != nil {
				fail(e)
				return
			}
		}
		amount, e := s.Backend.Quote(r.Context(), q.Model, q.Request)
		if e != nil {
			fail(e)
			return
		}
		if path == "quote" {
			write(map[string]any{"kind": q.Kind, "model": q.Model, "actual": amount, "freeze": amount})
			return
		}
		if s.Run == nil && g.Tunnel == nil {
			http.Error(w, "image runner unavailable", 503)
			return
		}
		if g.Admission != nil {
			select {
			case g.Admission <- struct{}{}:
				defer func() { <-g.Admission }()
			default:
				http.Error(w, "inference capacity exhausted", 429)
				return
			}
		}
		chosen, e := g.ChooseImage(m, atomic.AddUint64(&g.selection, 1)-1)
		if e != nil {
			http.Error(w, e.Error(), 503)
			return
		}
		holdID := id() + id()
		w.Header().Set("X-Hold-ID", holdID)
		if e = s.Backend.Freeze(r.Context(), chain.Action{Type: "image_request", Kind: q.Kind, HoldID: holdID, Payer: account, Node: chosen.NodeID, ModelID: q.Model, Amount: amount, ImageRequest: &q.Request}); e != nil {
			fail(e)
			return
		}
		timeout := g.RequestTimeout
		if timeout <= 0 {
			timeout = 30 * time.Minute
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		job, _ := json.Marshal(map[string]any{"kind": q.Kind, "chain": q.Model, "profile": m.ImageProfile.ExecutionProfile})
		started := time.Now()
		runner := s.Run
		if runner == nil {
			runner = g.runImageTunnel
		}

		result, e := runner(ctx, chosen.NodeID, holdID, job)
		if e != nil {
			http.Error(w, e.Error(), 502)
			return
		}
		var response struct {
			Envelope string `json:"envelope"`
		}
		if e = json.Unmarshal(result, &response); e != nil {
			fail(e)
			return
		}
		envelope, e := base64.StdEncoding.DecodeString(response.Envelope)
		if e != nil {
			fail(e)
			return
		}
		if e = s.SubmitImage(ctx, holdID, envelope); e != nil {
			http.Error(w, e.Error(), 502)
			return
		}
		elapsed := time.Since(started)
		// End-to-end delivery duration is separate from normalized step latency.
		g.SpeedStats.RecordImageDelivery(chosen.NodeID, imageCohort(m.ID, m.ImageProfile.ExecutionProfile), elapsed, time.Now())
		d, e := s.Store.Get(holdID)
		if e != nil {
			fail(e)
			return
		}
		// Gateway-observed generation time to the start of upload, normalized
		// by the frozen N. Includes fixed encoder/VAE overhead, excludes image
		// upload and receipt/TX delivery. This is amortized latency per step.
		if d.ArrivalUnixNano > started.UnixNano() && q.Request.Steps > 0 {
			g.SpeedStats.RecordImageStep(chosen.NodeID, imageCohort(m.ID, m.ImageProfile.ExecutionProfile), time.Duration(d.ArrivalUnixNano-started.UnixNano())/time.Duration(q.Request.Steps), time.Now())
		}
		write(map[string]any{"kind": q.Kind, "hold_id": holdID, "image_sha256": d.Submission.ImageSHA256, "image_bytes": d.Submission.ImageBytes, "receipt": d.Receipt, "spool_receipt_hash": chain.SpoolReceiptHash(d.Receipt), "url": "/v1/images/" + holdID + "/content"})
		return
	}
	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	h, height, e := s.Backend.Hold(r.Context(), parts[0])
	if e != nil {
		fail(e)
		return
	}
	if h.Image == nil {
		fail(errors.New("not an image hold"))
		return
	}
	if parts[1] == "spool" && r.Method == "PUT" {
		if account != h.Node {
			http.Error(w, "executor required", 403)
			return
		}
		binding, e := hex.DecodeString(r.Header.Get("X-Image-Binding"))
		if e != nil || len(binding) != 32 {
			fail(errors.New("X-Image-Binding must be a SHA-256 hex digest"))
			return
		}
		d, e := s.Store.PutBindingAt(h, binding, http.MaxBytesReader(w, r.Body, MaxImageBytes), func() (int64, error) { _, landed, e := s.Backend.Hold(r.Context(), h.ID); return landed, e })
		if e != nil {
			fail(e)
			return
		}
		// Exact Go canonical bytes: T2 hashes this response verbatim.
		raw, _ := json.Marshal(d.Receipt)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
		return
	}
	if account != h.Payer && account != h.Node {
		http.Error(w, "hold participant required", 403)
		return
	}
	if r.Method != "GET" {
		http.Error(w, "GET required", 405)
		return
	}
	if parts[1] == "absence" {
		a, e := s.Store.Absence(h, height)
		if e != nil {
			fail(e)
			return
		}
		write(a)
		return
	}
	if parts[1] == "status" {
		write(map[string]any{"kind": "text-to-image", "hold": h, "height": height})
		return
	}
	d, e := s.Store.Get(h.ID)
	if e != nil {
		http.Error(w, e.Error(), 404)
		return
	}
	switch parts[1] {
	case "receipt":
		write(map[string]any{"receipt": d.Receipt, "submission": d.Submission, "spool_receipt_hash": chain.SpoolReceiptHash(d.Receipt)})
	case "content":
		sum := sha256.Sum256(d.Image)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Image-SHA256", hex.EncodeToString(sum[:]))
		w.Header().Set("Cache-Control", "private, no-store")
		_, _ = w.Write(d.Image)
	default:
		http.NotFound(w, r)
	}
}

// FinalizedBlock is driven by the host's block-commit subscription. Replaying a
// block retries failed sends from durable spool records, including after restart.
// No polling interval controls assign_audit or settlement latency.
func (s *ImageService) FinalizedBlock(ctx context.Context) error {
	ids, e := s.Store.queue.Pending()
	if e != nil {
		return e
	}
	var errs []error
	for _, id := range ids {
		if strings.HasPrefix(id, "image-") {
			if e = s.Finalized(ctx, strings.TrimPrefix(id, "image-")); e != nil {
				errs = append(errs, e)
			}
		}
	}
	return errors.Join(errs...)
}

// The shared tunnel predates image_result and does not classify it as terminal.
// A private callback sentinel closes this operation without changing text Run.
func (g *Gateway) runImageTunnel(ctx context.Context, node, hold string, job []byte) ([]byte, error) {
	done := errors.New("image result received")
	var result []byte
	e := g.Tunnel.Run(ctx, node, hold, id(), job, func(f *pb.TunnelFrame) error {
		if f.Kind != "image_result" {
			return fmt.Errorf("unexpected image frame %s: %s", f.Kind, f.Payload)
		}
		result = append([]byte(nil), f.Payload...)
		return done
	})
	if errors.Is(e, done) {
		e = nil
	}
	return result, e
}

// imageAuditAttempt serializes retries for a finalized challenge, never a job hint.
type imageAuditAttempt struct {
	running, done bool
	retry         time.Time
	failures      uint
}

func (s *ImageService) dispatchAudit(ctx context.Context, id string, h *chain.Hold, height int64) {
	if s.Audit == nil || h.Unaudited || h.NoAuditor || h.Pipeline == nil || h.Pipeline.Spot == nil {
		return
	}
	c := h.Pipeline.Spot
	if h.Auditor == "" || c.Auditor != h.Auditor || c.ChallengeHeight <= 0 || height < c.ChallengeHeight || height > c.VerdictDeadline || len(c.StepSet) == 0 {
		return
	}
	key := fmt.Sprintf("%s/%s/%d", id, c.Round, c.ChallengeHeight)
	s.auditMu.Lock()
	if s.audits == nil {
		s.audits = map[string]*imageAuditAttempt{}
	}
	a := s.audits[key]
	if a == nil {
		a = &imageAuditAttempt{}
		s.audits[key] = a
	}
	if a.running || a.done || time.Now().Before(a.retry) {
		s.auditMu.Unlock()
		return
	}
	a.running = true
	s.auditMu.Unlock()
	auditor, round, challenge := h.Auditor, c.Round, c.ChallengeHeight
	job, _ := json.Marshal(map[string]any{"kind": "text-to-image", "hold_id": id, "challenge_height": challenge, "round": round})
	go func() {
		// Block callbacks must stay available for rotation and received commits.
		task, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Minute)
		defer cancel()
		log.Printf("IMAGE AUDIT send hold=%s auditor=%s round=%s challenge=%d", id, auditor, round, challenge)
		raw, err := s.Audit(task, auditor, id, job)
		if err == nil {
			var result struct {
				Envelope string `json:"envelope"`
			}
			err = json.Unmarshal(raw, &result)
			var envelope []byte
			if err == nil {
				envelope, err = base64.StdEncoding.DecodeString(result.Envelope)
			}
			if err == nil {
				var signer string
				var env chain.Envelope
				env, signer, err = chain.DecodeAndVerifyVersion(envelope, 16)
				if err == nil && (env.ChainID != s.ChainID || signer != auditor || env.Action.Type != "audit" || env.Action.HoldID != id) {
					err = errors.New("invalid image auditor envelope")
				}
			}
			if err == nil {
				current, at, readErr := s.Backend.Hold(task, id)
				err = readErr
				if err == nil && (current.State != "dispute" || current.AuditVerdict != "" || current.Auditor != auditor || current.Pipeline == nil || current.Pipeline.Spot == nil || current.Pipeline.Spot.ChallengeHeight != challenge || current.Pipeline.Spot.Round != round || at > current.Pipeline.Spot.VerdictDeadline) {
					err = errors.New("image audit challenge changed")
				}
			}
			if err == nil {
				err = s.Backend.Submit(task, envelope)
			}
			if err == nil {
				_, _, err = s.Backend.Hold(task, id)
			}
		}
		s.auditMu.Lock()
		a.running = false
		a.done = err == nil
		if err != nil {
			a.failures++
			a.retry = time.Now().Add(time.Second * time.Duration(1<<min(a.failures, 6)))
		}
		s.auditMu.Unlock()
		if err != nil {
			log.Printf("IMAGE AUDIT failed hold=%s round=%s challenge=%d: %v", id, round, challenge, err)
			return
		}
		// Re-read on the existing finalized block subscription before settlement.
		log.Printf("IMAGE AUDIT submitted hold=%s round=%s challenge=%d", id, round, challenge)
	}()
}
