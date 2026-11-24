package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	chain "ember/services/chain"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestImageFinalizedAuditDispatch(t *testing.T) {
	for _, unsampled := range []bool{false, true} {
		t.Run(map[bool]string{false: "sampled", true: "unsampled"}[unsampled], func(t *testing.T) {
			h := imageHold()
			h.State = "dispute"
			h.Auditor = "auditor"
			h.SubmitHeight = 2
			h.Unaudited = unsampled
			h.Image.Delivery = &chain.SpotChallenge{Received: true}
			h.Pipeline = &chain.PipelineHold{Spot: &chain.SpotChallenge{Auditor: "auditor", ChallengeHeight: 3, Round: "1", StepSet: []uint64{1}, VerdictDeadline: 100}}
			calls := make(chan string, 10)
			release := make(chan struct{})
			s := &ImageService{Backend: &imageBook{h: h, height: 4}, Audit: func(_ context.Context, node, id string, job []byte) ([]byte, error) {
				calls <- string(job)
				<-release
				return nil, errors.New("disconnected")
			}}
			defer close(release)
			replay := func() {
				t.Helper()
				for i := 0; i < 10; i++ {
					if err := s.Finalized(context.Background(), h.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			replay()
			if unsampled {
				select {
				case v := <-calls:
					t.Fatal("unsampled dispatched", v)
				case <-time.After(30 * time.Millisecond):
				}
				return
			}
			select {
			case <-calls:
			case <-time.After(time.Second):
				t.Fatal("missing audit")
			}
			replay()
			select {
			case v := <-calls:
				t.Fatal("duplicate", v)
			default:
			}
			h.Pipeline.Spot.Round = "2"
			h.Pipeline.Spot.ChallengeHeight = 4
			replay()
			select {
			case <-calls:
			case <-time.After(time.Second):
				t.Fatal("missing rotation audit")
			}
			replay()
			select {
			case v := <-calls:
				t.Fatal("duplicate rotation", v)
			default:
			}
		})
	}
}

// Snapshot reads model the real RPC backend: callbacks never share mutable
// finalized state with the block subscription.
type imageAuditBook struct {
	*imageBook
	mu  sync.Mutex
	raw chan []byte
}

func (b *imageAuditBook) Hold(context.Context, string) (*chain.Hold, int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	raw, _ := json.Marshal(b.h)
	var h chain.Hold
	_ = json.Unmarshal(raw, &h)
	return &h, b.height, nil
}
func (b *imageAuditBook) Submit(_ context.Context, raw []byte) error { b.raw <- raw; return nil }
func TestImageAuditSignedEnvelopeAndReplay(t *testing.T) {
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	h := imageHold()
	h.State = "dispute"
	h.Auditor = chain.AccountID(key.Public().(ed25519.PublicKey))
	h.SubmitHeight = 2
	h.Image.Delivery = &chain.SpotChallenge{Received: true}
	h.Pipeline = &chain.PipelineHold{Spot: &chain.SpotChallenge{Auditor: h.Auditor, ChallengeHeight: 3, Round: "1", StepSet: []uint64{1}, VerdictDeadline: 100}}
	b := &imageAuditBook{imageBook: &imageBook{h: h, height: 4}, raw: make(chan []byte, 4)}
	calls := make(chan struct{}, 4)
	signed, err := chain.Sign("domain", key, 1, chain.Action{Type: "audit", HoldID: h.ID})
	if err != nil {
		t.Fatal(err)
	}
	s := &ImageService{Backend: b, ChainID: "domain", Audit: func(context.Context, string, string, []byte) ([]byte, error) {
		calls <- struct{}{}
		return json.Marshal(map[string]string{"envelope": base64.StdEncoding.EncodeToString(signed)})
	}}
	if err := s.Finalized(context.Background(), h.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case raw := <-b.raw:
		if !bytes.Equal(raw, signed) {
			t.Fatal("worker envelope modified")
		}
	case <-time.After(time.Second):
		t.Fatal("missing submission")
	}
	deadline := time.Now().Add(time.Second)
	for {
		s.auditMu.Lock()
		done := s.audits["hold/1/3"].done
		s.auditMu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not confirmed")
		}
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 20; i++ {
		if err := s.Finalized(context.Background(), h.ID); err != nil {
			t.Fatal(err)
		}
	}
	if len(calls) != 1 {
		t.Fatal("successful task replayed", len(calls))
	}
}
