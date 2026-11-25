package chain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Readiness has L=1000 (validators.go Membership.validate), so its hot window
// W=2000 >= 2L. Assignment entropy spans <=64 blocks (assignmentSeed), but its
// age is UNBOUNDED: registerAppeal may revisit a historical freeze. Therefore
// no finite global L exists; older hashes must remain readable in the archive.
const blockHashWindow int64 = 2000

type historyArchive struct {
	hashes        map[int64][]byte
	pendingHashes map[int64][]byte
	holds         map[string]*Hold
	pendingHolds  map[string]*Hold
	head          string
}
type archiveDelta struct {
	Parent string           `json:"parent,omitempty"`
	Hashes map[int64][]byte `json:"block_hashes,omitempty"`
	Holds  map[string]*Hold `json:"holds,omitempty"`
}

func archiveKey(raw []byte) string        { h := sha256.Sum256(raw); return hex.EncodeToString(h[:]) }
func archivePath(path, key string) string { return filepath.Join(path+".archive", key+".json") }

func (s *State) pruneBlockHashes() {
	if s.Version < 11 {
		return
	}
	old := s.history
	next := &historyArchive{hashes: map[int64][]byte{}, pendingHashes: map[int64][]byte{}, holds: map[string]*Hold{}, pendingHolds: map[string]*Hold{}}
	if old != nil {
		next.head = old.head
		for id, h := range old.holds {
			next.holds[id] = h
		}
		for id, h := range old.pendingHolds {
			next.pendingHolds[id] = h
		}
		for h, b := range old.hashes {
			next.hashes[h] = b
		}
		for h, b := range old.pendingHashes {
			next.pendingHashes[h] = b
		}
	}
	for h, b := range s.BlockHashes {
		if h < s.Height-blockHashWindow {
			next.hashes[h] = b
			next.pendingHashes[h] = b
			delete(s.BlockHashes, h)
		}
	}
	s.history = next
}
func (s *State) blockHash(height int64) []byte {
	if b := s.BlockHashes[height]; b != nil {
		return b
	}
	if s.Version >= 11 && s.history != nil {
		return s.history.hashes[height]
	}
	return nil
}

// Archive deltas are immutable and selected by the exact new state's hash.
// Writing an orphan delta before a crash is harmless: the old state's hash
// still selects its old chain. The state rename happens only after fsync.
func persistArchive(path string, s *State, raw []byte) error {
	if s.Version < 11 {
		return nil
	}
	d := archiveDelta{}
	if s.history != nil {
		d.Parent = s.history.head
		d.Hashes = s.history.pendingHashes
		d.Holds = s.history.pendingHolds
	}
	key := archiveKey(raw)
	if d.Parent != "" {
		if _, err := os.Stat(archivePath(path, d.Parent)); os.IsNotExist(err) {
			// Saving an independently usable snapshot into another directory includes
			// its complete archive, rather than dangling references to the source.
			d.Parent = ""
			d.Hashes = s.history.hashes
			d.Holds = s.history.holds
		} else if err != nil {
			return err
		}
	}
	if d.Parent == key {
		return nil
	}
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return durableWrite(archivePath(path, key), b)
}
func loadArchive(path string, s *State, raw []byte) error {
	if s.Version < 11 {
		return nil
	}
	key := archiveKey(raw)
	store := &historyArchive{head: key, hashes: map[int64][]byte{}, holds: map[string]*Hold{}}
	seen := map[string]bool{}
	for key != "" {
		if len(key) != 64 {
			return errors.New("invalid archive reference")
		}
		if _, err := hex.DecodeString(key); err != nil {
			return err
		}
		if seen[key] {
			return errors.New("archive reference cycle")
		}
		seen[key] = true
		b, err := os.ReadFile(archivePath(path, key))
		if os.IsNotExist(err) && key == archiveKey(raw) {
			// A local transport marker authorizes only this exact omitted root.
			// Missing files in ordinary archives remain corruption errors.
			marker, markerErr := os.ReadFile(path + ".archive-omitted")
			if markerErr == nil && string(marker) == key {
				s.history = store
				return nil
			}
		}
		if err != nil {
			return fmt.Errorf("state archive %s: %w", key, err)
		}
		var d archiveDelta
		if err = json.Unmarshal(b, &d); err != nil {
			return err
		}
		for h, b := range d.Hashes {
			if _, ok := store.hashes[h]; !ok {
				store.hashes[h] = b
			}
		}
		for id, h := range d.Holds {
			if _, ok := store.holds[id]; !ok {
				store.holds[id] = h
			}
		}
		key = d.Parent
	}
	s.history = store
	return nil
}
func durableWrite(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(path+".tmp", path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
