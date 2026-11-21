package conformance_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/conformance"
)

// server is an in-memory DBServer; failOn names the call that fails.
type server struct {
	calls   []string
	running bool
	clean   bool
	failOn  string
	// crashLoses: after a killed server, Start fails (no crash consistency).
	crashLoses bool
	killed     bool
}

func (s *server) rec(name string) error {
	s.calls = append(s.calls, name)
	if s.failOn == name {
		return errors.New(name + " failed")
	}
	return nil
}
func (s *server) Initialize(context.Context, domain.Server) error   { return s.rec("Initialize") }
func (s *server) PrepareClone(context.Context, domain.Server) error { return s.rec("PrepareClone") }
func (s *server) Start(context.Context, domain.Server) error {
	if err := s.rec("Start"); err != nil {
		return err
	}
	if s.killed && s.crashLoses {
		return errors.New("data directory is corrupt")
	}
	s.running, s.killed = true, false
	return nil
}
func (s *server) Stop(_ context.Context, _ domain.Server, m domain.StopMode) error {
	if m == domain.StopImmediate {
		if err := s.rec("Stop(immediate)"); err != nil {
			return err
		}
		s.running, s.killed = false, true
		return nil
	}
	if err := s.rec("Stop"); err != nil {
		return err
	}
	s.running = false
	return nil
}
func (s *server) Running(context.Context, domain.Server) (bool, error) { return s.running, nil }
func (s *server) Exec(context.Context, domain.Server, string) error    { return nil }
func (s *server) ExecAs(context.Context, domain.Server, string, string, string) error {
	return nil
}
func (s *server) CreateUser(context.Context, domain.Server, string, string) error {
	return s.rec("CreateUser")
}
func (s *server) Load(context.Context, domain.Server, string) error { return nil }
func (s *server) Remove(context.Context, domain.Server) error       { return s.rec("Remove") }
func (s *server) Admin(domain.Server) domain.AdminAccess            { return domain.AdminAccess{} }

func TestRunLifecycle_契約を守るサーバーは全段階に合格する(t *testing.T) {
	s := &server{}
	rs := conformance.RunLifecycle(context.Background(), s, domain.Server{ID: "conformance"})
	for _, r := range rs {
		if r.Err != nil {
			t.Errorf("%s: %v", r.Name, r.Err)
		}
	}
	for _, want := range []string{"lifecycle initialize", "lifecycle create user twice", "lifecycle start", "lifecycle clean stop", "lifecycle restart", "lifecycle clone", "lifecycle crash recovery", "lifecycle remove"} {
		if _, ok := byName(rs)[want]; !ok {
			t.Errorf("no check %q in %v", want, rs)
		}
	}
	if s.running {
		t.Fatal("the server must be stopped at the end")
	}
	if got := s.calls[len(s.calls)-1]; got != "Remove" {
		t.Fatalf("the last call must be Remove, got %s (%v)", got, s.calls)
	}
}

func TestRunLifecycle_CreateUserは繰り返しても成功しなければならない(t *testing.T) {
	s := &server{}
	conformance.RunLifecycle(context.Background(), s, domain.Server{ID: "c"})
	n := 0
	for _, c := range s.calls {
		if c == "CreateUser" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("CreateUser called %d times, want 2", n)
	}
}

func TestRunLifecycle_失敗した段階を示し以降は実行せず後片付けする(t *testing.T) {
	s := &server{failOn: "Start"}
	rs := conformance.RunLifecycle(context.Background(), s, domain.Server{ID: "c"})
	m := byName(rs)
	if m["lifecycle start"].Err == nil || !strings.Contains(m["lifecycle start"].Err.Error(), "Start failed") {
		t.Fatalf("%+v", m["lifecycle start"])
	}
	if r := m["lifecycle clean stop"]; r.Err == nil || !strings.Contains(r.Err.Error(), "not run") {
		t.Fatalf("later steps must be reported as not run: %+v", r)
	}
	if conformance.Passed(rs) {
		t.Fatal("Passed must be false")
	}
	if s.calls[len(s.calls)-1] != "Remove" {
		t.Fatalf("cleanup must still remove the instance: %v", s.calls)
	}
}

func TestRunLifecycle_強制停止のあとに起動できなければ不合格にする(t *testing.T) {
	s := &server{crashLoses: true}
	r := byName(conformance.RunLifecycle(context.Background(), s, domain.Server{ID: "c"}))["lifecycle crash recovery"]
	if r.Err == nil || !strings.Contains(r.Err.Error(), "corrupt") {
		t.Fatalf("%+v", r)
	}
}

type neverRuns struct{ *server }

func (neverRuns) Running(context.Context, domain.Server) (bool, error) { return false, nil }

func TestRunLifecycle_起動後に動いていなければ不合格にする(t *testing.T) {
	r := byName(conformance.RunLifecycle(context.Background(), neverRuns{&server{}}, domain.Server{ID: "c"}))["lifecycle start"]
	if r.Err == nil || !strings.Contains(r.Err.Error(), "not running") {
		t.Fatalf("%+v", r)
	}
}

type stuck struct{ *server }

// Stop reports success but leaves the server running.
func (s stuck) Stop(_ context.Context, _ domain.Server, m domain.StopMode) error {
	if m == domain.StopClean {
		return nil
	}
	return s.server.Stop(context.Background(), domain.Server{}, m)
}
func (s stuck) Start(ctx context.Context, d domain.Server) error {
	s.server.running = true
	return nil
}
func (s stuck) Running(context.Context, domain.Server) (bool, error) { return true, nil }

func TestRunLifecycle_通常停止のあとも動いていれば不合格にする(t *testing.T) {
	r := byName(conformance.RunLifecycle(context.Background(), stuck{&server{}}, domain.Server{ID: "c"}))["lifecycle clean stop"]
	if r.Err == nil || !strings.Contains(r.Err.Error(), "still running") {
		t.Fatalf("%+v", r)
	}
}

type unknownState struct{ *server }

func (unknownState) Running(context.Context, domain.Server) (bool, error) {
	return false, errors.New("systemctl unavailable")
}

func TestRunLifecycle_状態を確かめられなければ不合格にする(t *testing.T) {
	r := byName(conformance.RunLifecycle(context.Background(), unknownState{&server{}}, domain.Server{ID: "c"}))["lifecycle start"]
	if r.Err == nil || !strings.Contains(r.Err.Error(), "systemctl unavailable") {
		t.Fatalf("%+v", r)
	}
}

func TestRunLifecycle_どの呼び出しの失敗も該当の段階の不合格になる(t *testing.T) {
	cases := map[string]string{
		"Initialize": "lifecycle initialize", "CreateUser": "lifecycle create user twice", "Start": "lifecycle start",
		"Stop": "lifecycle clean stop", "PrepareClone": "lifecycle clone", "Remove": "lifecycle remove",
	}
	for call, step := range cases {
		rs := conformance.RunLifecycle(context.Background(), &server{failOn: call}, domain.Server{ID: "c"})
		if r := byName(rs)[step]; r.Err == nil || !strings.Contains(r.Err.Error(), call+" failed") {
			t.Errorf("%s: %+v", call, r)
		}
	}
	// A kill that fails fails the crash step, and the removal that follows.
	rs := byName(conformance.RunLifecycle(context.Background(), &server{failOn: "Stop(immediate)"}, domain.Server{ID: "c"}))
	if rs["lifecycle crash recovery"].Err == nil {
		t.Fatal("a failing kill must fail the crash recovery step")
	}
}
