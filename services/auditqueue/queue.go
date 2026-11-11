package auditqueue

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Queue struct{ dir string }

func Open(dir string) (*Queue, error) {
	if dir == "" {
		return nil, errors.New("audit spool directory required")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	return &Queue{dir: dir}, nil
}

func validID(id string) bool {
	return id != "" && filepath.Base(id) == id && !strings.ContainsAny(id, `/\\`)
}

func (q *Queue) path(id string) (string, error) {
	if !validID(id) {
		return "", errors.New("invalid audit job id")
	}
	return filepath.Join(q.dir, id+".json"), nil
}

func (q *Queue) Put(id string, data []byte) error {
	target, err := q.path(id)
	if err != nil {
		return err
	}
	if _, err = os.Stat(target); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(q.dir, ".pending-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, target); err != nil {
		return err
	}
	d, err := os.Open(q.dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (q *Queue) Get(id string) ([]byte, error) {
	path, err := q.path(id)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func (q *Queue) Pending() ([]string, error) {
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			ids = append(ids, strings.TrimSuffix(entry.Name(), ".json"))
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func (q *Queue) Done(id string) error {
	path, err := q.path(id)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
