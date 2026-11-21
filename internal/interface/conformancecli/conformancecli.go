// Package conformancecli is db-branch-conformance: the tool a runtime module
// author runs to check the module against the contract of
// docs/runtime-modules-standard.md before it is installed.
package conformancecli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/conformance"
)

// StartFunc starts the module of a manifest and returns it with a function
// that stops it. asCurrentUser runs it as the calling user instead of its
// own OS user (for development without root).
type StartFunc func(m domain.RuntimeManifest, asCurrentUser bool) (conformance.Provider, func(), error)

// OwnFunc hands directories to the OS user the module runs as. Main calls it
// before it starts a module that runs as its own user; nil does nothing.
type OwnFunc func(m domain.RuntimeManifest, paths ...string) error

// LifecycleFunc makes the DBServer that runs instances of the module through
// systemd, and the server (instance) to put through its life; cleanup removes
// what it made. It is what `--lifecycle` needs, and needs root.
type LifecycleFunc func(m domain.RuntimeManifest, p conformance.Provider) (srv domain.DBServer, s domain.Server, cleanup func(), err error)

// Exit codes.
const (
	ExitPass  = 0
	ExitFail  = 1 // a check failed
	ExitUsage = 2 // bad arguments, an unreadable manifest, or a module that did not start
)

// Main runs the tool and returns the exit code.
func Main(args []string, out, errOut io.Writer, start StartFunc, own OwnFunc, lifecycle LifecycleFunc) int {
	fs := flag.NewFlagSet("db-branch-conformance", flag.ContinueOnError)
	fs.SetOutput(errOut)
	manifestPath := fs.String("manifest", "", "path of the module's manifest.json (required)")
	root := fs.String("root", domain.ManifestRoot, "directory the module's files must be under")
	withLifecycle := fs.Bool("lifecycle", false, "also run an instance through initialise, start, stop, clone, crash recovery and remove under systemd (root)")
	current := fs.Bool("as-current-user", false, "run the module as the calling user (development only)")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *manifestPath == "" {
		fmt.Fprintln(errOut, "db-branch-conformance: --manifest is required")
		return ExitUsage
	}
	if *withLifecycle && lifecycle == nil {
		fmt.Fprintln(errOut, "db-branch-conformance: --lifecycle is not supported here")
		return ExitUsage
	}
	data, err := os.ReadFile(*manifestPath)
	if err != nil {
		fmt.Fprintf(errOut, "db-branch-conformance: %v\n", err)
		return ExitUsage
	}
	m, err := domain.ParseManifestIn(*root, data)
	if err != nil {
		fmt.Fprintf(errOut, "db-branch-conformance: manifest: %v\n", err)
		return ExitUsage
	}

	inst, cleanup, err := workspace(m)
	if err != nil {
		fmt.Fprintf(errOut, "db-branch-conformance: %v\n", err)
		return ExitUsage
	}
	defer cleanup()
	if own != nil && !*current {
		// The module runs as its own user and writes into these.
		if err := own(m, filepath.Dir(inst.DataDir), inst.DataDir, inst.StateDir, inst.RunDir); err != nil {
			fmt.Fprintf(errOut, "db-branch-conformance: %v\n", err)
			return ExitUsage
		}
	}

	p, stop, err := start(m, *current)
	if err != nil {
		fmt.Fprintf(errOut, "db-branch-conformance: %v\n", err)
		return ExitUsage
	}
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	results := conformance.Run(ctx, m, p, inst)
	if *withLifecycle {
		srv, server, cleanupLifecycle, err := lifecycle(m, p)
		if err != nil {
			fmt.Fprintf(errOut, "db-branch-conformance: lifecycle: %v\n", err)
			return ExitUsage
		}
		defer cleanupLifecycle()
		results = append(results, conformance.RunLifecycle(ctx, srv, server)...)
	}
	for _, r := range results {
		switch {
		case r.Err != nil:
			fmt.Fprintf(out, "FAIL %s: %v\n", r.Name, r.Err)
		case r.Note != "":
			fmt.Fprintf(out, "SKIP %s: %s\n", r.Name, r.Note)
		default:
			fmt.Fprintf(out, "PASS %s\n", r.Name)
		}
	}
	if !conformance.Passed(results) {
		return ExitFail
	}
	fmt.Fprintln(out, "all checks passed")
	return ExitPass
}

// workspace makes the directories the checks work in; cleanup removes them.
func workspace(m domain.RuntimeManifest) (domain.InstanceSpec, func(), error) {
	work, err := os.MkdirTemp("", "db-branch-conformance")
	if err != nil {
		return domain.InstanceSpec{}, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(work) }
	inst := domain.InstanceSpec{
		ID: "conformance", Runtime: domain.RuntimeRef{Engine: domain.Engine(m.Engine), Version: "0.0"},
		RuntimeDir: "/var/lib/db-branch/runtimes/" + m.Engine + "/0.0", // where the node installs a runtime
		DataDir:    work + "/data", StateDir: work + "/state", RunDir: work + "/run",
		Bind: "127.0.0.1", Ports: map[string]int{},
	}
	for _, p := range m.Protocols {
		port, err := freePort()
		if err != nil {
			cleanup()
			return domain.InstanceSpec{}, nil, err
		}
		inst.Ports[p.Name] = port
	}
	for _, d := range []string{inst.DataDir, inst.StateDir, inst.RunDir} {
		if err := mkdirAll(d, 0o755); err != nil {
			cleanup()
			return domain.InstanceSpec{}, nil, err
		}
	}
	return inst, cleanup, nil
}

// mkdirAll and freePort are variables so that tests can make them fail.
var mkdirAll = os.MkdirAll

// freePort asks the system for a TCP port nobody is listening on.
var freePort = func() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
