package tunnel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sort"
	"time"

	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

// Audit jobs contain hex-encoded full-prefix tensors at every step and a
// finalized hold. There is no small hard bound on the latter. Keep each audit
// transport frame below the default 4 MiB instead of raising a shared limit.
const auditChunkBytes = 1 << 20

type auditManifest struct {
	Version int    `json:"version"`
	SHA256  string `json:"sha256"`
	Size    int    `json:"size"`
	Chunks  int    `json:"chunks"`
}

func (s *Server) audit(ctx context.Context, node, request, attempt string, payload []byte, receive func(*pb.TunnelFrame) error) error {
	return s.AuditReader(ctx, node, request, attempt, bytes.NewReader(payload), int64(len(payload)), receive)
}

// AuditReader keeps the existing manifest, chunk boundaries, hashes and retries,
// but reads evidence from a seekable disk spool rather than a whole-job slice.
func (s *Server) AuditReader(ctx context.Context, node, request, attempt string, payload io.ReadSeeker, size int64, receive func(*pb.TunnelFrame) error) (result error) {
	started := time.Now()
	chunks := (size + auditChunkBytes - 1) / auditChunkBytes
	index, offset, phase := -1, int64(0), "prepare"
	log.Printf("AUDIT_TRANSFER event=attempt_start hold=%s attempt=%s bytes=%d chunks=%d", request, attempt, size, chunks)
	defer func() {
		if result != nil {
			cause := "remote_or_io"
			if ctx.Err() != nil {
				cause = "parent_deadline_or_cancel"
			} else if errors.Is(result, context.DeadlineExceeded) {
				cause = "chunk_deadline"
			}
			log.Printf("AUDIT_TRANSFER event=failed hold=%s attempt=%s phase=%s chunk=%d offset=%d elapsed_s=%.3f cause=%s error=%q", request, attempt, phase, index, offset, time.Since(started).Seconds(), cause, result)
		}
	}()
	if _, err := payload.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if size <= auditChunkBytes {
		raw, err := io.ReadAll(io.LimitReader(payload, auditChunkBytes+1))
		if err != nil {
			return err
		}
		phase = "waiting_result"
		log.Printf("AUDIT_TRANSFER event=waiting_result hold=%s attempt=%s mode=inline elapsed_s=%.3f", request, attempt, time.Since(started).Seconds())
		return s.run(ctx, node, request, attempt, "audit", raw, receive)
	}
	h := sha256.New()
	if _, err := io.Copy(h, payload); err != nil {
		return err
	}
	root := h.Sum(nil)
	if _, err := payload.Seek(0, io.SeekStart); err != nil {
		return err
	}
	manifest, err := json.Marshal(auditManifest{1, hex.EncodeToString(root), int(size), int((size + auditChunkBytes - 1) / auditChunkBytes)})
	if err != nil {
		return err
	}
	resume := 0
	ack := func(kind, suffix string, data, expected []byte) error {
		var last error
		for retry := 0; retry < 3; retry++ {
			// A missing ack retransmits only this piece, with a fresh signed nonce and
			// attempt ID. The receiver accepts an identical duplicate idempotently.
			pieceCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			last = s.run(pieceCtx, node, request, fmt.Sprintf("%s-%s-%d", attempt, suffix, retry), kind, data, func(f *pb.TunnelFrame) error {
				ackPayload := f.Payload
				if kind == "audit_begin" {
					resume = 0
					if len(ackPayload) == 36 && bytes.Equal(ackPayload[:32], expected) {
						n := binary.BigEndian.Uint32(ackPayload[32:])
						if int64(n) > chunks {
							return errors.New("invalid audit resume watermark")
						}
						resume = int(n)
						ackPayload = ackPayload[:32]
					}
				}
				if f.Kind != "audit_chunk_ack" || !bytes.Equal(ackPayload, expected) {
					return errors.New("invalid audit chunk acknowledgement")
				}
				return nil
			})
			childErr := pieceCtx.Err()
			cancel()
			if last != nil {
				cause := "remote_or_io"
				if ctx.Err() != nil {
					cause = "parent_deadline_or_cancel"
				} else if childErr == context.DeadlineExceeded {
					cause = "chunk_deadline"
				}
				log.Printf("AUDIT_TRANSFER event=piece_failed hold=%s attempt=%s kind=%s chunk=%d offset=%d retry=%d cause=%s elapsed_s=%.3f", request, attempt, kind, index, offset, retry+1, cause, time.Since(started).Seconds())
			}
			if last == nil {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		return last
	}
	phase = "begin"
	if err = ack("audit_begin", "begin", manifest, root); err != nil {
		return err
	}
	log.Printf("AUDIT_TRANSFER event=begin_ack hold=%s attempt=%s resume_chunk=%d", request, attempt, resume)
	if _, err = payload.Seek(int64(resume)*auditChunkBytes, io.SeekStart); err != nil {
		return err
	}
	phase = "transfer"
	var timings []float64
	for index, offset = resume, int64(resume)*auditChunkBytes; offset < size; index, offset = index+1, offset+auditChunkBytes {
		pieceStart := time.Now()
		length := int64(auditChunkBytes)
		if size-offset < length {
			length = size - offset
		}
		data := make([]byte, int(length))
		if _, err = io.ReadFull(payload, data); err != nil {
			return err
		}
		wire, e := proto.Marshal(&pb.EvidenceChunk{ManifestRoot: root, Index: uint32(index), Data: data})
		if e != nil {
			return e
		}
		hash := sha256.Sum256(data)
		if err = ack("audit_chunk", fmt.Sprint(index), wire, hash[:]); err != nil {
			return err
		}
		timings = append(timings, time.Since(pieceStart).Seconds())
		if (index+1)%200 == 0 || int64(index+1) == chunks {
			sort.Float64s(timings)
			log.Printf("AUDIT_TRANSFER event=progress hold=%s attempt=%s resume_chunk=%d chunk=%d chunks=%d bytes=%d elapsed_s=%.3f piece_p50_s=%.3f piece_p95_s=%.3f piece_max_s=%.3f", request, attempt, resume, index+1, chunks, offset+length, time.Since(started).Seconds(), timings[len(timings)/2], timings[(len(timings)-1)*95/100], timings[len(timings)-1])
			timings = timings[:0]
		}
	}
	phase = "waiting_result"
	log.Printf("AUDIT_TRANSFER event=transfer_done_waiting_result hold=%s attempt=%s bytes=%d chunks=%d elapsed_s=%.3f", request, attempt, size, chunks, time.Since(started).Seconds())
	return s.run(ctx, node, request, attempt+"-commit", "audit_commit", manifest, receive)
}
