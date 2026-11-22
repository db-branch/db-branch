// Package fsutil holds small file helpers for directories the service user
// can write to while root writes into them.
package fsutil

import (
	"errors"
	"io/fs"
	"os"
)

// WriteFresh writes data to path as a new file: whatever is at path (a link
// included) is removed first, and the file is created exclusively, so a link
// planted by another user is never followed.
func WriteFresh(path string, data []byte, mode os.FileMode) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
