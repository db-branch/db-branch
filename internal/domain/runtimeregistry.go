package domain

import (
	"errors"
	"fmt"
	"regexp"
	"regexp/syntax"
	"sort"
	"strings"
	"sync"
)

// ErrInvalidEngineSpec is returned when registering an engine declaration that breaks the contract.
var ErrInvalidEngineSpec = errors.New("invalid engine spec")

// ProtocolPort is one TCP port an engine needs (e.g. ClickHouse's "http" and "native").
type ProtocolPort struct {
	Name string
	// Primary marks the port that stands for the instance (Branch.Port).
	Primary bool
}

// EngineSpec declares what one database engine is and needs.
type EngineSpec struct {
	// Engine matches enginePattern, e.g. "mysql", "clickhouse".
	Engine      Engine
	DisplayName string
	// VersionPattern is the regular expression a version must match; empty: the standard one.
	VersionPattern string
	// CollationBound: the data depends on the OS's collation libraries, so
	// copies stay on nodes of the same distribution (see CollationBound).
	CollationBound bool
	// RunAsUser is the OS user the engine runs as; empty: the default one.
	RunAsUser string
	// ZFSProps are recommended ZFS properties for datasets holding the engine's data.
	ZFSProps map[string]string
	// Protocols are the TCP ports an instance needs; when set, exactly one is primary.
	Protocols    []ProtocolPort
	Capabilities RuntimeCaps
	// CheckUpgradePath, when set, vets a move between two versions of the
	// engine (same engine, from older to newer is already ensured).
	CheckUpgradePath func(from, to RuntimeRef) error
	// FormatDSN, when set, writes a client URL; nil: scheme://user:password@host:port/.
	FormatDSN func(host string, port int, user, password string) string

	versionRE *regexp.Regexp
}

// Clone copies the map and slice, so neither side can change the other's.
func (s EngineSpec) Clone() EngineSpec {
	if s.ZFSProps != nil {
		props := make(map[string]string, len(s.ZFSProps))
		for k, v := range s.ZFSProps {
			props[k] = v
		}
		s.ZFSProps = props
	}
	s.Protocols = append([]ProtocolPort(nil), s.Protocols...)
	return s
}

var enginePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// allowedZFSProps are dataset performance-tuning properties an engine spec may set.
var allowedZFSProps = map[string]bool{
	"recordsize":           true,
	"compression":          true,
	"atime":                true,
	"relatime":             true,
	"xattr":                true,
	"logbias":              true,
	"sync":                 true,
	"primarycache":         true,
	"secondarycache":       true,
	"redundant_metadata":   true,
	"dnodesize":            true,
	"special_small_blocks": true,
}

var zfsPropValuePattern = regexp.MustCompile(`^[A-Za-z0-9_.][A-Za-z0-9_.-]*$`)

func (s EngineSpec) validate() (*regexp.Regexp, error) {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: engine %q: %s", ErrInvalidEngineSpec, s.Engine, fmt.Sprintf(format, a...))
	}
	if !enginePattern.MatchString(string(s.Engine)) {
		return nil, bad("name must match %s", enginePattern)
	}
	if s.RunAsUser == "root" {
		return nil, bad("must not run as root")
	}
	for k, v := range s.ZFSProps {
		if !allowedZFSProps[k] {
			return nil, bad("ZFS property %q is not allowed", k)
		}
		if !zfsPropValuePattern.MatchString(v) {
			return nil, bad("ZFS property %q has invalid value %q", k, v)
		}
	}
	if len(s.Protocols) > 0 {
		primaries := 0
		seen := map[string]bool{}
		for _, p := range s.Protocols {
			if p.Name == "" || seen[p.Name] {
				return nil, bad("protocol names must be set and unique")
			}
			seen[p.Name] = true
			if p.Primary {
				primaries++
			}
		}
		if primaries != 1 {
			return nil, bad("exactly one protocol must be primary, got %d", primaries)
		}
	}
	re := versionPattern
	if s.VersionPattern != "" {
		var err error
		if re, err = compileVersionPattern(s.VersionPattern); err != nil {
			return nil, bad("version pattern: %v", err)
		}
	}
	return re, nil
}

// RuntimeRegistry knows the engines db-branch can run.
type RuntimeRegistry struct {
	mu    sync.RWMutex
	specs map[Engine]EngineSpec
}

// NewRuntimeRegistry returns an empty registry.
func NewRuntimeRegistry() *RuntimeRegistry {
	return &RuntimeRegistry{specs: map[Engine]EngineSpec{}}
}

// Register adds an engine; one that breaks the contract, or is registered already, is refused.
func (r *RuntimeRegistry) Register(s EngineSpec) error {
	re, err := s.validate()
	if err != nil {
		return err
	}
	s.versionRE = re
	s = s.Clone()
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.specs[s.Engine]; dup {
		return fmt.Errorf("%w: engine %q is registered already", ErrInvalidEngineSpec, s.Engine)
	}
	r.specs[s.Engine] = s
	return nil
}

// Spec returns the declaration of a registered engine.
func (r *RuntimeRegistry) Spec(e Engine) (EngineSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.specs[e]
	return s.Clone(), ok
}

// Engines lists the registered engines in name order.
func (r *RuntimeRegistry) Engines() []Engine {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Engine, 0, len(r.specs))
	for e := range r.specs {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Parse parses "engine@version" for a registered engine.
func (r *RuntimeRegistry) Parse(s string) (RuntimeRef, error) {
	engine, version, ok := strings.Cut(s, "@")
	spec, known := r.Spec(Engine(engine))
	if !ok || !known || !spec.versionRE.MatchString(version) {
		return RuntimeRef{}, fmt.Errorf("%w %q: want engine@version, e.g. mysql@8.4.13 or postgres@17.2", ErrInvalidRuntime, s)
	}
	return RuntimeRef{Engine: Engine(engine), Version: version}, nil
}

// DSN is the URL a client uses to reach an instance of the engine.
func (e EngineSpec) DSN(host string, port int, user, password string) string {
	if e.FormatDSN != nil {
		return e.FormatDSN(host, port, user, password)
	}
	return formatDSN(e.Engine, host, port, user, password, "/")
}

// CheckUpgradePath decides whether a branch on from may move to to: the
// rules every engine shares, then the engine's own (EngineSpec.CheckUpgradePath).
func (r *RuntimeRegistry) CheckUpgradePath(from, to RuntimeRef) error {
	if err := checkUpgradeDirection(from, to); err != nil {
		return err
	}
	if spec, ok := r.Spec(to.Engine); ok && spec.CheckUpgradePath != nil {
		return spec.CheckUpgradePath(from, to)
	}
	return nil
}

// A version becomes part of paths and unit names, so a custom pattern must
// anchor the whole string (not one alternative of it) and must not let a path
// separator, whitespace or a dot-dot through.
var unsafeVersions = unsafeProbes()

func unsafeProbes() []string {
	probes := []string{"", ".", "..", "a..b", "1.2/../x", "-x", "a\x00b"}
	// each character a path or unit name cannot take, alone and between letters and digits
	for _, c := range []string{"/", "\\", " ", "\t", "\n", "\r", "%", ";", "$", "\"", "'"} {
		probes = append(probes, c, "a"+c+"b", "1"+c+"2")
	}
	return probes
}

func compileVersionPattern(pattern string) (*regexp.Regexp, error) {
	tree, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return nil, err
	}
	n := len(tree.Sub)
	if tree.Op != syntax.OpConcat || n < 2 || tree.Sub[0].Op != syntax.OpBeginText || tree.Sub[n-1].Op != syntax.OpEndText {
		return nil, errors.New("must start with ^ and end with $ around the whole expression")
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	for _, v := range unsafeVersions {
		if re.MatchString(v) {
			return nil, fmt.Errorf("accepts %q, which is not safe in a path", v)
		}
	}
	return re, nil
}
