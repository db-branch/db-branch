// Package backupdir keeps backup files in a directory only root reads.
package backupdir

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/db-branch/db-branch/internal/domain"
)

// Dir implements backup.Dir.
type Dir struct{ Path string }

// file renames its temporary file into place when closed, so a backup is
// never seen half written.
type file struct {
	*os.File
	final string
}

func (f file) Close() error {
	if err := f.File.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), f.final)
}

func (d Dir) Create(name string) (io.WriteCloser, error) {
	if err := os.MkdirAll(d.Path, 0o700); err != nil {
		return nil, err
	}
	_ = os.Chmod(d.Path, 0o700)
	f, err := os.OpenFile(filepath.Join(d.Path, "."+filepath.Base(name)+".tmp"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return file{File: f, final: filepath.Join(d.Path, filepath.Base(name))}, nil
}

// List returns the backups (*.json.gz), newest first.
func (d Dir) List() ([]domain.BackupFile, error) {
	entries, err := os.ReadDir(d.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []domain.BackupFile
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".json.gz") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if fi, err := e.Info(); err == nil {
			out = append(out, domain.BackupFile{Name: e.Name(), Size: fi.Size(), TakenAt: fi.ModTime().UTC()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TakenAt.After(out[j].TakenAt) })
	return out, nil
}

func (d Dir) Remove(name string) error { return os.Remove(filepath.Join(d.Path, filepath.Base(name))) }

func (d Dir) Open(name string) (io.ReadCloser, error) {
	if name != filepath.Base(name) {
		return nil, fmt.Errorf("backup %q: %w", name, domain.ErrNotFound)
	}
	f, err := os.Open(filepath.Join(d.Path, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("backup %s is not on this node: %w", name, domain.ErrNotFound)
	}
	return f, err
}
