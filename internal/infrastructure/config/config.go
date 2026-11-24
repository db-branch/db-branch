// Package config loads the agent configuration from /etc/db-branch/config.toml.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// DefaultPath is where the Debian package installs the configuration file.
const DefaultPath = "/etc/db-branch/config.toml"

// Config is the agent configuration.
type Config struct {
	// Pool is the ZFS pool db-branch manages. Empty until `db-branch init`.
	Pool string `toml:"pool"`
	// Storage names the module whose storage replaces the ZFS pool (e.g.
	// "fsx" for Amazon FSx for OpenZFS). Empty: the node's ZFS pool.
	Storage string `toml:"storage"`
	// DataDir holds runtimes, artifacts and local state.
	DataDir string `toml:"data_dir"`
	// Socket is the UNIX socket the local CLI uses to talk to the agent.
	Socket string `toml:"socket"`
	// Advertise is the host clients use to reach this node (DSNs, console).
	// Empty means the first non-loopback IPv4 address.
	Advertise string `toml:"advertise"`
	// Bind is the address database servers listen on.
	Bind string `toml:"bind"`
	// AllowUnsignedRuntimes lets runtime modules without a valid signature
	// load (for development and testing only; a warning is shown). A bad
	// signature is never accepted.
	AllowUnsignedRuntimes bool    `toml:"allow_unsigned_runtimes"`
	Ports                 Ports   `toml:"ports"`
	Branch                Branch  `toml:"branch"`
	ZFS                   ZFS     `toml:"zfs"`
	Console               Console `toml:"console"`
	Notify                Notify  `toml:"notify"`
}

// Notify configures outbound notifications (Slack, webhooks).
type Notify struct {
	// AllowPrivateIPs permits notification targets on private network ranges
	// (RFC 1918 and IPv6 ULA). Loopback and link-local addresses (including
	// cloud metadata endpoints) are always refused.
	AllowPrivateIPs bool `toml:"allow_private_ips"`
}

// Ports is the inclusive TCP port range handed to bases and branches.
type Ports struct {
	Min int `toml:"min"`
	Max int `toml:"max"`
}

// ZFS tunes the pool.
type ZFS struct {
	// ArcMaxMB caps the ARC; 0 means a quarter of physical memory.
	ArcMaxMB uint64 `toml:"arc_max_mb"`
}

// Branch holds per-branch defaults.
type Branch struct {
	// MemoryMB sizes the engine's own cache. Small on purpose: branches share
	// base blocks through the ZFS ARC.
	MemoryMB int `toml:"memory_mb"`
}

// Console configures the web console.
type Console struct {
	Listen  string `toml:"listen"`
	TLSCert string `toml:"tls_cert"`
	TLSKey  string `toml:"tls_key"`
}

// Default returns the built-in configuration.
func Default() Config {
	return Config{
		DataDir: "/var/lib/db-branch",
		Socket:  "/run/db-branch/agent.sock",
		Bind:    "0.0.0.0",
		Ports:   Ports{Min: 40000, Max: 40999},
		Branch:  Branch{MemoryMB: 128},
		Console: Console{Listen: ":7080"},
	}
}

// Load reads path on top of the defaults. A missing file is not an error.
// Unknown keys are rejected so that typos do not silently fall back to defaults.
func Load(path string) (Config, error) {
	cfg := Default()

	body, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}

	md, err := toml.Decode(string(body), &cfg)
	if err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return Config{}, fmt.Errorf("config %s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	if cfg.Ports.Min > cfg.Ports.Max {
		return Config{}, fmt.Errorf("config %s: ports.min %d is above ports.max %d", path, cfg.Ports.Min, cfg.Ports.Max)
	}
	if (cfg.Console.TLSCert == "") != (cfg.Console.TLSKey == "") {
		return Config{}, fmt.Errorf("config %s: console.tls_cert and console.tls_key must both be set", path)
	}
	return cfg, nil
}
