package fsutil

import "os"

// SetWrite replaces the write of WriteFresh and returns a function that restores it.
func SetWrite(w func(*os.File, []byte) (int, error)) (restore func()) {
	old := write
	write = w
	return func() { write = old }
}
