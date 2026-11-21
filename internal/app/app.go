// Package app is the composition root: it builds use cases from real infrastructure.
package app

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/artifact"
	"github.com/db-branch/db-branch/internal/infrastructure/catalog"
	"github.com/db-branch/db-branch/internal/infrastructure/config"
	"github.com/db-branch/db-branch/internal/infrastructure/execx"
	"github.com/db-branch/db-branch/internal/infrastructure/filestore"
	"github.com/db-branch/db-branch/internal/infrastructure/modstorage"
	"github.com/db-branch/db-branch/internal/infrastructure/mysql"
	"github.com/db-branch/db-branch/internal/infrastructure/osrelease"
	"github.com/db-branch/db-branch/internal/infrastructure/postgres"
	"github.com/db-branch/db-branch/internal/infrastructure/runtimeprovider"
	"github.com/db-branch/db-branch/internal/infrastructure/runtimestore"
	"github.com/db-branch/db-branch/internal/infrastructure/sqlquery"
	"github.com/db-branch/db-branch/internal/infrastructure/srcdump"
	"github.com/db-branch/db-branch/internal/infrastructure/svcids"
	"github.com/db-branch/db-branch/internal/infrastructure/zfs"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
	"github.com/db-branch/db-branch/internal/usecase/runtime"
)

// ErrNotInitialized is returned before `db-branch init` has chosen a pool.
var ErrNotInitialized = errors.New("not initialized: run `db-branch init --pool <pool>`")

// ServiceUser runs the database servers.
const ServiceUser = "db-branch"

// MySQLRunRoot must match RuntimeDirectory= of db-branch-mysqld@.service.
const MySQLRunRoot = "/run/db-branch-mysqld"

// PostgresRunRoot must match RuntimeDirectory= of db-branch-postgres@.service.
const PostgresRunRoot = "/run/db-branch-postgres"

// App holds the wired use cases.
type App struct {
	Config    config.Config
	Runtimes  runtime.Service
	branches  *dbbranch.Service
	providers domain.ProviderSet
	// modStorage is set when the node runs on a module's storage.
	modStorage *modstorage.Storage
}

type node struct {
	Pool string `json:"pool"`
	// Storage names the module whose storage replaces the pool.
	Storage string `json:"storage,omitempty"`
}

func nodeFile(cfg config.Config) string { return filepath.Join(cfg.DataDir, "node.json") }

// SetPool records the pool chosen by `db-branch init`.
func SetPool(cfg config.Config, pool string) error {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(node{Pool: pool, Storage: cfg.Storage})
	return os.WriteFile(nodeFile(cfg), b, 0o644)
}

// loadNode reads what `db-branch init` chose; the config file overrides it.
func loadNode(cfg config.Config) (node, error) {
	var n node
	b, err := os.ReadFile(nodeFile(cfg))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return n, err
	default:
		if err := json.Unmarshal(b, &n); err != nil {
			return n, fmt.Errorf("%s: %w", nodeFile(cfg), err)
		}
	}
	if cfg.Storage != "" {
		n.Storage, n.Pool = cfg.Storage, cfg.Storage
	}
	if cfg.Pool != "" {
		n.Pool = cfg.Pool
	}
	return n, nil
}

// New wires everything for cfg.
func New(cfg config.Config) (*App, error) {
	nd, err := loadNode(cfg)
	if err != nil {
		return nil, err
	}
	pool := nd.Pool
	cfg.Storage = nd.Storage
	cat, err := catalog.Default()
	if err != nil {
		return nil, err
	}
	// Unknown when /etc/os-release is unreadable: runtimes that link the
	// system libraries then fail to install with an error saying so.
	distro, _ := osrelease.Distro(osrelease.Path)
	run := execx.Runner{}
	mysqlFixup := mysql.Fixup(mysql.SystemLibDirs(goruntime.GOARCH), mysql.NeededLibs)
	store := runtimestore.Store{
		Dir: filepath.Join(cfg.DataDir, "runtimes"),
		Run: run,
		Fixups: map[domain.Engine]func(string) error{
			domain.MySQL:    mysqlFixup,
			domain.Postgres: postgres.Fixup,
		},
		// The runtime modules' PrepareRuntime, added when a module is attached.
		RefFixups: map[domain.Engine]func(context.Context, domain.RuntimeRef, string) error{},
	}
	a := &App{
		Config: cfg,
		Runtimes: runtime.Service{
			Catalog: cat,
			Cache:   artifact.Cache{Dir: filepath.Join(cfg.DataDir, "artifacts"), Client: http.DefaultClient},
			Store:   store,
			Arch:    goruntime.GOARCH,
			Distro:  distro,
		},
	}
	if pool == "" {
		return a, nil
	}
	var storage domain.Storage = zfs.New(run)
	if cfg.Storage != "" {
		a.modStorage = &modstorage.Storage{Module: cfg.Storage}
		storage = a.modStorage
	}
	mysqlServer := &mysql.Server{
		Run:          run,
		RuntimeDir:   store.Path,
		StateRoot:    filepath.Join(cfg.DataDir, "instances"),
		RunRoot:      MySQLRunRoot,
		Bind:         cfg.Bind,
		Chown:        ChownTo(ServiceUser),
		CheckOwner:   svcids.OwnerCheck(serviceUID),
		StartTimeout: 2 * time.Minute,
		PollInterval: 200 * time.Millisecond,
	}
	postgresServer := &postgres.Server{
		Run:          run,
		RuntimeDir:   store.Path,
		StateRoot:    filepath.Join(cfg.DataDir, "instances"),
		RunRoot:      PostgresRunRoot,
		Bind:         cfg.Bind,
		User:         ServiceUser,
		Chown:        ChownTo(ServiceUser),
		CheckOwner:   svcids.OwnerCheck(serviceUID),
		StartTimeout: 2 * time.Minute,
		PollInterval: 200 * time.Millisecond,
	}

	// Each engine is run through its runtime provider where the core asks for
	// SQL or a source dump; the servers themselves are shared.
	queries := sqlquery.Runner{}
	a.providers = domain.ProviderSet{
		domain.MySQL:    mysql.NewProvider(mysqlServer, runtimeprovider.Hooks{PrepareRuntime: mysqlFixup, Query: queries.QueryHook(), DumpSource: srcdump.MySQLDump(run.StreamEnv)}),
		domain.Postgres: postgres.NewProvider(postgresServer, runtimeprovider.Hooks{Query: queries.QueryHook(), DumpSource: srcdump.PostgresDump(run.StreamEnv)}),
	}
	a.branches = &dbbranch.Service{
		Store:   &filestore.Store{Path: filepath.Join(cfg.DataDir, "state.json")},
		Storage: storage,
		Servers: map[domain.Engine]domain.DBServer{
			domain.MySQL:    mysqlServer,
			domain.Postgres: postgresServer,
		},
		Runtimes:  store,
		Providers: a.providers,
		// Scheduled refreshes: dumps (unmasked data) live here only while loading.
		Dumper:    srcdump.Dumper{WorkDir: filepath.Join(cfg.DataDir, "refresh"), Providers: a.providers},
		AutoPrune: pruneSnapshots.Load,
		// Checkpoints rename, promote and label ZFS datasets: only this node's own ZFS.
		MaxCheckpoints: func() int { return int(maxCheckpoints.Load()) },
		Pool:           pool,
		Node:           hostname(),
		Distro:         distro,
		Ports:          dbbranch.PortRange{Min: cfg.Ports.Min, Max: cfg.Ports.Max},
		MemoryMB:       cfg.Branch.MemoryMB,
		Clock:          time.Now,
		Password:       Password,
	}
	if cfg.Storage == "" {
		a.branches.Lineage = zfs.New(run)
	}
	return a, nil
}

// Providers returns the engines' runtime providers, or nil on a node that is not initialized.
func (a *App) Providers() domain.RuntimeProviders {
	if a.providers == nil {
		return nil
	}
	return a.providers
}

// Branches returns the base/branch service, or ErrNotInitialized.
func (a *App) Branches() (*dbbranch.Service, error) {
	if a.branches == nil {
		return nil, ErrNotInitialized
	}
	return a.branches, nil
}

const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// Password returns 24 random letters and digits (~142 bits).
func Password() string {
	b := make([]byte, 24)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet)))) // crypto/rand does not fail on Linux
		b[i] = alphabet[n.Int64()]
	}
	return string(b)
}

// ChownTo returns a function that gives a path to the named user and its primary group.
func ChownTo(name string) func(string) error {
	return func(path string) error {
		u, err := user.Lookup(name)
		if err != nil {
			return err
		}
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(u.Gid)
		return os.Chown(path, uid, gid)
	}
}
