// Package fsutil holds small file helpers for directories the service user
// can write to while root writes into them.
package fsutil

import (
	"errors"
	"io/fs"
	"os"
)

// write is replaced in tests: a full disk cannot be had on demand.
var write = func(f *os.File, b []byte) (int, error) { return f.Write(b) }

// WriteFresh writes data to path as a new file, in one step: it is written
// to a file of its own, created exclusively so that a link planted by another
// user is never followed, and renamed over path (a link there is replaced, not
// followed).
func WriteFresh(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".new"
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, werr := write(f, data)
	if err := errors.Join(werr, f.Close()); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
