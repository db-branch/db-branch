package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
)

// ErrInvalidManifest is returned for a runtime module manifest that breaks the contract.
var ErrInvalidManifest = errors.New("invalid runtime manifest")

const (
	// ManifestRoot holds one directory per external engine, with its manifest.json.
	ManifestRoot = "/usr/lib/db-branch/runtimes"
	// ManifestSchemaVersion and SupportedRuntimeAPIVersion are the versions this core reads.
	ManifestSchemaVersion      = 1
	SupportedRuntimeAPIVersion = 1
	// RuntimeUserPrefix starts the OS user of an external engine: prefix + engine.
	RuntimeUserPrefix = "db-branch-rt-"
	// maxUserName is the longest login name Linux accepts.
	maxUserName = 32
)

// RuntimeManifest is the static declaration of an external engine
// (/usr/lib/db-branch/runtimes/<engine>/manifest.json).
type RuntimeManifest struct {
	SchemaVersion  int               `json:"schema_version"`
	Engine         string            `json:"engine"`
	DisplayName    string            `json:"display_name"`
	ModuleVersion  string            `json:"module_version"`
	APIVersion     int               `json:"api_version"`
	Binary         string            `json:"binary"`
	BinarySHA256   string            `json:"binary_sha256"`
	Catalog        string            `json:"catalog"`
	CatalogSHA256  string            `json:"catalog_sha256"`
	RunAsUser      string            `json:"run_as_user"`
	CollationBound bool              `json:"collation_bound"`
	ZFSProps       map[string]string `json:"zfs_props"`
	Protocols      []ManifestPort    `json:"protocols"`
	Capabilities   map[string]bool   `json:"capabilities"`
	Systemd        ManifestSystemd   `json:"systemd"`
}

// ManifestPort is one TCP port the engine needs.
type ManifestPort struct {
	Name    string `json:"name"`
	Primary bool   `json:"primary"`
}

// ManifestSystemd holds the unit settings that differ between engines.
type ManifestSystemd struct {
	ServiceType    string `json:"service_type"`
	KillSignal     string `json:"kill_signal"`
	KillMode       string `json:"kill_mode"`
	NotifyAccess   string `json:"notify_access"`
	TimeoutStopSec int    `json:"timeout_stop_sec"`
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ParseManifest reads a manifest strictly: unknown fields, trailing data, an
// engine of the core's own (mysql, postgres), another engine's user or
// directory, or a setting outside the allowed ones are all refused. Nothing a
// manifest says is trusted until it has passed here.
func ParseManifest(data []byte) (RuntimeManifest, error) {
	bad := func(format string, a ...any) (RuntimeManifest, error) {
		return RuntimeManifest{}, fmt.Errorf("%w: %s", ErrInvalidManifest, fmt.Sprintf(format, a...))
	}
	var m RuntimeManifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return bad("%v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return bad("data after the manifest")
	}
	if m.SchemaVersion != ManifestSchemaVersion {
		return bad("schema_version %d is not supported (want %d)", m.SchemaVersion, ManifestSchemaVersion)
	}
	if m.APIVersion != SupportedRuntimeAPIVersion {
		return bad("api_version %d is not supported (want %d)", m.APIVersion, SupportedRuntimeAPIVersion)
	}
	if !enginePattern.MatchString(m.Engine) {
		return bad("engine %q must match %s", m.Engine, enginePattern)
	}
	if e := Engine(m.Engine); e == MySQL || e == Postgres {
		return bad("engine %q is built into the core and cannot be replaced by a module", m.Engine)
	}
	if m.DisplayName == "" || len(m.DisplayName) > 64 || hasControl(m.DisplayName) {
		return bad("display_name must be 1 to 64 printable characters")
	}
	user := RuntimeUserPrefix + m.Engine
	if len(user) > maxUserName {
		return bad("engine %q is too long for its service user %q (at most %d characters)", m.Engine, user, maxUserName)
	}
	if m.RunAsUser != "" && m.RunAsUser != user {
		return bad("run_as_user %q: want %q or nothing", m.RunAsUser, user)
	}
	m.RunAsUser = user
	for name, p := range map[string]string{"binary": m.Binary, "catalog": m.Catalog} {
		if !underEngineDir(m.Engine, p) {
			return bad("%s %q must be a clean absolute path inside %s/%s/", name, p, ManifestRoot, m.Engine)
		}
	}
	for name, d := range map[string]string{"binary_sha256": m.BinarySHA256, "catalog_sha256": m.CatalogSHA256} {
		if !sha256Hex.MatchString(d) {
			return bad("%s must be 64 lowercase hex digits", name)
		}
	}
	if _, err := CapsFromFeatures(m.Capabilities); err != nil {
		return RuntimeManifest{}, err
	}
	unit := LaunchPlan{Command: "/x", ServiceType: m.Systemd.ServiceType, KillSignal: m.Systemd.KillSignal,
		KillMode: m.Systemd.KillMode, NotifyAccess: m.Systemd.NotifyAccess, TimeoutStopSec: m.Systemd.TimeoutStopSec}
	if err := unit.Validate(); err != nil {
		return bad("systemd: %v", err)
	}
	if _, err := m.Spec().validate(); err != nil {
		return bad("%v", err)
	}
	return m, nil
}

func underEngineDir(engine, p string) bool {
	dir := ManifestRoot + "/" + engine + "/"
	return p == path.Clean(p) && strings.HasPrefix(p, dir) && len(p) > len(dir) &&
		!hasControl(p) && !strings.ContainsAny(p, " \t")
}

// Spec is the declaration the manifest makes, for the registry.
func (m RuntimeManifest) Spec() EngineSpec {
	caps, _ := CapsFromFeatures(m.Capabilities) // ParseManifest has checked the keys
	ports := make([]ProtocolPort, len(m.Protocols))
	for i, p := range m.Protocols {
		ports[i] = ProtocolPort{Name: p.Name, Primary: p.Primary}
	}
	return EngineSpec{
		Engine: Engine(m.Engine), DisplayName: m.DisplayName, CollationBound: m.CollationBound,
		RunAsUser: m.RunAsUser, ZFSProps: m.ZFSProps, Protocols: ports, Capabilities: caps,
	}
}

// CapsFromFeatures turns the capability keys of a manifest into flags; an
// unknown key is an error (a typo must not silently switch a feature off).
func CapsFromFeatures(f map[string]bool) (RuntimeCaps, error) {
	known := map[string]bool{}
	for _, n := range FeatureNames {
		known[n] = true
	}
	for k := range f {
		if !known[k] {
			return RuntimeCaps{}, fmt.Errorf("%w: unknown capability %q", ErrInvalidManifest, k)
		}
	}
	return RuntimeCaps{
		Query: f["query"], Checkpoints: f["checkpoints"], LiveFreeze: f["live_freeze"], Upgrade: f["upgrade"],
		SourceDump: f["source_dump"], BranchMerge: f["branch_merge"], HistoryRecord: f["history_record"],
		UpdateStats: f["update_stats"], InstanceMetrics: f["instance_metrics"],
	}, nil
}
