// Package keychain stores API tokens in the OS keychain with a 0600 file fallback.
package keychain

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/db-branch/db-branch/internal/infrastructure/clientcfg"
)

const serviceName = "db-branch"

// ErrNotFound is returned when no token exists for the requested profile.
var ErrNotFound = errors.New("token not found")

// Backend persists tokens for named profiles.
type Backend interface {
	Name() string
	Available() bool
	Save(profile, token string) error
	Load(profile string) (string, error)
	Delete(profile string) error
}

// Store prefers the OS keychain and falls back to a 0600 credentials file.
type Store struct {
	osBackend   Backend
	fileBackend Backend
}

// New returns a Store using the platform OS keychain and credPath as fallback.
func New(credPath string) *Store {
	return NewWithBackends(newOSBackend(), NewFileBackend(credPath))
}

// NewWithBackends builds a Store with explicit backends (for tests).
func NewWithBackends(osBackend, fileBackend Backend) *Store {
	return &Store{osBackend: osBackend, fileBackend: fileBackend}
}

// Save stores token for profile, returning ("keychain", "", nil) or
// ("file", warning, nil) when falling back to the 0600 credentials file.
func (s *Store) Save(profile, token string) (string, string, error) {
	if err := validateTokenInput(profile, token); err != nil {
		return "", "", err
	}
	if s.osBackend != nil && s.osBackend.Available() {
		if err := s.osBackend.Save(profile, token); err == nil {
			_ = s.fileBackend.Delete(profile)
			return "keychain", "", nil
		}
	}
	if err := s.fileBackend.Save(profile, token); err != nil {
		return "", "", err
	}
	warn := fmt.Sprintf("OS keychain unavailable; saved token with 0600 permissions in %s", s.fileBackend.Name())
	return "file", warn, nil
}

// Load retrieves the token for profile from the OS keychain or fallback file.
func (s *Store) Load(profile string) (string, error) {
	if err := clientcfg.ValidateProfileName(profile); err != nil {
		return "", err
	}
	if s.osBackend != nil && s.osBackend.Available() {
		tok, err := s.osBackend.Load(profile)
		if err == nil {
			return tok, nil
		}
		if !errors.Is(err, ErrNotFound) {
			// Try fallback file before failing in case token was saved there.
			if ftok, ferr := s.fileBackend.Load(profile); ferr == nil {
				return ftok, nil
			}
			return "", err
		}
	}
	return s.fileBackend.Load(profile)
}

// Delete removes any token for profile from both the OS keychain and fallback file.
func (s *Store) Delete(profile string) error {
	if err := clientcfg.ValidateProfileName(profile); err != nil {
		return err
	}
	var errs []error
	if s.osBackend != nil && s.osBackend.Available() {
		if err := s.osBackend.Delete(profile); err != nil && !errors.Is(err, ErrNotFound) {
			errs = append(errs, err)
		}
	}
	if err := s.fileBackend.Delete(profile); err != nil && !errors.Is(err, ErrNotFound) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func validateTokenInput(profile, token string) error {
	if err := clientcfg.ValidateProfileName(profile); err != nil {
		return err
	}
	if token == "" {
		return errors.New("token cannot be empty")
	}
	for i := 0; i < len(token); i++ {
		if token[i] < 0x20 || token[i] == 0x7f {
			return errors.New("token cannot contain control characters")
		}
	}
	return nil
}

type credentialsFile struct {
	Tokens map[string]string `toml:"tokens,omitempty"`
}

// FileBackend stores tokens in a 0600 TOML file.
type FileBackend struct {
	path string
}

// NewFileBackend creates a FileBackend at path.
func NewFileBackend(path string) *FileBackend {
	return &FileBackend{path: path}
}

func (f *FileBackend) Name() string    { return f.path }
func (f *FileBackend) Available() bool { return true }

func (f *FileBackend) readAll() (credentialsFile, error) {
	cf := credentialsFile{Tokens: map[string]string{}}
	raw, err := os.ReadFile(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return cf, nil
	}
	if err != nil {
		return cf, err
	}
	if _, err := toml.Decode(string(raw), &cf); err != nil {
		return cf, fmt.Errorf("parse credentials %s: %w", f.path, err)
	}
	if cf.Tokens == nil {
		cf.Tokens = map[string]string{}
	}
	return cf, nil
}

func (f *FileBackend) Save(profile, token string) error {
	if err := validateTokenInput(profile, token); err != nil {
		return err
	}
	cf, err := f.readAll()
	if err != nil {
		return err
	}
	cf.Tokens[profile] = token
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(cf); err != nil {
		return err
	}
	return clientcfg.WriteFileAtomic(f.path, buf.Bytes())
}

func (f *FileBackend) Load(profile string) (string, error) {
	if err := clientcfg.ValidateProfileName(profile); err != nil {
		return "", err
	}
	cf, err := f.readAll()
	if err != nil {
		return "", err
	}
	tok, ok := cf.Tokens[profile]
	if !ok || tok == "" {
		return "", ErrNotFound
	}
	return tok, nil
}

func (f *FileBackend) Delete(profile string) error {
	if err := clientcfg.ValidateProfileName(profile); err != nil {
		return err
	}
	cf, err := f.readAll()
	if err != nil {
		return err
	}
	if _, ok := cf.Tokens[profile]; !ok {
		return ErrNotFound
	}
	delete(cf.Tokens, profile)
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(cf); err != nil {
		return err
	}
	return clientcfg.WriteFileAtomic(f.path, buf.Bytes())
}

type cmdRunner func(name, stdin string, args ...string) (stdout, stderr string, err error)

// ExecCmdRunnerForTest exposes execCmdRunner for unit tests.
func ExecCmdRunnerForTest(name, stdin string, args ...string) (string, string, error) {
	return execCmdRunner(name, stdin, args...)
}

func execCmdRunner(name, stdin string, args ...string) (string, string, error) {
	cmd := exec.Command(name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	return out.String(), errOut.String(), err
}
