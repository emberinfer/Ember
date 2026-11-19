package chain

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

type SpotLeaf struct {
	Step                                                        uint64
	InputHash, OutputHash, FormatHash, ProfileIDHash, RequestID [32]byte
}
type SpotProofNode struct {
	Hash [32]byte `json:"hash"`
	Left bool     `json:"left"`
}

func SpotLeafHash(l SpotLeaf) [32]byte {
	b := append([]byte{0}, []byte("EMBER-SPOTLEAF-V1")...)
	var step [8]byte
	binary.BigEndian.PutUint64(step[:], l.Step)
	b = append(b, step[:]...)
	for _, h := range [][32]byte{l.InputHash, l.OutputHash, l.FormatHash, l.ProfileIDHash, l.RequestID} {
		b = append(b, h[:]...)
	}
	return sha256.Sum256(b)
}
func spotParent(a, b [32]byte) [32]byte {
	raw := append([]byte{1}, a[:]...)
	raw = append(raw, b[:]...)
	return sha256.Sum256(raw)
}
func SpotMerkleRoot(leaves [][32]byte) ([32]byte, error) {
	if len(leaves) == 0 {
		return [32]byte{}, errors.New("empty tree")
	}
	level := append([][32]byte(nil), leaves...)
	for len(level) > 1 {
		next := make([][32]byte, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			j := i + 1
			if j == len(level) {
				j = i
			}
			next = append(next, spotParent(level[i], level[j]))
		}
		level = next
	}
	return level[0], nil
}
func SpotMerkleProof(leaves [][32]byte, index int) ([]SpotProofNode, error) {
	if index < 0 || index >= len(leaves) {
		return nil, errors.New("invalid leaf index")
	}
	level := append([][32]byte(nil), leaves...)
	proof := []SpotProofNode{}
	for len(level) > 1 {
		j := index ^ 1
		if j >= len(level) {
			j = index
		}
		proof = append(proof, SpotProofNode{level[j], j < index})
		next := [][32]byte{}
		for i := 0; i < len(level); i += 2 {
			j := i + 1
			if j == len(level) {
				j = i
			}
			next = append(next, spotParent(level[i], level[j]))
		}
		index /= 2
		level = next
	}
	return proof, nil
}
func VerifySpotProof(leaf, root [32]byte, proof []SpotProofNode) bool {
	for _, p := range proof {
		if p.Left {
			leaf = spotParent(p.Hash, leaf)
		} else {
			leaf = spotParent(leaf, p.Hash)
		}
	}
	return leaf == root
}

// Caller supplies raw row-major little-endian payload. This validates shape and
// byte count without interpreting floating point values or normalizing NaNs.
func SpotTensorBytes(dtype string, shape []uint64, payload []byte) ([]byte, error) {
	codes := map[string]byte{"f32": 1, "f64": 2, "bf16": 3, "f16": 4, "i64": 5, "i32": 6}
	sizes := map[string]uint64{"f32": 4, "f64": 8, "bf16": 2, "f16": 2, "i64": 8, "i32": 4}
	code := codes[dtype]
	if code == 0 || len(shape) > 255 {
		return nil, errors.New("invalid tensor descriptor")
	}
	size := sizes[dtype]
	for _, d := range shape {
		if d > 0 && size > ^uint64(0)/d {
			return nil, errors.New("tensor size overflow")
		}
		size *= d
	}
	if size != uint64(len(payload)) {
		return nil, errors.New("tensor payload length mismatch")
	}
	b := append([]byte("ETV1"), code, byte(len(shape)))
	for _, d := range shape {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], d)
		b = append(b, n[:]...)
	}
	return append(b, payload...), nil
}
func SpotFormatHash(dtype string, shape []uint64) [32]byte {
	dims := make([]string, len(shape))
	for i, d := range shape {
		dims[i] = fmt.Sprint(d)
	}
	return sha256.Sum256([]byte(dtype + "|" + strings.Join(dims, ",") + "|raw-le-v1"))
}
