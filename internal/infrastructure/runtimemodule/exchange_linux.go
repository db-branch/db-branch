//go:build linux

package runtimemodule

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

func renameExchange(a, b string) error {
	err := unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE)
	if errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EINVAL) {
		return ErrExchangeUnsupported
	}
	return err
}
