// Package uploads keeps dump files sent through the API on the node's disk
// until a base is created or loaded from them.
package uploads

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

// Store implements domain.UploadStore in a directory.
type Store struct {
	Dir string
	// MaxBytes is the largest upload accepted.
	MaxBytes func() int64
	// Reserve is disk space left free after an upload.
	Reserve uint64
	// Free reports the free bytes of the file system holding dir.
	Free  func(dir string) (uint64, error)
	Clock func() time.Time
}

// DefaultReserve keeps 1 GiB free for the rest of the node.
const DefaultReserve = 1 << 30

var extensions = []string{".sql.gz", ".sql"}

func extension(name string) (string, bool) {
	lower := strings.ToLower(name)
	for _, ext := range extensions {
		if strings.HasSuffix(lower, ext) && len(lower) > len(ext) {
			return ext, true
		}
	}
	return "", false
}

// Save streams exactly size bytes into a new upload. A partial or oversized
// body leaves nothing behind.
func (s *Store) Save(r io.Reader, name string, size int64) (domain.Upload, error) {
	ext, ok := extension(name)
	if !ok {
		return domain.Upload{}, fmt.Errorf("%q: a dump must be a .sql or .sql.gz file", name)
	}
	if size <= 0 {
		return domain.Upload{}, errors.New("the upload is empty (or its size is unknown)")
	}
	if max := s.MaxBytes(); size > max {
		return domain.Upload{}, fmt.Errorf("%w: %d bytes; the limit is %d (setting upload.max_mb)", domain.ErrTooLarge, size, max)
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return domain.Upload{}, err
	}
	free, err := s.Free(s.Dir)
	if err != nil {
		return domain.Upload{}, err
	}
	if uint64(size)+s.Reserve > free {
		return domain.Upload{}, fmt.Errorf("%w: %d bytes to store, %d free in %s (keeping %d)", domain.ErrNoSpace, size, free, s.Dir, s.Reserve)
	}
	id := newID()
	part := filepath.Join(s.Dir, id+".part")
	path := filepath.Join(s.Dir, id+ext)
	err = write(part, r, size)
	if err == nil {
		err = os.Rename(part, path)
	}
	if err != nil {
		_ = os.Remove(part)
		return domain.Upload{}, err
	}
	return domain.Upload{ID: id, Name: filepath.Base(name), Size: size, Path: path, CreatedAt: s.Clock()}, nil
}

func write(path string, r io.Reader, size int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, err := io.CopyN(f, r, size)
	if err != nil {
		f.Close()
		return fmt.Errorf("upload ended after %d of %d bytes: %w", n, size, err)
	}
	if extra, _ := r.Read(make([]byte, 1)); extra > 0 {
		f.Close()
		return fmt.Errorf("the upload is longer than its announced %d bytes", size)
	}
	return f.Close()
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

// Room is the largest upload accepted now: the size limit, or the disk
// space above the reserve if smaller.
func (s *Store) Room() (int64, error) {
	free, err := s.Free(s.Dir)
	if err != nil {
		return 0, err
	}
	room := int64(0)
	if free > s.Reserve {
		room = int64(free - s.Reserve)
	}
	return min(room, s.MaxBytes()), nil
}

// Get finds an upload by ID.
func (s *Store) Get(id string) (domain.Upload, error) {
	if validID(id) {
		for _, ext := range extensions {
			path := filepath.Join(s.Dir, id+ext)
			if fi, err := os.Stat(path); err == nil {
				return domain.Upload{ID: id, Name: filepath.Base(path), Size: fi.Size(), Path: path, CreatedAt: fi.ModTime()}, nil
			}
		}
	}
	return domain.Upload{}, fmt.Errorf("upload %q on this node: %w", id, domain.ErrNotFound)
}

// Remove deletes an upload; a missing one is not an error.
func (s *Store) Remove(id string) error {
	u, err := s.Get(id)
	if err != nil {
		return nil
	}
	return os.Remove(u.Path)
}

// Sweep deletes uploads (and interrupted ones) older than maxAge and
// returns how many.
func (s *Store) Sweep(maxAge time.Duration) int {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil || s.Clock().Sub(fi.ModTime()) <= maxAge {
			continue
		}
		if os.Remove(filepath.Join(s.Dir, e.Name())) == nil {
			n++
		}
	}
	return n
}

// DiskFree returns the bytes available to unprivileged users in the file
// system holding dir (or its nearest existing parent).
func DiskFree(dir string) (uint64, error) {
	var st syscall.Statfs_t
	err := syscall.Statfs(dir, &st)
	for err != nil && filepath.Dir(dir) != dir {
		dir = filepath.Dir(dir)
		err = syscall.Statfs(dir, &st)
	}
	return uint64(st.Bavail) * uint64(st.Bsize), err
}
