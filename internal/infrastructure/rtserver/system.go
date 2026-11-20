package rtserver

import (
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/execx"
	"github.com/db-branch/db-branch/internal/infrastructure/rtunit"
)

// NewSystem is a Server for this node: real systemctl, drop-ins in systemd's
// runtime directory, directories made and handed over on the file system.
// dataDir is the agent's data directory (state under instances/, unpacked
// runtimes under runtimes/).
func NewSystem(engine domain.EngineSpec, p domain.RuntimeProvider, osUser, dataDir, bind string) *Server {
	return &Server{
		Engine: engine, Provider: p, Run: execx.Runner{}, Units: rtunit.Writer{}, User: osUser,
		Mkdir:     makeDir,
		Chown:     chownTo,
		StateRoot: filepath.Join(dataDir, "instances"), RuntimeRoot: filepath.Join(dataDir, "runtimes"),
		Bind: bind, StartTimeout: 2 * time.Minute, PollInterval: 200 * time.Millisecond,
	}
}

// chownTo gives path to the user and its primary group.
func chownTo(name, path string) error {
	u, err := user.Lookup(name)
	if err != nil {
		return err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return os.Chown(path, uid, gid)
}

// makeDir makes path with parents anyone can enter (the module runs as its own
// user and walks down to its directory) and the directory itself closed to others.
func makeDir(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}
