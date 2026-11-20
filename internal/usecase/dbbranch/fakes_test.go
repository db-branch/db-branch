package dbbranch_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/filestore"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
)

var ctx = context.Background()

var mysql84 = domain.RuntimeRef{Engine: domain.MySQL, Version: "8.4.13"}

func name(s string) domain.Name {
	n, err := domain.ParseName(s)
	if err != nil {
		panic(err)
	}
	return n
}

// journal records what happened, in order, across the fakes.
type journal struct {
	mu      sync.Mutex
	entries []string
}

func (j *journal) add(format string, args ...any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.entries = append(j.entries, fmt.Sprintf(format, args...))
}

func (j *journal) has(entry string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, e := range j.entries {
		if e == entry {
			return true
		}
	}
	return false
}

// indexOf returns the position of the first entry equal to e, or -1.
func (j *journal) indexOf(e string) int {
	j.mu.Lock()
	defer j.mu.Unlock()
	for i, x := range j.entries {
		if x == e {
			return i
		}
	}
	return -1
}

type fakeStorage struct {
	j        *journal
	root     string
	datasets map[string]bool
	snaps    map[string]bool
	failOn   string
	// props are the properties each dataset was created with.
	props map[string]map[string]string
	// destroyCtxErr is the state of the context the last Destroy got.
	destroyCtxErr error
}

func (s *fakeStorage) fail(op string) error {
	if s.failOn == op {
		return errors.New(op + " failed")
	}
	return nil
}

func (s *fakeStorage) Create(_ context.Context, ds string, props map[string]string) error {
	s.j.add("zfs create %s", ds)
	if s.props == nil {
		s.props = map[string]map[string]string{}
	}
	s.props[ds] = props
	if err := s.fail("create"); err != nil {
		return err
	}
	s.datasets[ds] = true
	return nil
}

func (s *fakeStorage) Snapshot(_ context.Context, snap domain.Snapshot) error {
	s.j.add("zfs snapshot %s", snap)
	if err := s.fail("snapshot"); err != nil {
		return err
	}
	s.snaps[snap.String()] = true
	return nil
}

func (s *fakeStorage) Clone(_ context.Context, origin domain.Snapshot, ds string) error {
	s.j.add("zfs clone %s %s", origin, ds)
	if err := s.fail("clone"); err != nil {
		return err
	}
	if !s.snaps[origin.String()] {
		return fmt.Errorf("no snapshot %s", origin)
	}
	s.datasets[ds] = true
	return nil
}

func (s *fakeStorage) Rollback(_ context.Context, snap domain.Snapshot) error {
	s.j.add("zfs rollback %s", snap)
	return s.fail("rollback")
}

func (s *fakeStorage) Destroy(ctx context.Context, ds string) error {
	s.j.add("zfs destroy %s", ds)
	s.destroyCtxErr = ctx.Err()
	if err := s.fail("destroy"); err != nil {
		return err
	}
	delete(s.datasets, ds)
	for k := range s.snaps {
		if strings.HasPrefix(k, ds+"@") {
			delete(s.snaps, k)
		}
	}
	return nil
}

func (s *fakeStorage) DestroySnapshot(_ context.Context, snap domain.Snapshot) error {
	s.j.add("zfs destroy %s", snap)
	if err := s.fail("destroy-snapshot"); err != nil {
		return err
	}
	delete(s.snaps, snap.String())
	return nil
}

func (s *fakeStorage) Exists(_ context.Context, ds string) (bool, error) {
	return s.datasets[ds], s.fail("exists")
}

func (s *fakeStorage) Info(_ context.Context, ds string) (domain.DatasetInfo, error) {
	if err := s.fail("info"); err != nil {
		return domain.DatasetInfo{}, err
	}
	return domain.DatasetInfo{Mountpoint: filepath.Join(s.root, ds), Used: 1 << 20, Referenced: 500 << 20}, nil
}

type fakeServer struct {
	memory  map[string]int
	j       *journal
	running map[string]bool
	failOn  string
	sql     map[string][]string
	// started are the specs the server was started with, in order.
	started []domain.Server
	// ranAs are the logins init SQL ran as (never the admin), in order.
	ranAs []string

	// Upgrader behaviour (see upgrade_test.go).
	plan       domain.UpgradePlan
	checkErr   error
	checkedFor domain.Login
	upgraded   []domain.Server
	upFail     error
	upFindings []domain.Finding
	upExcerpt  string
}

func (f *fakeServer) fail(op string) error {
	if f.failOn == op {
		if op == "stop-clean" {
			return fmt.Errorf("%w: injected", domain.ErrUncleanShutdown)
		}
		return errors.New(op + " failed")
	}
	return nil
}

func (f *fakeServer) Initialize(_ context.Context, s domain.Server) error {
	f.j.add("init %s %s port=%d", s.ID, s.Runtime, s.Port)
	return f.fail("init")
}

func (f *fakeServer) PrepareClone(_ context.Context, s domain.Server) error {
	f.j.add("prepare %s", s.ID)
	return f.fail("prepare")
}

func (f *fakeServer) Start(_ context.Context, s domain.Server) error {
	if f.memory == nil {
		f.memory = map[string]int{}
	}
	f.memory[s.ID] = s.MemoryMB
	f.started = append(f.started, s)
	f.j.add("start %s", s.ID)
	if err := f.fail("start"); err != nil {
		return err
	}
	f.running[s.ID] = true
	return nil
}

func (f *fakeServer) Stop(_ context.Context, s domain.Server, mode domain.StopMode) error {
	m := map[domain.StopMode]string{domain.StopClean: "clean", domain.StopImmediate: "immediate"}[mode]
	f.j.add("stop %s %s", s.ID, m)
	if err := f.fail("stop-" + m); err != nil {
		return err
	}
	f.running[s.ID] = false
	return nil
}

func (f *fakeServer) Running(_ context.Context, s domain.Server) (bool, error) {
	return f.running[s.ID], nil
}

func (f *fakeServer) Exec(_ context.Context, s domain.Server, sql string) error {
	f.j.add("exec %s", s.ID)
	f.sql[s.ID] = append(f.sql[s.ID], sql)
	return f.fail("exec")
}

// ExecAs records the SQL like Exec and the login it ran as.
func (f *fakeServer) ExecAs(ctx context.Context, s domain.Server, user, _, sql string) error {
	f.ranAs = append(f.ranAs, user)
	return f.Exec(ctx, s, sql)
}

// CreateUser records the MySQL statement, so tests can read what was made.
func (f *fakeServer) CreateUser(ctx context.Context, s domain.Server, user, password string) error {
	return f.Exec(ctx, s, fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s'", user, password))
}

func (f *fakeServer) Load(_ context.Context, s domain.Server, dump string) error {
	f.j.add("load %s %s", s.ID, dump)
	return f.fail("load")
}

func (f *fakeServer) Remove(_ context.Context, s domain.Server) error {
	f.j.add("remove %s", s.ID)
	f.running[s.ID] = false
	return f.fail("remove")
}

func (f *fakeServer) Admin(s domain.Server) domain.AdminAccess {
	return domain.AdminAccess{Engine: s.Runtime.Engine, Socket: "/run/" + s.ID, Port: s.Port, User: "root", BinDir: "/rt/bin"}
}

type fakeRuntimes struct{ installed map[domain.RuntimeRef]bool }

func (r fakeRuntimes) List(context.Context) ([]domain.RuntimeRef, error) { return nil, nil }
func (r fakeRuntimes) Has(_ context.Context, ref domain.RuntimeRef) (bool, error) {
	return r.installed[ref], nil
}
func (r fakeRuntimes) Install(context.Context, domain.RuntimeRef, []string) error { return nil }
func (r fakeRuntimes) Remove(context.Context, domain.RuntimeRef) error            { return nil }

type env struct {
	svc     *dbbranch.Service
	j       *journal
	storage *fakeStorage
	server  *fakeServer
	store   *filestore.Store
	now     time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	j := &journal{}
	e := &env{
		j:       j,
		storage: &fakeStorage{j: j, root: t.TempDir(), datasets: map[string]bool{}, snaps: map[string]bool{}},
		server:  &fakeServer{j: j, running: map[string]bool{}, sql: map[string][]string{}},
		store:   &filestore.Store{Path: filepath.Join(t.TempDir(), "state.json")},
		now:     time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC),
	}
	seq := 0
	e.svc = &dbbranch.Service{
		Store:    e.store,
		Storage:  e.storage,
		Servers:  map[domain.Engine]domain.DBServer{domain.MySQL: e.server},
		Runtimes: fakeRuntimes{installed: map[domain.RuntimeRef]bool{mysql84: true}},
		Pool:     "tank",
		Node:     "node1",
		Ports:    dbbranch.PortRange{Min: 40000, Max: 40010},
		MemoryMB: 128,
		Clock:    func() time.Time { return e.now },
		Password: func() string { seq++; return fmt.Sprintf("pw%d", seq) },
	}
	return e
}

// withBase creates a ready base "prod" with one snapshot and clears the journal.
func (e *env) withBase(t *testing.T) domain.Base {
	t.Helper()
	b, err := e.svc.CreateBase(ctx, dbbranch.CreateBaseRequest{Name: name("prod"), Runtime: mysql84, Dump: "/dumps/prod.sql.gz"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.j.entries = nil
	return b
}

func (e *env) withBranch(t *testing.T, n string) domain.Branch {
	t.Helper()
	b, err := e.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name(n), Base: name("prod")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.j.entries = nil
	return b
}

func (f *fakeServer) UpdateStats(_ context.Context, s domain.Server) error {
	f.j.add("analyze %s", s.ID)
	return f.fail("analyze")
}
