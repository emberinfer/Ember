package chain

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	abci "github.com/cometbft/cometbft/abci/types"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func snapshotWait(t *testing.T, app *Application) *abci.Snapshot {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		r, e := app.ListSnapshots(context.Background(), &abci.ListSnapshotsRequest{})
		if e != nil {
			t.Fatal(e)
		}
		if len(r.Snapshots) > 0 {
			return r.Snapshots[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("snapshot build timeout")
	return nil
}
func snapshotTransfer(t *testing.T, donor, target *Application, snap *abci.Snapshot, expected []byte, mutate func(uint32, []byte) (uint32, []byte)) bool {
	t.Helper()
	ctx := context.Background()
	offer, e := target.OfferSnapshot(ctx, &abci.OfferSnapshotRequest{Snapshot: snap, AppHash: expected})
	if e != nil {
		t.Fatal(e)
	}
	if offer.Result != abci.OFFER_SNAPSHOT_RESULT_ACCEPT {
		return false
	}
	for i := uint32(0); i < snap.Chunks; i++ {
		r, e := donor.LoadSnapshotChunk(ctx, &abci.LoadSnapshotChunkRequest{Height: snap.Height, Format: snap.Format, Chunk: i})
		if e != nil {
			t.Fatal(e)
		}
		index, b := i, r.Chunk
		if mutate != nil {
			index, b = mutate(i, b)
		}
		result, e := target.ApplySnapshotChunk(ctx, &abci.ApplySnapshotChunkRequest{Index: index, Chunk: b})
		if e != nil {
			t.Fatal(e)
		}
		if result.Result != abci.APPLY_SNAPSHOT_CHUNK_RESULT_ACCEPT {
			return false
		}
	}
	return true
}
func TestSnapshotIsolationRestoreAndRejections(t *testing.T) {
	s, _ := testState(t)
	s.Version = 11
	s.Height = 5000
	s.PriceEpochStart = 5000
	s.BlockHashes[1] = bytes.Repeat([]byte{7}, 32)
	donor := &Application{path: filepath.Join(t.TempDir(), "state"), committed: s}
	if e := SaveState(donor.path, s); e != nil {
		t.Fatal(e)
	}
	// A real persistent spool object larger than one transport chunk also exercises
	// order checks. Active protocol chunks are covered by the fixture test below.
	spool := s.chunkSpool()
	payload := bytes.Repeat([]byte("spool"), 300000)
	if e := spool.PutChunk("history-probe", 0, payload); e != nil {
		t.Fatal(e)
	}
	baselineState, e := s.blockCopy()
	if e != nil {
		t.Fatal(e)
	}
	baseline := &Application{committed: baselineState}
	if e = donor.ConfigureSnapshots(s.Parameters.ChainID, 1, 2); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(donor.WaitSnapshotBuild)
	ctx := context.Background()
	advance := func(app *Application, height int64) {
		t.Helper()
		if _, e := app.FinalizeBlock(ctx, &abci.FinalizeBlockRequest{Height: height}); e != nil {
			t.Fatal(e)
		}
		if _, e := app.Commit(ctx, &abci.CommitRequest{}); e != nil {
			t.Fatal(e)
		}
	}
	advance(donor, 5001)
	advance(baseline, 5001)
	snap := snapshotWait(t, donor)
	expected, _ := donor.committed.AppHash()
	baseHash, _ := baseline.committed.AppHash()
	if !bytes.Equal(expected, baseHash) {
		t.Fatal("snapshot changed app hash")
	}
	target := func() *Application {
		t.Helper()
		a, e := OpenApplication(filepath.Join(t.TempDir(), "state"))
		if e != nil {
			t.Fatal(e)
		}
		if e = a.ConfigureSnapshots(s.Parameters.ChainID, 0, 2); e != nil {
			t.Fatal(e)
		}
		return a
	}

	mismatch := target()
	badSnap := *snap
	var badMeta snapshotMetadata
	json.Unmarshal(snap.Metadata, &badMeta)
	badMeta.AppHash = bytes.Repeat([]byte{99}, 32)
	badSnap.Metadata, _ = json.Marshal(badMeta)
	if snapshotTransfer(t, donor, mismatch, &badSnap, badMeta.AppHash, nil) {
		t.Fatal("wrong expected app hash accepted")
	}
	if mismatch.committed != nil {
		t.Fatal("mismatch installed state")
	}
	restored := target()
	if !snapshotTransfer(t, donor, restored, snap, expected, nil) {
		t.Fatal("restore rejected")
	}
	if !bytes.Equal(restored.committed.blockHash(1), s.BlockHashes[1]) {
		t.Fatal("historical archive missing")
	}
	b, e := restored.committed.chunkSpool().GetChunk("history-probe", 0)
	if e != nil || !bytes.Equal(b, payload) {
		t.Fatal("spool missing", e)
	}
	restarted, e := OpenApplication(restored.path)
	if e != nil {
		t.Fatal(e)
	}
	for h := int64(5002); h <= 5022; h++ {
		advance(donor, h)
		advance(baseline, h)
		advance(restarted, h)
		a, _ := donor.committed.AppHash()
		b, _ := baseline.committed.AppHash()
		c, _ := restarted.committed.AppHash()
		if !bytes.Equal(a, b) || !bytes.Equal(a, c) {
			t.Fatalf("app hash differs at %d", h)
		}
		t.Logf("height=%d snapshot=baseline=restored app_hash=%x", h, a)
	}
	for name, mut := range map[string]func(uint32, []byte) (uint32, []byte){"corrupt": func(i uint32, b []byte) (uint32, []byte) { b[0] ^= 1; return i, b }, "out-of-order": func(i uint32, b []byte) (uint32, []byte) { return i + 1, b }} {
		t.Run(name, func(t *testing.T) {
			a := target()
			offer, _ := a.OfferSnapshot(ctx, &abci.OfferSnapshotRequest{Snapshot: snap, AppHash: expected})
			if offer.Result != abci.OFFER_SNAPSHOT_RESULT_ACCEPT {
				t.Fatal("offer")
			}
			i, b := mut(0, bytes.Repeat([]byte{0}, snapshotChunkSize))
			r, _ := a.ApplySnapshotChunk(ctx, &abci.ApplySnapshotChunkRequest{Index: i, Chunk: b})
			if r.Result != abci.APPLY_SNAPSHOT_CHUNK_RESULT_REJECT_SNAPSHOT {
				t.Fatal("accepted")
			}
			if a.committed != nil {
				t.Fatal("false height")
			}
			entries, _ := os.ReadDir(a.snapshots.root)
			if len(entries) != 0 {
				t.Fatal("restore not cleaned")
			}
		})
	}
	t.Log("archive and chunk reads succeeded; app hashes identical across snapshot and 21 following blocks")
}

func TestSnapshotRestoresActiveProtocolSpool(t *testing.T) {
	f, raw, a := chunkFixture(t)
	if e := f.s.submitMoeManifest(f.a["coordinator"].id, a, 3); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < a.ChunkTotal; i++ {
		if e := f.s.submitMoeChunk(f.a["coordinator"].id, chunkAction(raw, i), 3); e != nil {
			t.Fatal(e)
		}
	}
	f.s.Height = 3
	// Use the fixture's spool path, without changing consensus serialization.
	donor := &Application{path: strings.TrimSuffix(f.s.chunkSpoolRoot, ".chunk-spool"), committed: f.s}
	if e := SaveState(donor.path, f.s); e != nil {
		t.Fatal(e)
	}
	if e := donor.ConfigureSnapshots(f.s.Parameters.ChainID, 1, 2); e != nil {
		t.Fatal(e)
	}
	donor.pinSnapshot()
	t.Cleanup(donor.WaitSnapshotBuild)
	snap := snapshotWait(t, donor)
	hash, _ := f.s.AppHash()
	target, e := OpenApplication(filepath.Join(t.TempDir(), "state"))
	if e != nil {
		t.Fatal(e)
	}
	if e = target.ConfigureSnapshots(f.s.Parameters.ChainID, 0, 2); e != nil {
		t.Fatal(e)
	}
	if !snapshotTransfer(t, donor, target, snap, hash, nil) {
		t.Fatal("restore failed")
	}
	b, e := target.committed.chunkSpool().Reassemble(target.committed.Holds[a.HoldID])
	if e != nil || !bytes.Equal(b, raw) {
		t.Fatal("protocol receipt unavailable", e)
	}
	t.Logf("restored active hold %s: receipt bytes=%d, root verified", a.HoldID, len(b))
}

// Real archive deltas are generated by normal commits before either package
// is built. Restore to a fresh home, restart, and execute 21 identical blocks.
func TestSnapshotArchivePolicy(t *testing.T) {
	s, _ := testState(t)
	s.Version = 11
	s.Height = 5000
	s.PriceEpochStart = 5000
	s.BlockHashes[1] = bytes.Repeat([]byte{7}, 32)
	s.Holds["old-audit"] = &Hold{ID: "old-audit", State: "settled"}
	// A full hot window becomes archival during the following 21 blocks.
	for h := int64(3002); h <= 3022; h++ {
		s.BlockHashes[h] = bytes.Repeat([]byte{byte(h)}, 32)
	}
	donor := &Application{path: filepath.Join(t.TempDir(), "state"), committed: s}
	if err := SaveState(donor.path, s); err != nil {
		t.Fatal(err)
	}
	advance := func(a *Application, h int64) {
		t.Helper()
		if _, err := a.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: h, Hash: bytes.Repeat([]byte{byte(h)}, 32)}); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	advance(donor, 5001)
	entries, err := os.ReadDir(donor.path + ".archive")
	if err != nil || len(entries) == 0 {
		t.Fatal("no real archive", err)
	}
	// Add many valid immutable orphan deltas, as retained archive history does not
	// need to be reachable from the latest state. No synthetic tar entries.
	raw, err := os.ReadFile(filepath.Join(donor.path+".archive", entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		delta := append(append([]byte(nil), raw...), bytes.Repeat([]byte(" "), i+4096)...)
		if err := durableWrite(archivePath(donor.path, archiveKey(delta)), delta); err != nil {
			t.Fatal(err)
		}
	}
	if err := donor.committed.chunkSpool().PutChunk("policy-probe", 0, []byte("required spool")); err != nil {
		t.Fatal(err)
	}
	expected, _ := donor.committed.AppHash()
	var fullSize int64
	for _, included := range []bool{true, false} {
		if err := donor.ConfigureSnapshots(s.Parameters.ChainID, 1, 2, included); err != nil {
			t.Fatal(err)
		}
		donor.pinSnapshot()
		donor.WaitSnapshotBuild()
		snap := snapshotWait(t, donor)
		var meta snapshotMetadata
		if err := json.Unmarshal(snap.Metadata, &meta); err != nil {
			t.Fatal(err)
		}
		if meta.ArchiveIncluded == nil || *meta.ArchiveIncluded != included {
			t.Fatal("policy metadata")
		}
		var packageBytes bytes.Buffer
		for i := uint32(0); i < snap.Chunks; i++ {
			chunk, err := donor.LoadSnapshotChunk(context.Background(), &abci.LoadSnapshotChunkRequest{Height: snap.Height, Format: snap.Format, Chunk: i})
			if err != nil {
				t.Fatal(err)
			}
			packageBytes.Write(chunk.Chunk)
		}
		tr := tar.NewReader(&packageBytes)
		archiveEntries := 0
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(h.Name, "state.archive/") {
				archiveEntries++
			}
			if h.Name != "state" && !strings.HasPrefix(h.Name, "state.archive/") && !strings.HasPrefix(h.Name, "state.chunk-spool/") {
				t.Fatal(h.Name)
			}
		}
		if included {
			fullSize = meta.Size
			if archiveEntries == 0 {
				t.Fatal("full archive absent")
			}
		} else if archiveEntries != 0 || meta.Size*10 >= fullSize {
			t.Fatal("archive-free package too large", archiveEntries, meta.Size, fullSize)
		}
		target, err := OpenApplication(filepath.Join(t.TempDir(), "state"))
		if err != nil {
			t.Fatal(err)
		}
		if err = target.ConfigureSnapshots(s.Parameters.ChainID, 0, 2); err != nil {
			t.Fatal(err)
		}
		if !snapshotTransfer(t, donor, target, snap, expected, nil) {
			t.Fatal("restore")
		}
		if !included {
			entries, err := os.ReadDir(target.path + ".archive")
			if err != nil || len(entries) != 0 {
				t.Fatal("archive not empty", err)
			}
		}
		target, err = OpenApplication(target.path)
		if err != nil {
			t.Fatal("restart", err)
		}
		if got := target.committed.blockHash(1); included && !bytes.Equal(got, s.BlockHashes[1]) || !included && got != nil {
			t.Fatal("old archive result", got)
		}
		q, err := target.Query(context.Background(), &abci.QueryRequest{Path: "/hold/old-audit"})
		if err != nil || q.Code != 0 {
			t.Fatal("historical hold query", q, err)
		}
		if included && bytes.Equal(q.Value, []byte("null")) || !included && !bytes.Equal(q.Value, []byte("null")) {
			t.Fatal("historical hold query value", string(q.Value))
		}
		if !included {
			t.Logf("omitted /hold/old-audit: code=%d value=%s", q.Code, q.Value)
		}
		if b, err := target.committed.chunkSpool().GetChunk("policy-probe", 0); err != nil || string(b) != "required spool" {
			t.Fatal("spool", err)
		}
		baseState, err := LoadState(donor.path)
		if err != nil {
			t.Fatal(err)
		}
		baseline := &Application{committed: baseState}
		for h := int64(5002); h <= 5022; h++ {
			advance(baseline, h)
			advance(target, h)
			want, _ := baseline.committed.AppHash()
			got, _ := target.committed.AppHash()
			if !bytes.Equal(want, got) {
				t.Fatalf("hash at %d", h)
			}
			t.Logf("include_archive=%t height=%d app_hash=%x baseline=restored", included, h, got)
		}
		target, err = OpenApplication(target.path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(target.committed.blockHash(3002), s.BlockHashes[3002]) {
			t.Fatal("new archive unreadable")
		}
		t.Logf("include_archive=%t bytes=%d archive_entries=%d; restart and 21 blocks match; old missing hash returns nil", included, meta.Size, archiveEntries)
		// Remove ready package so the next policy builds the same height anew.
		if err = os.RemoveAll(donor.snapshots.root); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOmittedArchiveMarkerIsBoundToExactState(t *testing.T) {
	s, _ := testState(t)
	s.Version = 11
	path := filepath.Join(t.TempDir(), "state")
	if err := SaveState(path, s); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	key := archiveKey(raw)
	if err = os.Remove(archivePath(path, key)); err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"", strings.Repeat("a", 64)} {
		if err = durableWrite(path+".archive-omitted", []byte(marker)); err != nil {
			t.Fatal(err)
		}
		if _, err = LoadState(path); err == nil {
			t.Fatal("unrelated marker authorized missing archive")
		}
	}
	if err = durableWrite(path+".archive-omitted", []byte(key)); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadState(path); err != nil {
		t.Fatal(err)
	}
	// Even a valid marker must not authorize a missing parent in a real archive.
	if err = durableWrite(archivePath(path, key), []byte(`{"parent":"`+strings.Repeat("b", 64)+`"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadState(path); err == nil {
		t.Fatal("missing parent accepted")
	}
}
