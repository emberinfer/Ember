package gateway

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"

	"sync"

	core "ember/core-go"
	"ember/services/auditqueue"
	chain "ember/services/chain"
)

type SpotDelivery struct {
	Kind            string                 `json:"kind"`
	RequestID       string                 `json:"request_id"`
	ChallengeHeight int64                  `json:"challenge_height"`
	Height          int64                  `json:"height"`
	Steps           []uint64               `json:"steps"`
	Round           string                 `json:"round"`
	Receipt         chain.SpoolAttestation `json:"receipt"`
	Packages        []map[string]any       `json:"packages"`
	Delivered       map[string]bool        `json:"delivered"`
}

// All absence queries and opening writes share this lock and durable index.
// Queue uses fsync + atomic rename; one process owns the configured directory.
type SpotStore struct {
	mu    sync.Mutex
	queue *auditqueue.Queue
	key   ed25519.PrivateKey
}

func OpenSpotStore(dir string, key ed25519.PrivateKey) (*SpotStore, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("registered gateway key required")
	}
	q, err := auditqueue.Open(dir)
	if err != nil {
		return nil, err
	}
	return &SpotStore{queue: q, key: key}, nil
}
func (s *SpotStore) GatewayID() string { return core.NodeID(s.key.Public().(ed25519.PublicKey)) }

func spotID(id string, height int64) string { return fmt.Sprintf("%s-%d", id, height) }
func (s *SpotStore) sign(c *chain.SpotChallenge, received bool, height int64) chain.SpoolAttestation {
	a := chain.SpoolAttestation{RequestID: c.RequestID, ChallengeHeight: c.ChallengeHeight, StepSetHash: chain.SpotStepSetHash(c.StepSet), Gateway: core.NodeID(s.key.Public().(ed25519.PublicKey)), Received: received, Height: height}
	a.Signature = ed25519.Sign(s.key, chain.SpoolSigningBytes(a))
	return a
}
func (s *SpotStore) records(id string) ([]SpotDelivery, error) {
	ids, err := s.queue.Pending()
	if err != nil {
		return nil, err
	}
	var out []SpotDelivery
	for _, key := range ids {
		raw, err := s.queue.Get(key)
		if err != nil {
			return nil, err
		}
		var r SpotDelivery
		if err = json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		if r.RequestID == id {
			out = append(out, r)
		}
	}
	return out, nil
}
func (s *SpotStore) Receipt(id string, height int64) (chain.SpoolAttestation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := s.queue.Get(spotID(id, height))
	if err != nil {
		return chain.SpoolAttestation{}, err
	}
	var r SpotDelivery
	err = json.Unmarshal(raw, &r)
	return r.Receipt, err
}
func (s *SpotStore) Absence(c *chain.SpotChallenge, height int64) (chain.SpoolAttestation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if height <= c.Deadline || c.Deadline < c.ChallengeHeight || len(c.StepSet) == 0 {
		return chain.SpoolAttestation{}, errors.New("deadline not finalized")
	}
	rows, err := s.records(c.RequestID)
	if err != nil {
		return chain.SpoolAttestation{}, err
	}
	for _, r := range rows {
		if r.ChallengeHeight == c.ChallengeHeight && r.Height >= c.ChallengeHeight && r.Height <= c.Deadline {
			for _, a := range r.Steps {
				for _, b := range c.StepSet {
					if a == b {
						return chain.SpoolAttestation{}, errors.New("intersecting timely opening exists")
					}
				}
			}
		}
	}
	return s.sign(c, false, height), nil
}
func (s *SpotStore) Prepare(e *PipelineExecution, m *PipelineSpotCommitment, c *chain.SpotChallenge, height int64) (*SpotDelivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if height < c.ChallengeHeight || c.RequestID != m.RequestID || len(c.StepSet) == 0 {
		return nil, errors.New("invalid challenge identity/height")
	}
	key := spotID(c.RequestID, c.ChallengeHeight)
	if raw, err := s.queue.Get(key); err == nil {
		var r SpotDelivery
		err = json.Unmarshal(raw, &r)
		if err != nil {
			return nil, err
		}
		if r.Round != c.Round || !bytes.Equal(chain.SpotStepSetHash(r.Steps), chain.SpotStepSetHash(c.StepSet)) {
			return nil, errors.New("challenge set changed")
		}
		return &r, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	r := &SpotDelivery{Kind: "opening", RequestID: c.RequestID, ChallengeHeight: c.ChallengeHeight, Height: height, Steps: c.StepSet, Round: c.Round, Delivered: map[string]bool{}, Packages: []map[string]any{}}
	for _, step := range c.StepSet {
		p, err := BuildPipelineSpotPackage(e, m, step)
		if err != nil {
			return nil, err
		}
		r.Packages = append(r.Packages, p)
	}
	r.Receipt = s.sign(c, true, height)
	raw, err := json.Marshal(r)
	if err == nil {
		err = s.queue.Put(key, raw)
	}
	return r, err
}
func (s *SpotStore) MarkDelivered(r *SpotDelivery, auditor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Delivered == nil {
		r.Delivered = map[string]bool{}
	}
	r.Delivered[auditor] = true
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	// Queue Put is immutable/idempotent. Replace the delivery metadata via its
	// atomic stream writer in a separate, versioned marker record.
	marker := spotID(r.RequestID, r.ChallengeHeight) + "-" + hex.EncodeToString([]byte(auditor))
	return s.queue.Put(marker, raw)
}
func (s *SpotStore) Delivered(r *SpotDelivery, auditor string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := s.queue.Get(spotID(r.RequestID, r.ChallengeHeight) + "-" + hex.EncodeToString([]byte(auditor)))
	if err != nil {
		return false
	}
	var marker SpotDelivery
	return json.Unmarshal(raw, &marker) == nil && marker.Delivered[auditor]
}

// Cleanup is called with a freshly queried hold; no local timeout authorizes deletion.
func (s *SpotStore) Cleanup(q *auditqueue.Queue, h *chain.Hold) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	allowed := h.State == "settled" || h.State == "slashed" || h.State == "cancelled" || h.Sampling != nil && h.Unaudited
	if !allowed {
		return false, nil
	}
	rows, err := s.queue.Pending()
	if err != nil {
		return false, err
	}
	for _, id := range rows {
		raw, err := s.queue.Get(id)
		if err != nil {
			return false, err
		}
		var r SpotDelivery
		if err = json.Unmarshal(raw, &r); err != nil {
			return false, err
		}
		if r.RequestID == h.ID {
			if err = s.queue.Done(id); err != nil {
				return false, err
			}
		}
	}
	if err = q.Done(h.ID); err != nil {
		return false, err
	}
	log.Printf("SPOT_SPOOL_DELETE hold=%s state=%s unaudited=%t", h.ID, h.State, h.Unaudited)
	return true, nil
}
