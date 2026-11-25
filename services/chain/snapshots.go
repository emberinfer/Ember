package chain

// Snapshots are local transport artifacts. No snapshot field enters State.Bytes.
import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	abci "github.com/cometbft/cometbft/abci/types"
)

const snapshotFormat uint32 = 1
const snapshotChunkSize = 1 << 20
const snapshotMaxBytes int64 = 256 << 20

type snapshotMetadata struct {
	Chain  string   `json:"chain"`
	Size   int64    `json:"size"`
	Hashes [][]byte `json:"hashes"`
}
type snapshotStore struct {
	workers     sync.WaitGroup
	mu          sync.Mutex
	root, chain string
	interval    uint64
	keep        int
	busy        bool
	offer       *abci.Snapshot
	meta        snapshotMetadata
	expected    []byte
	temp        string
	next        uint32
}

// ConfigureSnapshots separates production (interval=0 disables it) from consumption.
func (app *Application) ConfigureSnapshots(chain string, interval uint64, keep int) error {
	if app.path == "" || chain == "" || keep < 1 || keep > 20 {
		return errors.New("snapshots require a state path, chain ID and keep_recent in 1..20")
	}
	app.snapshots = &snapshotStore{root: app.path + ".snapshots", chain: chain, interval: interval, keep: keep}
	return os.MkdirAll(app.snapshots.root, 0700)
}

// Called with the application lock. Pin inodes before later commits can remove
// spool files. Packaging, hashing and transport run outside the application lock.
func (app *Application) pinSnapshot() {
	s := app.snapshots
	if s == nil || app.committed == nil || s.interval == 0 || app.committed.Height <= 0 || uint64(app.committed.Height)%s.interval != 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy {
		return
	}
	tmp, err := os.MkdirTemp(s.root, ".build-")
	if err != nil {
		log.Printf("snapshot pin: %v", err)
		return
	}
	fail := func(e error) { os.RemoveAll(tmp); log.Printf("snapshot pin: %v", e) }
	// State and archive files are atomically replaced, never modified in place.
	if err = os.Link(app.path, filepath.Join(tmp, "state")); err != nil {
		fail(err)
		return
	}
	for _, suffix := range []string{".archive", ".chunk-spool"} {
		root := app.path + suffix
		err = filepath.WalkDir(root, func(path string, d os.DirEntry, e error) error {
			if os.IsNotExist(e) && path == root {
				return nil
			}
			if e != nil {
				return e
			}
			rel, e := filepath.Rel(root, path)
			if e != nil {
				return e
			}
			dest := filepath.Join(tmp, "state"+suffix, rel)
			if d.IsDir() {
				return os.MkdirAll(dest, 0700)
			}
			if !d.Type().IsRegular() {
				return errors.New("nonregular snapshot dependency")
			}
			return os.Link(path, dest)
		})
		if err != nil {
			fail(err)
			return
		}
	}
	height := uint64(app.committed.Height)
	s.busy = true
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		defer os.RemoveAll(tmp)
		err := s.build(tmp, height)
		s.mu.Lock()
		s.busy = false
		s.mu.Unlock()
		if err != nil {
			log.Printf("snapshot build height=%d: %v", height, err)
		}
	}()
}

func (s *snapshotStore) build(tmp string, height uint64) error {
	out, err := os.CreateTemp(s.root, ".package-")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	defer out.Close()
	tw := tar.NewWriter(out)
	var size int64
	err = filepath.WalkDir(tmp, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		size += info.Size() + 1024
		if size > snapshotMaxBytes {
			return errors.New("snapshot exceeds 256 MiB limit")
		}
		rel, _ := filepath.Rel(tmp, path)
		if e = tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel), Mode: 0600, Size: info.Size()}); e != nil {
			return e
		}
		f, e := os.Open(path)
		if e != nil {
			return e
		}
		defer f.Close()
		_, e = io.Copy(tw, f)
		return e
	})
	if err != nil {
		return err
	}
	if err = tw.Close(); err != nil {
		return err
	}
	if _, err = out.Seek(0, 0); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(s.root, ".ready-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	m := snapshotMetadata{Chain: s.chain}
	all := sha256.New()
	for i := 0; ; i++ {
		b := make([]byte, snapshotChunkSize)
		n, e := io.ReadFull(out, b)
		if e == io.EOF {
			break
		}
		if e != nil && e != io.ErrUnexpectedEOF {
			return e
		}
		b = b[:n]
		m.Size += int64(n)
		if m.Size > snapshotMaxBytes {
			return errors.New("snapshot too large")
		}
		h := sha256.Sum256(b)
		m.Hashes = append(m.Hashes, h[:])
		all.Write(b)
		if err = durableWrite(filepath.Join(dir, fmt.Sprint(i)), b); err != nil {
			return err
		}
	}
	metadata, _ := json.Marshal(m)
	snap := abci.Snapshot{Height: height, Format: snapshotFormat, Chunks: uint32(len(m.Hashes)), Hash: all.Sum(nil), Metadata: metadata}
	raw, _ := json.Marshal(&snap)
	if err = durableWrite(filepath.Join(dir, "snapshot.json"), raw); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dest := filepath.Join(s.root, fmt.Sprintf("%020d", height))
	if err = os.Rename(dir, dest); err != nil {
		return err
	}
	list, _ := s.list()
	for i := s.keep; i < len(list); i++ {
		os.RemoveAll(filepath.Join(s.root, fmt.Sprintf("%020d", list[i].Height)))
	}
	return nil
}
func (s *snapshotStore) list() ([]*abci.Snapshot, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	var out []*abci.Snapshot
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		b, e := os.ReadFile(filepath.Join(s.root, e.Name(), "snapshot.json"))
		if e != nil {
			continue
		}
		var v abci.Snapshot
		if json.Unmarshal(b, &v) == nil {
			out = append(out, &v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Height > out[j].Height })
	return out, nil
}
func (app *Application) ListSnapshots(context.Context, *abci.ListSnapshotsRequest) (*abci.ListSnapshotsResponse, error) {
	if app.snapshots == nil {
		return &abci.ListSnapshotsResponse{}, nil
	}
	s := app.snapshots
	s.mu.Lock()
	defer s.mu.Unlock()
	v, e := s.list()
	return &abci.ListSnapshotsResponse{Snapshots: v}, e
}
func (app *Application) LoadSnapshotChunk(_ context.Context, r *abci.LoadSnapshotChunkRequest) (*abci.LoadSnapshotChunkResponse, error) {
	out := &abci.LoadSnapshotChunkResponse{}
	s := app.snapshots
	if s == nil || r.Format != snapshotFormat {
		return out, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	list, _ := s.list()
	for _, v := range list {
		if v.Height == r.Height && r.Chunk < v.Chunks {
			b, e := os.ReadFile(filepath.Join(s.root, fmt.Sprintf("%020d", r.Height), fmt.Sprint(r.Chunk)))
			out.Chunk = b
			return out, e
		}
	}
	return out, nil
}
func (s *snapshotStore) reset() {
	if s.temp != "" {
		os.RemoveAll(s.temp)
	}
	s.temp = ""
	s.offer = nil
	s.next = 0
}
func (app *Application) OfferSnapshot(_ context.Context, r *abci.OfferSnapshotRequest) (*abci.OfferSnapshotResponse, error) {
	out := &abci.OfferSnapshotResponse{Result: abci.OFFER_SNAPSHOT_RESULT_REJECT}
	s := app.snapshots
	if s == nil {
		return out, nil
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reset()
	v := r.Snapshot
	if v == nil || v.Format != snapshotFormat || v.Height == 0 || v.Height > uint64(^uint64(0)>>1) || v.Chunks == 0 || v.Chunks > 256 || len(v.Metadata) > 32768 || len(v.Hash) != 32 || len(r.AppHash) != 32 {
		return out, nil
	}
	if app.committed != nil && app.committed.Height != 0 {
		return out, nil
	}
	var m snapshotMetadata
	if json.Unmarshal(v.Metadata, &m) != nil || m.Chain != s.chain || m.Size <= 0 || m.Size > snapshotMaxBytes || len(m.Hashes) != int(v.Chunks) || uint32((m.Size+snapshotChunkSize-1)/snapshotChunkSize) != v.Chunks {
		return out, nil
	}
	for _, h := range m.Hashes {
		if len(h) != 32 {
			return out, nil
		}
	}
	tmp, e := os.MkdirTemp(s.root, ".restore-")
	if e != nil {
		return out, e
	}
	copyV := *v
	s.offer = &copyV
	s.meta = m
	s.expected = append([]byte(nil), r.AppHash...)
	s.temp = tmp
	out.Result = abci.OFFER_SNAPSHOT_RESULT_ACCEPT
	return out, nil
}
func (app *Application) ApplySnapshotChunk(_ context.Context, r *abci.ApplySnapshotChunkRequest) (*abci.ApplySnapshotChunkResponse, error) {
	out := &abci.ApplySnapshotChunkResponse{Result: abci.APPLY_SNAPSHOT_CHUNK_RESULT_REJECT_SNAPSHOT}
	s := app.snapshots
	if s == nil {
		return out, nil
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	reject := func() (*abci.ApplySnapshotChunkResponse, error) { s.reset(); return out, nil }
	if s.offer == nil || r.Index != s.next || len(r.Chunk) > snapshotChunkSize {
		return reject()
	}
	want := int64(snapshotChunkSize)
	if r.Index == s.offer.Chunks-1 {
		want = s.meta.Size - int64(r.Index)*snapshotChunkSize
	}
	h := sha256.Sum256(r.Chunk)
	if int64(len(r.Chunk)) != want || !bytes.Equal(h[:], s.meta.Hashes[r.Index]) {
		return reject()
	}
	if e := durableWrite(filepath.Join(s.temp, fmt.Sprint(r.Index)), r.Chunk); e != nil {
		s.reset()
		return out, e
	}
	s.next++
	if s.next < s.offer.Chunks {
		out.Result = abci.APPLY_SNAPSHOT_CHUNK_RESULT_ACCEPT
		return out, nil
	}
	if e := app.installSnapshot(s); e != nil {
		log.Printf("snapshot rejected: %v", e)
		return reject()
	}
	s.reset()
	out.Result = abci.APPLY_SNAPSHOT_CHUNK_RESULT_ACCEPT
	return out, nil
}
func (app *Application) installSnapshot(s *snapshotStore) error {
	var readers []io.Reader
	var files []*os.File
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	for i := uint32(0); i < s.offer.Chunks; i++ {
		f, e := os.Open(filepath.Join(s.temp, fmt.Sprint(i)))
		if e != nil {
			return e
		}
		files = append(files, f)
		readers = append(readers, f)
	}
	hash := sha256.New()
	tr := tar.NewReader(io.TeeReader(io.MultiReader(readers...), hash))
	root := filepath.Join(s.temp, "unpack")
	if e := os.Mkdir(root, 0700); e != nil {
		return e
	}
	seen := map[string]bool{}
	var total int64
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		name := h.Name
		if h.Typeflag != tar.TypeReg || filepath.ToSlash(filepath.Clean(name)) != name || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") || !(name == "state" || strings.HasPrefix(name, "state.archive/") || strings.HasPrefix(name, "state.chunk-spool/")) || strings.Contains(name, "../") || seen[name] {
			return errors.New("invalid snapshot entry")
		}
		seen[name] = true
		total += h.Size
		if h.Size < 0 || total > snapshotMaxBytes {
			return errors.New("snapshot expanded size limit")
		}
		path := filepath.Join(root, filepath.FromSlash(name))
		if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
			return e
		}
		f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		_, e = io.Copy(f, tr)
		if e == nil {
			e = f.Sync()
		}
		f.Close()
		if e != nil {
			return e
		}
	}
	// Consume tar padding before checking the whole transport digest.
	if _, e := io.Copy(hash, io.MultiReader(readers...)); e != nil {
		return e
	}
	if !bytes.Equal(hash.Sum(nil), s.offer.Hash) {
		return errors.New("snapshot transport hash mismatch")
	}
	path := filepath.Join(root, "state")
	state, e := LoadState(path)
	if e != nil {
		return e
	}
	got, e := state.AppHash()
	if e != nil {
		return e
	}
	if state.Height != int64(s.offer.Height) || state.Parameters.ChainID != s.chain || !bytes.Equal(got, s.expected) {
		return errors.New("snapshot height, chain or expected app hash mismatch")
	}
	for _, h := range state.Holds {
		if h.Moe != nil && h.Moe.ChunkTotal > 0 {
			m, err := state.chunkSpool().GetManifest(h.ID)
			if err != nil {
				return err
			}
			if err = validateSpoolManifest(m, h); err != nil {
				return err
			}
			if len(h.Moe.ChunkBitmap) != (m.Chunks+7)/8 {
				return errors.New("invalid chunk bitmap")
			}
			for i := 0; i < m.Chunks; i++ {
				if h.Moe.ChunkBitmap[i/8]&(1<<uint(i%8)) == 0 {
					continue
				}
				b, err := state.chunkSpool().GetChunk(h.ID, i)
				if err != nil {
					return err
				}
				sum := sha256.Sum256(b)
				if len(b) != expectedChunkLength(m.Size, m.Chunks, i) || !bytes.Equal(sum[:], m.Hashes[i]) {
					return errors.New("corrupt required spool chunk")
				}
			}
		}
	}
	if app.committed != nil && app.committed.Height != 0 {
		return errors.New("snapshot target is not empty")
	}
	// Dependencies first; the state-file rename is the atomic visibility point.
	// A crash before it leaves only orphan dependencies in a height-zero home.
	for _, suffix := range []string{".archive", ".chunk-spool"} {
		src := path + suffix
		if _, e = os.Stat(src); os.IsNotExist(e) {
			continue
		}
		dest := app.path + suffix
		if e = os.RemoveAll(dest); e != nil {
			return e
		}
		if e = os.Rename(src, dest); e != nil {
			return e
		}
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	if e = durableWrite(app.path, raw); e != nil {
		return e
	}
	state.SetChunkSpoolPath(app.path)
	app.committed = state
	log.Printf("snapshot installed height=%d app_hash=%x", state.Height, got)
	app.staged = nil
	app.stagedBytes = nil
	return nil
}

// WaitSnapshotBuild drains the background writer after consensus has stopped.
func (app *Application) WaitSnapshotBuild() {
	if app.snapshots != nil {
		app.snapshots.workers.Wait()
	}
}
