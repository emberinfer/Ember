package gateway

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	core "ember/core-go"
	"ember/services/auditqueue"
	chain "ember/services/chain"
)

const MaxImageBytes = 16 << 20

// ImageStore uses the audit spool's fsync/atomic rename discipline. A record
// contains both the bytes and the exact signed receipt, so restart is atomic.
type ImageStore struct {
	mu    sync.Mutex
	queue *auditqueue.Queue
	key   ed25519.PrivateKey
}
type ImageDelivery struct {
	ArrivalUnixNano   int64                  `json:"arrival_unix_nano"`
	PersistedUnixNano int64                  `json:"persisted_unix_nano"`
	Submission        chain.ImageSubmission  `json:"submission"`
	Receipt           chain.SpoolAttestation `json:"receipt"`
	Image             []byte                 `json:"image"`
}

func OpenImageStore(dir string, key ed25519.PrivateKey) (*ImageStore, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("registered gateway key required")
	}
	q, e := auditqueue.Open(dir)
	if e != nil {
		return nil, e
	}
	return &ImageStore{queue: q, key: append(ed25519.PrivateKey(nil), key...)}, nil
}
func imageKey(id string) string { return "image-" + id }
func (s *ImageStore) record(id string) (*ImageDelivery, error) {
	raw, e := s.queue.Get(imageKey(id))
	if e != nil {
		return nil, e
	}
	var d ImageDelivery
	e = json.Unmarshal(raw, &d)
	return &d, e
}
func (s *ImageStore) sign(h *chain.Hold, binding []byte, received bool, height int64) chain.SpoolAttestation {
	a := chain.SpoolAttestation{RequestID: h.ID, ChallengeHeight: h.FreezeHeight, StepSetHash: binding, Gateway: core.NodeID(s.key.Public().(ed25519.PublicKey)), Received: received, Height: height}
	a.Signature = ed25519.Sign(s.key, chain.SpoolSigningBytes(a))
	return a
}
func (s *ImageStore) Put(h *chain.Hold, sub chain.ImageSubmission, src io.Reader, height int64) (*ImageDelivery, error) {
	data, e := io.ReadAll(io.LimitReader(src, MaxImageBytes+1))
	if e != nil {
		return nil, e
	}
	sum := sha256.Sum256(data)
	if sub.ImageBytes != uint64(len(data)) || sub.ImageSHA256 != hex.EncodeToString(sum[:]) {
		return nil, errors.New("image hash/length mismatch")
	}
	return s.PutBinding(h, chain.ImageDeliveryBinding(&sub), bytes.NewReader(data), height)
}

// PutBinding matches T2 transport.spool(png,binding,holdID,hold). The final
// submission later proves that pixel/final-latent anchors match this binding.
func (s *ImageStore) PutBinding(h *chain.Hold, binding []byte, src io.Reader, height int64) (*ImageDelivery, error) {
	return s.PutBindingAt(h, binding, src, func() (int64, error) { return height, nil })
}

// PutBindingAt snapshots the finalized height only after the image bytes have
// been fsynced. An interrupted arrival also blocks false absence attestations.
func (s *ImageStore) PutBindingAt(h *chain.Hold, binding []byte, src io.Reader, landedHeight func() (int64, error)) (*ImageDelivery, error) {
	arrived := time.Now().UnixNano()
	s.mu.Lock()
	defer s.mu.Unlock()
	if h == nil || h.Image == nil || h.State != "frozen" || len(binding) != 32 {
		return nil, errors.New("invalid image arrival")
	}
	data, e := io.ReadAll(io.LimitReader(src, MaxImageBytes+1))
	if e != nil {
		return nil, e
	}
	if len(data) == 0 || len(data) > MaxImageBytes {
		return nil, errors.New("invalid image size")
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if old, e := s.record(h.ID); e == nil {
		if !bytes.Equal(binding, old.Receipt.StepSetHash) || old.Submission.ImageSHA256 != hash {
			return nil, errors.New("image already delivered with another binding")
		}
		return old, nil
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	sub := chain.ImageSubmission{ImageSHA256: hash, ImageBytes: uint64(len(data))}
	arrivalKey := "arrival-" + h.ID
	if raw, e := s.queue.Get(arrivalKey); e == nil {
		var prior ImageDelivery
		if e = json.Unmarshal(raw, &prior); e != nil {
			return nil, e
		}
		if prior.Submission.ImageSHA256 != hash || !bytes.Equal(prior.Receipt.StepSetHash, binding) {
			return nil, errors.New("incomplete arrival has another binding")
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	arrival := &ImageDelivery{Submission: sub, Image: data, Receipt: chain.SpoolAttestation{StepSetHash: binding}}
	arrivalRaw, e := json.Marshal(arrival)
	if e != nil {
		return nil, e
	}
	if e = s.queue.Put(arrivalKey, arrivalRaw); e != nil {
		return nil, e
	}
	height, e := landedHeight()
	if e != nil {
		return nil, e
	}
	if height < h.FreezeHeight || height >= h.ExpireHeight {
		return nil, errors.New("image arrival outside frozen lifetime")
	}
	d := &ImageDelivery{ArrivalUnixNano: arrived, PersistedUnixNano: time.Now().UnixNano(), Submission: sub, Receipt: s.sign(h, append([]byte(nil), binding...), true, height), Image: data}
	raw, e := json.Marshal(d)
	if e == nil {
		e = s.queue.Put(imageKey(h.ID), raw)
	}
	if e != nil {
		return nil, e
	}
	if e = s.queue.Done(arrivalKey); e != nil {
		return nil, e
	}
	return d, nil
}
func (s *ImageStore) Get(id string) (*ImageDelivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, e := s.record(id)
	if e != nil {
		return nil, e
	}
	sum := sha256.Sum256(d.Image)
	if hex.EncodeToString(sum[:]) != d.Submission.ImageSHA256 || uint64(len(d.Image)) != d.Submission.ImageBytes {
		return nil, errors.New("corrupt image spool")
	}
	return d, nil
}

// Absence is serialized with writes and fails closed on storage errors. T1's
// image_unopened/image_counterproof consume these ordinary spool attestations.
func (s *ImageStore) Absence(h *chain.Hold, height int64) (chain.SpoolAttestation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h == nil || h.Image == nil || h.Image.Submission == nil || h.Image.Delivery == nil || height <= h.Image.Delivery.Deadline {
		return chain.SpoolAttestation{}, errors.New("delivery deadline not passed")
	}
	if _, e := s.record(h.ID); e == nil {
		return chain.SpoolAttestation{}, errors.New("image arrival exists")
	} else if !os.IsNotExist(e) {
		return chain.SpoolAttestation{}, e
	}
	if _, e := s.queue.Get("arrival-" + h.ID); e == nil {
		return chain.SpoolAttestation{}, errors.New("durable image arrival awaits receipt")
	} else if !os.IsNotExist(e) {
		return chain.SpoolAttestation{}, e
	}
	return s.sign(h, chain.ImageDeliveryBinding(h.Image.Submission), false, height), nil
}
