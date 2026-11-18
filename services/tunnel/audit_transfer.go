package tunnel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
func (s *Server) AuditReader(ctx context.Context, node, request, attempt string, payload io.ReadSeeker, size int64, receive func(*pb.TunnelFrame) error) error {
	if _, err := payload.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if size <= auditChunkBytes {
		raw, err := io.ReadAll(io.LimitReader(payload, auditChunkBytes+1))
		if err != nil {
			return err
		}
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
	ack := func(kind, suffix string, data, expected []byte) error {
		var last error
		for retry := 0; retry < 3; retry++ {
			// A missing ack retransmits only this piece, with a fresh signed nonce and
			// attempt ID. The receiver accepts an identical duplicate idempotently.
			pieceCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			last = s.run(pieceCtx, node, request, fmt.Sprintf("%s-%s-%d", attempt, suffix, retry), kind, data, func(f *pb.TunnelFrame) error {
				if f.Kind != "audit_chunk_ack" || !bytes.Equal(f.Payload, expected) {
					return errors.New("invalid audit chunk acknowledgement")
				}
				return nil
			})
			cancel()
			if last == nil {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		return last
	}
	if err = ack("audit_begin", "begin", manifest, root); err != nil {
		return err
	}
	for index, offset := 0, int64(0); offset < size; index, offset = index+1, offset+auditChunkBytes {
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
	}
	return s.run(ctx, node, request, attempt+"-commit", "audit_commit", manifest, receive)
}
