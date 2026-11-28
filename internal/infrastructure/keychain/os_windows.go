//go:build windows

package keychain

import (
	"errors"
	"fmt"

	"github.com/danieljoos/wincred"
	"golang.org/x/sys/windows"
)

// WinCredOps abstracts Win32 Credential Manager calls for unit testing.
type WinCredOps interface {
	Available() bool
	Write(target, user string, blob []byte) error
	Read(target string) ([]byte, error)
	Delete(target string) error
}

type nativeWinCredOps struct{}

var advapi32 = windows.NewLazySystemDLL("advapi32.dll")

func (nativeWinCredOps) Available() bool {
	return advapi32.NewProc("CredWriteW").Find() == nil &&
		advapi32.NewProc("CredReadW").Find() == nil &&
		advapi32.NewProc("CredDeleteW").Find() == nil &&
		advapi32.NewProc("CredFree").Find() == nil
}

func (nativeWinCredOps) Write(target, user string, blob []byte) error {
	cred := wincred.NewGenericCredential(target)
	cred.UserName = user
	cred.Persist = wincred.PersistLocalMachine
	cred.CredentialBlob = append([]byte(nil), blob...)
	if err := cred.Write(); err != nil {
		return fmt.Errorf("CredWriteW: %w", err)
	}
	return nil
}

func (nativeWinCredOps) Read(target string) ([]byte, error) {
	cred, err := wincred.GetGenericCredential(target)
	if err != nil {
		if errors.Is(err, wincred.ErrElementNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("CredReadW: %w", err)
	}
	if len(cred.CredentialBlob) == 0 {
		return nil, ErrNotFound
	}
	return append([]byte(nil), cred.CredentialBlob...), nil
}

func (nativeWinCredOps) Delete(target string) error {
	cred := wincred.NewGenericCredential(target)
	if err := cred.Delete(); err != nil {
		if errors.Is(err, wincred.ErrElementNotFound) {
			return ErrNotFound
		}
		return fmt.Errorf("CredDeleteW: %w", err)
	}
	return nil
}

type windowsBackend struct{ ops WinCredOps }

func newOSBackend() Backend { return &windowsBackend{ops: nativeWinCredOps{}} }

// NewWindowsBackendForTest constructs a windowsBackend with injectable WinCredOps.
func NewWindowsBackendForTest(ops WinCredOps) Backend { return &windowsBackend{ops: ops} }

func (w *windowsBackend) Name() string    { return "Windows Credential Manager" }
func (w *windowsBackend) Available() bool { return w.ops.Available() }

func winTarget(profile string) string { return serviceName + ":" + profile }

func (w *windowsBackend) Save(profile, token string) error {
	if err := validateTokenInput(profile, token); err != nil {
		return err
	}
	return w.ops.Write(winTarget(profile), profile, []byte(token))
}

func (w *windowsBackend) Load(profile string) (string, error) {
	b, err := w.ops.Read(winTarget(profile))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (w *windowsBackend) Delete(profile string) error {
	return w.ops.Delete(winTarget(profile))
}
