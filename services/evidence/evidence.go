// Package evidence implements the off-chain evidence wire format and durable store.
package evidence

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	pb "ember/proto"
)

const DefaultMaxFrameTokens uint32 = 512

const MaxFrameBytes = 3 * 1024 * 1024

type Parameters struct {
	MaxFrameTokens uint32 `json:"max_frame_tokens"`
}

func (p Parameters) FrameTokens() uint32 {
	if p.MaxFrameTokens == 0 {
		return DefaultMaxFrameTokens
	}
	return p.MaxFrameTokens
}

// MerkleRoot pairs adjacent hashes, duplicating the last hash at odd levels.
// Interior nodes are SHA256(0x01 || left || right); leaves are SHA256(chunk).
func MerkleRoot(hashes [][]byte) []byte {
	if len(hashes) == 0 {
		return nil
	}
	level := append([][]byte(nil), hashes...)
	for len(level) > 1 {
		next := make([][]byte, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			right := level[i]
			if i+1 < len(level) {
				right = level[i+1]
			}
			input := append([]byte{1}, level[i]...)
			input = append(input, right...)
			h := sha256.Sum256(input)
			next = append(next, h[:])
		}
		level = next
	}
	return level[0]
}
func Validate(m *pb.EvidenceManifest, total, limit uint32) error {
	if m == nil || limit == 0 || m.TotalTokens != total || m.ChunkCount < 2 || int(m.ChunkCount) != len(m.ChunkHashes) || m.ChunkCount > total || uint64(total) > uint64(m.ChunkCount)*uint64(limit) {
		return errors.New("invalid evidence manifest coverage")
	}
	for _, h := range m.ChunkHashes {
		if len(h) != sha256.Size {
			return errors.New("invalid evidence chunk hash")
		}
	}
	if len(m.MerkleRoot) != 32 || !bytes.Equal(MerkleRoot(m.ChunkHashes), m.MerkleRoot) {
		return errors.New("invalid evidence Merkle root")
	}
	return nil
}
func VerifyChunk(m *pb.EvidenceManifest, index uint32, data []byte, limit uint32) (int, error) {
	if err := Validate(m, m.GetTotalTokens(), limit); err != nil {
		return 0, err
	}
	if len(data) > MaxFrameBytes {
		return 0, errors.New("evidence frame exceeds byte limit")
	}
	if index >= m.ChunkCount {
		return 0, errors.New("unknown evidence index")
	}
	h := sha256.Sum256(data)
	if !bytes.Equal(h[:], m.ChunkHashes[index]) {
		return 0, errors.New("evidence chunk hash mismatch")
	}
	var tokens []json.RawMessage
	if err := json.Unmarshal(data, &tokens); err != nil || len(tokens) == 0 || uint64(len(tokens)) > uint64(limit) {
		return 0, errors.New("invalid evidence frame token count")
	}
	return len(tokens), nil
}

type Store struct{ Dir string }

func (s *Store) Put(data []byte) ([]byte, error) {
	h := sha256.Sum256(data)
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(s.Dir, ".chunk-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(s.Dir, hex.EncodeToString(h[:])))
	}
	if err == nil {
		directory, openErr := os.Open(s.Dir)
		if openErr != nil {
			return nil, openErr
		}
		err = directory.Sync()
		closeErr = directory.Close()
		if err == nil {
			err = closeErr
		}
	}
	return h[:], err
}
func (s *Store) Fetch(m *pb.EvidenceManifest, index, limit uint32) ([]byte, error) {
	if err := Validate(m, m.GetTotalTokens(), limit); err != nil {
		return nil, err
	}
	if index >= m.ChunkCount {
		return nil, errors.New("unknown evidence index")
	}
	data, err := os.ReadFile(filepath.Join(s.Dir, hex.EncodeToString(m.ChunkHashes[index])))
	if err != nil {
		return nil, err
	}
	_, err = VerifyChunk(m, index, data, limit)
	return data, err
}
func (s *Store) Assemble(m *pb.EvidenceManifest, limit uint32) ([]byte, error) {
	if err := Validate(m, m.GetTotalTokens(), limit); err != nil {
		return nil, err
	}
	var tokens []json.RawMessage
	for i := uint32(0); i < m.GetChunkCount(); i++ {
		data, err := s.Fetch(m, i, limit)
		if err != nil {
			return nil, err
		}
		var group []json.RawMessage
		if err = json.Unmarshal(data, &group); err != nil {
			return nil, err
		}
		tokens = append(tokens, group...)
	}
	if len(tokens) != int(m.GetTotalTokens()) {
		return nil, errors.New("evidence total token count mismatch")
	}
	return json.Marshal(tokens)
}
