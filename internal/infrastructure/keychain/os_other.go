//go:build !darwin && !linux && !windows

package keychain

type noopBackend struct{}

func newOSBackend() Backend { return noopBackend{} }

func (noopBackend) Name() string             { return "none" }
func (noopBackend) Available() bool          { return false }
func (noopBackend) Save(string, string) error { return ErrNotFound }
func (noopBackend) Load(string) (string, error) { return "", ErrNotFound }
func (noopBackend) Delete(string) error       { return ErrNotFound }
