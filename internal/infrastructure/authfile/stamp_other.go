//go:build !unix

package authfile

import "io/fs"

func fileStampID(fs.FileInfo) string {
	return ""
}
