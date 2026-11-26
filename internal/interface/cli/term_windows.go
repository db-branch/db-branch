//go:build windows

package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

func disableEcho(f *os.File) (func(), bool) {
	h := windows.Handle(f.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return nil, false
	}
	if err := windows.SetConsoleMode(h, mode&^windows.ENABLE_ECHO_INPUT); err != nil {
		return nil, false
	}
	return func() { _ = windows.SetConsoleMode(h, mode) }, true
}
