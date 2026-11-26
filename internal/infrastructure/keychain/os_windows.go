//go:build windows

package keychain

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	credTypeGeneric         uint32 = 1
	credPersistLocalMachine uint32 = 2
	errNotFoundWin32               = windows.Errno(1168)
)

var (
	advapi32        = windows.NewLazySystemDLL("advapi32.dll")
	procCredWriteW  = advapi32.NewProc("CredWriteW")
	procCredReadW   = advapi32.NewProc("CredReadW")
	procCredDeleteW = advapi32.NewProc("CredDeleteW")
	procCredFree    = advapi32.NewProc("CredFree")
)

type winCredential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        windows.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

// WinCredOps abstracts Win32 Credential Manager calls for unit testing.
type WinCredOps interface {
	Available() bool
	Write(target, user string, blob []byte) error
	Read(target string) ([]byte, error)
	Delete(target string) error
}

type nativeWinCredOps struct{}

func (nativeWinCredOps) Available() bool {
	return procCredWriteW.Find() == nil &&
		procCredReadW.Find() == nil &&
		procCredDeleteW.Find() == nil &&
		procCredFree.Find() == nil
}

func (nativeWinCredOps) Write(target, user string, blob []byte) error {
	tPtr, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	uPtr, err := windows.UTF16PtrFromString(user)
	if err != nil {
		return err
	}
	cred := winCredential{
		Type:               credTypeGeneric,
		TargetName:         tPtr,
		CredentialBlobSize: uint32(len(blob)),
		CredentialBlob:     &blob[0],
		Persist:            credPersistLocalMachine,
		UserName:           uPtr,
	}
	r1, _, callErr := procCredWriteW.Call(uintptr(unsafe.Pointer(&cred)), 0)
	if r1 == 0 {
		return fmt.Errorf("CredWriteW: %w", callErr)
	}
	return nil
}

func (nativeWinCredOps) Read(target string) ([]byte, error) {
	tPtr, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return nil, err
	}
	var cred *winCredential
	r1, _, callErr := procCredReadW.Call(uintptr(unsafe.Pointer(tPtr)), uintptr(credTypeGeneric), 0, uintptr(unsafe.Pointer(&cred)))
	if r1 == 0 {
		if callErr == errNotFoundWin32 {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("CredReadW: %w", callErr)
	}
	defer func() {
		_, _, _ = procCredFree.Call(uintptr(unsafe.Pointer(cred)))
	}()
	if cred.CredentialBlobSize == 0 || cred.CredentialBlob == nil {
		return nil, ErrNotFound
	}
	return append([]byte(nil), unsafe.Slice(cred.CredentialBlob, cred.CredentialBlobSize)...), nil
}

func (nativeWinCredOps) Delete(target string) error {
	tPtr, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	r1, _, callErr := procCredDeleteW.Call(uintptr(unsafe.Pointer(tPtr)), uintptr(credTypeGeneric), 0)
	if r1 == 0 {
		if callErr == errNotFoundWin32 {
			return ErrNotFound
		}
		return fmt.Errorf("CredDeleteW: %w", callErr)
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
