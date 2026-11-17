package chain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ChunkManifest follows the segaudit manifest shape. Hashes are deliberately
// local-only: consensus stores only sha256(concat(Hashes)). Every validator
// obtains the same list by executing the manifest transaction.
type ChunkManifest struct {
	Version int      `json:"version"`
	SHA256  string   `json:"sha256"`
	Size    int      `json:"size"`
	Chunks  int      `json:"chunks"`
	Hashes  [][]byte `json:"hashes"`
	Actual  int64    `json:"actual"`
}

type ChunkSpool struct{ Root string }

// SetChunkSpoolPath binds local storage to the state-file path, just as the
// history archive does. In-memory/drill users must supply a local path too.
func (s *State) SetChunkSpoolPath(statePath string) {
	if statePath != "" {
		s.chunkSpoolRoot = statePath + ".chunk-spool"
	}
}
func (s *State) chunkSpool() *ChunkSpool { return &ChunkSpool{Root: s.chunkSpoolRoot} }
func (c *ChunkSpool) holdDir(hold string) (string, error) {
	if c.Root == "" {
		return "", errors.New("chunk spool has no state path")
	}
	if hold == "" || hold == "." || hold == ".." || filepath.Base(hold) != hold || strings.ContainsAny(hold, "/\\\x00") {
		return "", errors.New("invalid chunk spool hold id")
	}
	return filepath.Join(c.Root, hold), nil
}

// put uses a unique 0600 temporary file, fsync, atomic rename, and directory
// fsync. Unique files also make speculative CheckTx writes safe to repeat.
func (c *ChunkSpool) put(hold, name string, raw []byte) error {
	dir, err := c.holdDir(hold)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".chunk-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func (c *ChunkSpool) PutManifest(hold string, m ChunkManifest) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return c.put(hold, "manifest.json", raw)
}
func (c *ChunkSpool) GetManifest(hold string) (ChunkManifest, error) {
	var m ChunkManifest
	dir, err := c.holdDir(hold)
	if err != nil {
		return m, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(raw, &m)
	return m, err
}
func (c *ChunkSpool) PutChunk(hold string, index int, raw []byte) error {
	return c.put(hold, fmt.Sprintf("%d", index), raw)
}
func (c *ChunkSpool) GetChunk(hold string, index int) ([]byte, error) {
	dir, err := c.holdDir(hold)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(dir, fmt.Sprintf("%d", index)))
}
func (c *ChunkSpool) Remove(hold string) error {
	dir, err := c.holdDir(hold)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}
func chunkListHash(hashes [][]byte) []byte {
	h := sha256.New()
	for _, v := range hashes {
		h.Write(v)
	}
	return h.Sum(nil)
}
func validateSpoolManifest(m ChunkManifest, h *Hold) error {
	if h == nil || h.Moe == nil {
		return errors.New("no chunked hold")
	}
	v := h.Moe
	if m.Version != 1 || m.SHA256 != hex.EncodeToString(v.ChunkRoot) || m.Size != v.ChunkBytes || m.Chunks != v.ChunkTotal || m.Actual != v.ChunkActual || len(m.Hashes) != v.ChunkTotal {
		return errors.New("chunk spool manifest differs from state")
	}
	for _, hash := range m.Hashes {
		if len(hash) != sha256.Size {
			return errors.New("invalid spool chunk hash")
		}
	}
	if !bytes.Equal(chunkListHash(m.Hashes), v.ChunkListHash) {
		return errors.New("chunk spool list hash differs from state")
	}
	return nil
}
func (c *ChunkSpool) Reassemble(h *Hold) ([]byte, error) {
	m, err := c.GetManifest(h.ID)
	if err != nil {
		return nil, err
	}
	if err = validateSpoolManifest(m, h); err != nil {
		return nil, err
	}
	if m.Size < 1 || m.Size > MaxChunkBytes || m.Chunks < 1 || m.Chunks > MaxChunkTotal {
		return nil, errors.New("invalid chunk spool size")
	}
	raw := make([]byte, 0, m.Size)
	for i := 0; i < m.Chunks; i++ {
		part, e := c.GetChunk(h.ID, i)
		if e != nil {
			return nil, e
		}
		sum := sha256.Sum256(part)
		if len(part) != expectedChunkLength(m.Size, m.Chunks, i) || !bytes.Equal(sum[:], m.Hashes[i]) {
			return nil, errors.New("corrupt receipt chunk")
		}
		raw = append(raw, part...)
	}
	sum := sha256.Sum256(raw)
	if len(raw) != m.Size || !bytes.Equal(sum[:], h.Moe.ChunkRoot) {
		return nil, errors.New("receipt chunk root mismatch")
	}
	return raw, nil
}

// Cleanup runs only AFTER archive and state persistence succeeds. Removing in
// archiveTerminalHolds itself would let a speculative proposal erase a live
// dispute's spool. Failures leave harmless local files for the next commit.
func (s *State) cleanupChunkSpools() {
	if s.chunkSpoolRoot == "" || s.history == nil {
		return
	}
	for id, h := range s.history.holds {
		if s.Holds[id] == nil && h.Moe != nil && h.Moe.ChunkTotal > 0 && s.terminalHold(h) {
			_ = s.chunkSpool().Remove(id)
		}
	}
}
