package runtimeprovider_test

import (
	"context"
	"fmt"

	"github.com/db-branch/db-branch/internal/domain"
)

// fakeServer records what the adapter asked of the DBServer.
type fakeServer struct {
	calls []string
	err   error
	admin domain.AdminAccess
}

func (f *fakeServer) rec(format string, a ...any) error {
	f.calls = append(f.calls, fmt.Sprintf(format, a...))
	return f.err
}

func (f *fakeServer) Initialize(_ context.Context, s domain.Server) error {
	return f.rec("Initialize %+v", s)
}
func (f *fakeServer) PrepareClone(_ context.Context, s domain.Server) error {
	return f.rec("PrepareClone %s", s.ID)
}
func (f *fakeServer) Start(_ context.Context, s domain.Server) error { return f.rec("Start %s", s.ID) }
func (f *fakeServer) Stop(_ context.Context, s domain.Server, m domain.StopMode) error {
	return f.rec("Stop %s %d", s.ID, m)
}
func (f *fakeServer) Running(context.Context, domain.Server) (bool, error) { return true, f.err }
func (f *fakeServer) Exec(_ context.Context, s domain.Server, sql string) error {
	return f.rec("Exec %s", sql)
}
func (f *fakeServer) ExecAs(_ context.Context, s domain.Server, user, pw, sql string) error {
	return f.rec("ExecAs %s %s %s", user, pw, sql)
}
func (f *fakeServer) CreateUser(_ context.Context, s domain.Server, user, pw string) error {
	return f.rec("CreateUser %s %s port=%d", user, pw, s.Port)
}
func (f *fakeServer) Load(_ context.Context, s domain.Server, p string) error {
	return f.rec("Load %s", p)
}
func (f *fakeServer) Remove(_ context.Context, s domain.Server) error {
	return f.rec("Remove %s", s.ID)
}
func (f *fakeServer) Admin(s domain.Server) domain.AdminAccess { return f.admin }

// fakeFull adds the optional ports.
type fakeFull struct {
	fakeServer
	plan domain.UpgradePlan
	log  domain.UpgradeLog
}

func (f *fakeFull) UpdateStats(_ context.Context, s domain.Server) error {
	return f.rec("UpdateStats %s", s.ID)
}
func (f *fakeFull) CheckUpgrade(_ context.Context, s domain.Server, to domain.RuntimeRef, own domain.Login) (domain.UpgradePlan, error) {
	return f.plan, f.rec("CheckUpgrade %s %s %s", s.ID, to, own.User)
}
func (f *fakeFull) StartUpgraded(_ context.Context, s domain.Server, own domain.Login) (domain.UpgradeLog, error) {
	return f.log, f.rec("StartUpgraded %s %s", s.ID, own.User)
}
