package gateway

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"io"

	chain "ember/services/chain"
)

// HashSpotTensor hashes existing spool payloads with a bounded copy buffer.
// It never runs a model or materializes a complete tensor in memory.
func HashSpotTensor(r io.Reader, dtype string, shape []uint64) ([32]byte, error) {
	codes := map[string]byte{"f32": 1, "f64": 2, "bf16": 3, "f16": 4, "i64": 5, "i32": 6}
	widths := map[string]uint64{"f32": 4, "f64": 8, "bf16": 2, "f16": 2, "i64": 8, "i32": 4}
	size := widths[dtype]
	if size == 0 || len(shape) > 255 {
		return [32]byte{}, errors.New("invalid tensor descriptor")
	}
	for _, d := range shape {
		if d > 0 && size > ^uint64(0)/d {
			return [32]byte{}, errors.New("tensor size overflow")
		}
		size *= d
	}
	h := sha256.New()
	_, _ = h.Write(append([]byte("ETV1"), codes[dtype], byte(len(shape))))
	for _, d := range shape {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], d)
		_, _ = h.Write(b[:])
	}
	n, err := io.CopyBuffer(h, r, make([]byte, 64*1024))
	if err != nil {
		return [32]byte{}, err
	}
	if uint64(n) != size {
		return [32]byte{}, errors.New("tensor payload length mismatch")
	}
	return spotDigest(h), nil
}
func spotDigest(h hash.Hash) (v [32]byte) { copy(v[:], h.Sum(nil)); return }

// SpotRoot reuses the consensus implementation, including odd self-pairing.
func SpotRoot(leaves []chain.SpotLeaf) ([32]byte, error) {
	hashes := make([][32]byte, len(leaves))
	for i, l := range leaves {
		hashes[i] = chain.SpotLeafHash(l)
	}
	return chain.SpotMerkleRoot(hashes)
}
