// Command db-branch-conformance checks a runtime module against the contract
// of docs/runtime-modules-standard.md. Run it as root so the module starts
// as its own OS user, or with --as-current-user while developing.
package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/rtserver"
	"github.com/db-branch/db-branch/internal/infrastructure/runtimegrpc"
	"github.com/db-branch/db-branch/internal/interface/conformancecli"
	"github.com/db-branch/db-branch/internal/usecase/conformance"
)

func main() {
	os.Exit(conformancecli.Main(os.Args[1:], os.Stdout, os.Stderr, start, own, lifecycle))
}

func start(m domain.RuntimeManifest, asCurrentUser bool) (conformance.Provider, func(), error) {
	var cmd *exec.Cmd
	if asCurrentUser {
		cmd = exec.Command(m.Binary)
		cmd.Dir = "/"
		cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	} else {
		var err error
		if cmd, err = runtimegrpc.Command(m, runtimegrpc.SystemUser); err != nil {
			return nil, nil, err
		}
	}
	return runtimegrpc.Start(cmd, m)
}

// own hands the directories to the module's OS user (db-branch-rt-<engine>).
func own(m domain.RuntimeManifest, paths ...string) error {
	name := m.RunAsUser
	if name == "" {
		name = domain.RuntimeUserPrefix + m.Engine
	}
	uid, gid, err := runtimegrpc.SystemUser(name)
	if err != nil {
		return fmt.Errorf("OS user %s: %w", name, err)
	}
	for _, p := range paths {
		if err := os.Chown(p, int(uid), int(gid)); err != nil {
			return err
		}
	}
	return nil
}

// lifecycle runs the module's instance under systemd as the node would. Its
// data lives outside /tmp, which the unit does not see (PrivateTmp).
func lifecycle(m domain.RuntimeManifest, p conformance.Provider) (domain.DBServer, domain.Server, func(), error) {
	rp, ok := p.(domain.RuntimeProvider)
	if !ok {
		return nil, domain.Server{}, nil, errors.New("the module does not offer the full provider")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, domain.Server{}, nil, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	name := m.RunAsUser
	if name == "" {
		name = domain.RuntimeUserPrefix + m.Engine
	}
	id := "conformance-" + m.Engine
	data := "/var/lib/db-branch/conformance/" + m.Engine
	srv := rtserver.NewSystem(m.Spec(), rp, name, "/var/lib/db-branch", "127.0.0.1")
	if err := srv.Mkdir(data); err != nil {
		return nil, domain.Server{}, nil, err
	}
	s := domain.Server{ID: id, Runtime: domain.RuntimeRef{Engine: domain.Engine(m.Engine), Version: "0.0"}, DataDir: data, Port: port}
	cleanup := func() {
		_ = os.RemoveAll(filepath.Dir(data))
		_ = os.RemoveAll(filepath.Join(srv.StateRoot, id))
	}
	return srv, s, cleanup, nil
}
