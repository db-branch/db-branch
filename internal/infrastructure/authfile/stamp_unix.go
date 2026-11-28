//go:build unix

package authfile

import (
	"fmt"
	"io/fs"
	"syscall"
)

func fileStampID(info fs.FileInfo) string {
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st != nil {
		return fmt.Sprintf("%d:%d", st.Dev, st.Ino)
	}
	return ""
}
