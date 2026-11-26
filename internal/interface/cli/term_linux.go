//go:build linux

package cli

import (
	"os"

	"golang.org/x/sys/unix"
)

func disableEcho(f *os.File) (func(), bool) {
	fd := int(f.Fd())
	oldState, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, false
	}
	newState := *oldState
	newState.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &newState); err != nil {
		return nil, false
	}
	return func() { _ = unix.IoctlSetTermios(fd, unix.TCSETS, oldState) }, true
}
