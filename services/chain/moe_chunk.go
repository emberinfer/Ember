package chain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

// ChunkPayloadBytes intentionally matches segaudit's auditChunkBytes.
const ChunkPayloadBytes = 1 << 20
const MaxChunkTotal = 256
const MaxChunkBytes = 256 * ChunkPayloadBytes

func expectedChunkLength(size, total, index int) int {
	if index == total-1 {
		return size - ChunkPayloadBytes*(total-1)
	}
	return ChunkPayloadBytes
}
func (s *State) chunkHold(signer string, a Action, height int64) (*Hold, error) {
	if s.Version < 13 {
		return nil, errors.New("MoE chunks require AppVersion 13")
	}
	h := s.Holds[a.HoldID]
	if h == nil || h.Moe == nil || h.State != "frozen" || height >= h.ExpireHeight || !s.Coordinators[signer] {
		return nil, errors.New("invalid chunked MoE submission")
	}
	return h, nil
}
func (s *State) submitMoeManifest(signer string, a Action, height int64) error {
	h, err := s.chunkHold(signer, a, height)
	if err != nil {
		return err
	}
	if a.Actual <= 0 || a.Actual > h.Amount || len(a.ChunkRoot) != sha256.Size || a.ChunkTotal < 1 || a.ChunkTotal > MaxChunkTotal || a.ChunkBytes < 1 || a.ChunkBytes > MaxChunkBytes || (a.ChunkBytes+ChunkPayloadBytes-1)/ChunkPayloadBytes != a.ChunkTotal || len(a.ChunkHashes) != a.ChunkTotal {
		return errors.New("invalid MoE chunk manifest")
	}
	for _, hash := range a.ChunkHashes {
		if len(hash) != sha256.Size {
			return errors.New("invalid MoE chunk hash")
		}
	}
	listHash := chunkListHash(a.ChunkHashes)
	v := h.Moe
	if v.ChunkTotal > 0 && (!bytes.Equal(v.ChunkRoot, a.ChunkRoot) || !bytes.Equal(v.ChunkListHash, listHash) || v.ChunkTotal != a.ChunkTotal || v.ChunkBytes != a.ChunkBytes || v.ChunkActual != a.Actual) {
		return errors.New("conflicting MoE chunk manifest")
	}
	// Write before publishing consensus metadata. Replay heals a lost local
	// manifest. The list is never assigned into any consensus State field.
	m := ChunkManifest{Version: 1, SHA256: hex.EncodeToString(a.ChunkRoot), Size: a.ChunkBytes, Chunks: a.ChunkTotal, Hashes: a.ChunkHashes, Actual: a.Actual}
	if err = s.chunkSpool().PutManifest(h.ID, m); err != nil {
		return err
	}
	if v.ChunkTotal == 0 {
		v.ChunkRoot = append([]byte(nil), a.ChunkRoot...)
		v.ChunkListHash = listHash
		v.ChunkTotal = a.ChunkTotal
		v.ChunkBytes = a.ChunkBytes
		v.ChunkActual = a.Actual
		v.ChunkBitmap = make([]byte, (a.ChunkTotal+7)/8)
	}
	return nil
}
func (s *State) submitMoeChunk(signer string, a Action, height int64) error {
	h, err := s.chunkHold(signer, a, height)
	if err != nil {
		return err
	}
	v := h.Moe
	if v.ChunkTotal == 0 || a.ChunkIndex < 0 || a.ChunkIndex >= v.ChunkTotal || len(a.ReceiptChunk) > ChunkPayloadBytes || len(a.ReceiptChunk) != expectedChunkLength(v.ChunkBytes, v.ChunkTotal, a.ChunkIndex) {
		return errors.New("invalid MoE receipt chunk")
	}
	// Expected hashes live only in manifest.json, populated by every validator
	// processing the manifest tx. Missing manifest rejects; resend the identical
	// manifest to heal it. Re-check its list hash against consensus state so a
	// speculative/local manifest can never select different expected hashes.
	m, err := s.chunkSpool().GetManifest(h.ID)
	if err != nil {
		return err
	}
	if err = validateSpoolManifest(m, h); err != nil {
		return err
	}
	sum := sha256.Sum256(a.ReceiptChunk)
	if !bytes.Equal(sum[:], m.Hashes[a.ChunkIndex]) {
		return errors.New("MoE receipt chunk hash mismatch")
	}
	// Always rewrite: duplicate chunks heal missing/corrupt spool files without
	// changing the bitmap (including after all bits have already been set).
	if err = s.chunkSpool().PutChunk(h.ID, a.ChunkIndex, a.ReceiptChunk); err != nil {
		return err
	}
	v.ChunkBitmap[a.ChunkIndex/8] |= 1 << uint(a.ChunkIndex%8)
	return nil
}
func (s *State) submitMoeFinalize(signer string, a Action, height int64) error {
	h, err := s.chunkHold(signer, a, height)
	if err != nil {
		return err
	}
	v := h.Moe
	if v.ChunkTotal == 0 || len(v.ChunkBitmap) != (v.ChunkTotal+7)/8 {
		return errors.New("missing MoE chunk manifest")
	}
	for i := 0; i < v.ChunkTotal; i++ {
		if v.ChunkBitmap[i/8]&(1<<uint(i%8)) == 0 {
			return errors.New("incomplete MoE receipt chunks")
		}
	}
	raw, err := s.chunkSpool().Reassemble(h)
	if err != nil {
		return err
	}
	return s.deriveMoeSubmission(signer, Action{HoldID: h.ID, Actual: v.ChunkActual, Receipt: raw}, height, false)
}

// moeReceipt is the only receipt source for adjudication. A chunked hold must
// verify its local bytes against the on-chain root; missing/corrupt spool data
// fails closed rather than silently judging an absent receipt.
func (s *State) moeReceipt(h *Hold) ([]byte, error) {
	if h == nil || h.Moe == nil {
		return nil, errors.New("missing MoE hold")
	}
	if h.Moe.ChunkTotal > 0 {
		return s.chunkSpool().Reassemble(h)
	}
	if len(h.Moe.Receipt) == 0 {
		return nil, errors.New("missing frozen MoE receipt")
	}
	return h.Moe.Receipt, nil
}
