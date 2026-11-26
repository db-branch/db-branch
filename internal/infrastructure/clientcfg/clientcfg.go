// Package clientcfg manages the client CLI configuration file (client.toml).
package clientcfg

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"
)

// Profile stores connection and TLS pinning settings for one server.
type Profile struct {
	URL          string `toml:"url"`
	CACert       string `toml:"ca_cert,omitempty"`
	TLSSHA256    string `toml:"tls_sha256,omitempty"`
	TokenStorage string `toml:"token_storage,omitempty"`
}

// Config is the persisted client CLI configuration.
type Config struct {
	CurrentProfile string             `toml:"current_profile,omitempty"`
	Profiles       map[string]Profile `toml:"profiles,omitempty"`
}

var profileNamePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)

// ValidateProfileName ensures profile names contain only safe characters.
func ValidateProfileName(name string) error {
	if name == "" {
		return errors.New("profile name cannot be empty")
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("invalid profile name %q: cannot start with '.'", name)
	}
	if !profileNamePattern.MatchString(name) {
		return fmt.Errorf("invalid profile name %q: use up to 64 characters of [a-zA-Z0-9._-]", name)
	}
	return nil
}

// DefaultPath returns the default client.toml path for the current OS.
func DefaultPath() (string, error) {
	return PathForOS(runtime.GOOS, os.Getenv, os.UserHomeDir)
}

// ResolvePath returns path when non-empty, or DefaultPath otherwise.
func ResolvePath(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	return DefaultPath()
}

// PathForOS resolves client.toml for goos using getenv and homeDir.
func PathForOS(goos string, getenv func(string) string, homeDir func() (string, error)) (string, error) {
	if goos == "windows" {
		base := getenv("AppData")
		if base == "" {
			home, err := homeDir()
			if err != nil {
				return "", fmt.Errorf("resolve home directory: %w", err)
			}
			base = filepath.Join(home, "AppData", "Roaming")
		}
		return filepath.Join(base, "db-branch", "client.toml"), nil
	}
	base := getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := homeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "db-branch", "client.toml"), nil
}

// CredentialsPath returns the fallback 0600 credentials file path alongside configPath.
func CredentialsPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "credentials.toml")
}

// Load reads client.toml from path. A missing file yields an empty Config.
func Load(path string) (Config, error) {
	cfg := Config{Profiles: map[string]Profile{}}
	body, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read client config %s: %w", path, err)
	}
	md, err := toml.Decode(string(body), &cfg)
	if err != nil {
		return Config{}, fmt.Errorf("parse client config %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return Config{}, fmt.Errorf("client config %s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]Profile{}
	}
	for name := range cfg.Profiles {
		if err := ValidateProfileName(name); err != nil {
			return Config{}, fmt.Errorf("client config %s: %w", path, err)
		}
	}
	return cfg, nil
}

// Save writes cfg atomically to path with 0600 permissions.
func Save(path string, cfg Config) error {
	for name := range cfg.Profiles {
		if err := ValidateProfileName(name); err != nil {
			return err
		}
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		return fmt.Errorf("encode client config: %w", err)
	}
	return WriteFileAtomic(path, buf.Bytes())
}

// SetProfile validates name, inserts or updates p, and optionally selects it as CurrentProfile.
func (c *Config) SetProfile(name string, p Profile, makeCurrent bool) error {
	if err := ValidateProfileName(name); err != nil {
		return err
	}
	if c.Profiles == nil {
		c.Profiles = map[string]Profile{}
	}
	c.Profiles[name] = p
	if makeCurrent || c.CurrentProfile == "" {
		c.CurrentProfile = name
	}
	return nil
}

// UseProfile switches CurrentProfile to an existing profile name.
func (c *Config) UseProfile(name string) error {
	if err := ValidateProfileName(name); err != nil {
		return err
	}
	if _, ok := c.Profiles[name]; !ok {
		return fmt.Errorf("profile %q does not exist", name)
	}
	c.CurrentProfile = name
	return nil
}

// RemoveProfile deletes name from Profiles and updates CurrentProfile if needed.
func (c *Config) RemoveProfile(name string) error {
	if err := ValidateProfileName(name); err != nil {
		return err
	}
	if _, ok := c.Profiles[name]; !ok {
		return fmt.Errorf("profile %q does not exist", name)
	}
	delete(c.Profiles, name)
	if c.CurrentProfile == name {
		c.CurrentProfile = ""
	}
	return nil
}

// WriteFileAtomic writes data to path with 0600 permissions via a temporary file.
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	_ = tmp.Close()
	if err := os.WriteFile(tmpName, data, 0o600); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}
