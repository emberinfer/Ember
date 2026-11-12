package auditqueue

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPersistentQueue(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	q, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = q.Put("request-1", []byte(`{"request":"1"}`)); err != nil {
		t.Fatal(err)
	}
	if err = q.Put("request-1", []byte("changed")); err != nil {
		t.Fatal(err)
	}
	q, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := q.Pending()
	if err != nil || len(ids) != 1 || ids[0] != "request-1" {
		t.Fatalf("pending=%v %v", ids, err)
	}
	data, err := q.Get(ids[0])
	if err != nil || string(data) != `{"request":"1"}` {
		t.Fatalf("data=%q %v", data, err)
	}
	if mode := fileMode(t, dir); mode != 0700 {
		t.Fatalf("dir mode=%o", mode)
	}
	if mode := fileMode(t, filepath.Join(dir, "request-1.json")); mode != 0600 {
		t.Fatalf("file mode=%o", mode)
	}
	if err = q.Done(ids[0]); err != nil {
		t.Fatal(err)
	}
	if ids, err = q.Pending(); err != nil || len(ids) != 0 {
		t.Fatalf("pending=%v %v", ids, err)
	}
	if err = q.Put("../escape", nil); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
