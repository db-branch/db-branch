//go:build darwin

package cli

import (
	"os"

	"golang.org/x/sys/unix"
)

func disableEcho(f *os.File) (func(), bool) {
	fd := int(f.Fd())
	oldState, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil {
		return nil, false
	}
	newState := *oldState
	newState.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, unix.TIOCSETA, &newState); err != nil {
		return nil, false
	}
	return func() { _ = unix.IoctlSetTermios(fd, unix.TIOCSETA, oldState) }, true
}
