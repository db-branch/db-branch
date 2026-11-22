// Package apt installs and removes Debian packages for the agent.
package apt

import (
	"context"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

// Runner runs a command and returns its standard output.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Packages implements module.Packages with dpkg-query, apt-cache and
// apt-get. Changes run as their own transient systemd unit: a package's
// maintainer script may restart the agent, which must not kill apt halfway.
type Packages struct {
	Run Runner
	// Systemd says whether to detach changes with systemd-run (default: when
	// systemd runs the machine).
	Systemd *bool
	// RepoList is the APT source list of the db-branch repository (default
	// DefaultRepoList). Refresh reads only it when it exists.
	RepoList string
}

// DefaultRepoList is where docs/distribution.md puts the repository.
const DefaultRepoList = "/etc/apt/sources.list.d/db-branch.list"

func (p Packages) systemd() bool {
	if p.Systemd != nil {
		return *p.Systemd
	}
	_, err := os.Stat("/run/systemd/system")
	return err == nil
}

// Installed returns the installed versions of packages named prefix*.
func (p Packages) Installed(ctx context.Context, prefix string) (map[string]string, error) {
	out, err := p.Run.Run(ctx, "dpkg-query", "-W", "-f=${Package}\t${Status}\t${Version}\n", prefix+"*")
	if err != nil && len(out) == 0 {
		// dpkg-query fails when nothing matches the pattern.
		return map[string]string{}, nil
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) == 3 && f[1] == "install ok installed" {
			got[f[0]] = f[2]
		}
	}
	return got, nil
}

// Search lists packages named prefix* known to apt, with their descriptions.
func (p Packages) Search(ctx context.Context, prefix string) (map[string]string, error) {
	out, err := p.Run.Run(ctx, "apt-cache", "search", "--names-only", "^"+strings.ReplaceAll(prefix, ".", `\.`))
	if err != nil {
		return nil, err
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if pkg, desc, ok := strings.Cut(line, " - "); ok && strings.HasPrefix(pkg, prefix) {
			got[pkg] = desc
		}
	}
	return got, nil
}

// Candidate is the version apt would install, refreshing the package lists
// once if it knows none.
func (p Packages) Candidate(ctx context.Context, pkg string) (string, error) {
	if v, err := p.candidate(ctx, pkg); v != "" || err != nil {
		return v, err
	}
	if err := p.update(ctx); err != nil {
		return "", err
	}
	return p.candidate(ctx, pkg)
}

func (p Packages) candidate(ctx context.Context, pkg string) (string, error) {
	out, err := p.Run.Run(ctx, "apt-cache", "policy", pkg)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "Candidate:"); ok {
			v = strings.TrimSpace(v)
			if v == "(none)" {
				return "", nil
			}
			return v, nil
		}
	}
	return "", nil
}

// Timeouts keep a dead mirror from blocking module changes forever.
var (
	UpdateTimeout = 2 * time.Minute
	ChangeTimeout = 15 * time.Minute
)

func (p Packages) update(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, UpdateTimeout)
	defer cancel()
	_, err := p.Run.Run(ctx, "apt-get", "update", "-q")
	return err
}

// changing serializes package changes in this process: module sync and the
// rolling update each hold their own Packages, and two dpkg runs at once fail
// on dpkg's lock.
var changing sync.Mutex

// lockWait is how long apt-get waits for dpkg's lock held by something else
// (unattended-upgrades, an operator's apt) instead of failing at once.
const lockWait = "DPkg::Lock::Timeout=60"

func (p Packages) change(ctx context.Context, args ...string) error {
	changing.Lock()
	defer changing.Unlock()
	ctx, cancel := context.WithTimeout(ctx, ChangeTimeout)
	defer cancel()
	apt := append([]string{"apt-get", "-y", "-q", "-o", "Dpkg::Options::=--force-confold", "-o", lockWait}, args...)
	if !p.systemd() {
		_, err := p.Run.Run(ctx, "env", append([]string{"DEBIAN_FRONTEND=noninteractive"}, apt...)...)
		return err
	}
	_, err := p.Run.Run(ctx, "systemd-run", append([]string{"--quiet", "--wait", "--collect",
		"--description=db-branch module change", "--setenv=DEBIAN_FRONTEND=noninteractive"}, apt...)...)
	return err
}

// Install installs pkg at exactly version (downgrading if needed).
func (p Packages) Install(ctx context.Context, pkg, version string) error {
	if err := p.change(ctx, "install", "--allow-downgrades", pkg+"="+version); err != nil {
		// The version may be newer than the package lists know.
		if uerr := p.update(ctx); uerr != nil {
			return err
		}
		return p.change(ctx, "install", "--allow-downgrades", pkg+"="+version)
	}
	return nil
}

// Remove removes pkg.
func (p Packages) Remove(ctx context.Context, pkg string) error {
	return p.change(ctx, "remove", pkg)
}

// Refresh downloads the package lists of the db-branch repository only (the
// other sources are left alone); without that source list, of all sources.
func (p Packages) Refresh(ctx context.Context) error {
	list := p.RepoList
	if list == "" {
		list = DefaultRepoList
	}
	args := []string{"update", "-q"}
	if _, err := os.Stat(list); err == nil {
		args = append(args, "-o", "Dir::Etc::sourcelist="+list, "-o", "Dir::Etc::sourceparts=-", "-o", "APT::Get::List-Cleanup=0")
	}
	ctx, cancel := context.WithTimeout(ctx, UpdateTimeout)
	defer cancel()
	_, err := p.Run.Run(ctx, "apt-get", args...)
	return err
}

var coreRequirement = regexp.MustCompile(`(?:^|[,|]\s*)db-branch\s*\((?:>=|>>|=)\s*([^)\s]+)\s*\)`)

// Versions lists the versions of pkg the lists offer, newest first as apt
// prints them, with the db-branch version each requires.
func (p Packages) Versions(ctx context.Context, pkg string) ([]domain.PackageVersion, error) {
	out, err := p.Run.Run(ctx, "apt-cache", "madison", pkg)
	if err != nil {
		return nil, err
	}
	var versions []string
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Split(line, "|"); len(f) >= 2 && !slices.Contains(versions, strings.TrimSpace(f[1])) {
			versions = append(versions, strings.TrimSpace(f[1]))
		}
	}
	if len(versions) == 0 {
		return nil, nil
	}
	shown, err := p.Run.Run(ctx, "apt-cache", "show", pkg)
	if err != nil {
		return nil, err
	}
	requires := requirements(string(shown))
	got := make([]domain.PackageVersion, 0, len(versions))
	for _, v := range versions {
		got = append(got, domain.PackageVersion{Version: v, Requires: requires[v]})
	}
	return got, nil
}

// requirements maps each version in apt-cache show output to the db-branch
// version its Depends / Pre-Depends name.
func requirements(show string) map[string]string {
	out := map[string]string{}
	version := ""
	for _, line := range strings.Split(show, "\n") {
		switch {
		case line == "":
			version = ""
		case strings.HasPrefix(line, " "):
		case strings.HasPrefix(line, "Version:"):
			version = strings.TrimSpace(strings.TrimPrefix(line, "Version:"))
		case strings.HasPrefix(line, "Depends:"), strings.HasPrefix(line, "Pre-Depends:"):
			if m := coreRequirement.FindStringSubmatch(strings.TrimSpace(line[strings.Index(line, ":")+1:])); m != nil && version != "" {
				out[version] = m[1]
			}
		}
	}
	return out
}

// Upgrade moves the packages (package -> version) to those versions, never
// down. The change runs detached: db-branch's own postinst restarts the agent.
func (p Packages) Upgrade(ctx context.Context, versions map[string]string) error {
	pkgs := make([]string, 0, len(versions))
	for pkg := range versions {
		pkgs = append(pkgs, pkg)
	}
	slices.Sort(pkgs)
	args := []string{"install", "--only-upgrade"}
	for _, pkg := range pkgs {
		args = append(args, pkg+"="+versions[pkg])
	}
	if err := p.change(ctx, args...); err != nil {
		// The versions may be newer than this node's package lists.
		if uerr := p.Refresh(ctx); uerr != nil {
			return err
		}
		return p.change(ctx, args...)
	}
	return nil
}
