package raftstore

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/auth"
)

// BackupFormat is the version of the backup file layout.
const BackupFormat = 1

// MaxBackupBytes caps the size of a backup after decompression (a gzip
// can expand a thousandfold), so a hostile file cannot exhaust memory.
var MaxBackupBytes int64 = 64 << 20

// ErrBackupFormat rejects a backup this version cannot read.
var ErrBackupFormat = errors.New("unsupported backup format")

// backupFile is what a backup holds: the cluster's configuration and
// records, without what belongs to the running cluster (nodes, operations,
// the join secret).
type backupFile struct {
	Format     int                          `json:"format"`
	TakenAt    time.Time                    `json:"taken_at"`
	Node       string                       `json:"node"`
	Admin      *auth.Credentials            `json:"admin,omitempty"`
	Runtimes   map[string]bool              `json:"runtimes,omitempty"`
	RouteUsers map[string]domain.RouteUser  `json:"route_users,omitempty"`
	Modules    map[string]domain.ModuleSpec `json:"modules,omitempty"`
	Settings   map[string]map[string]string `json:"settings,omitempty"`
	Tokens     map[string]domain.APIToken   `json:"tokens,omitempty"`
	ModuleData map[string]map[string][]byte `json:"module_data,omitempty"`
	Bases      map[string]domain.Base       `json:"bases,omitempty"`
	Branches   map[string]domain.Branch     `json:"branches,omitempty"`
}

// WriteBackup writes the cluster state as gzipped JSON. It holds secrets
// (password and token hashes, the licence key, module data): keep it as
// carefully as the cluster itself.
func (s *Store) WriteBackup(w io.Writer, node string, now time.Time) error {
	st := s.fsm.State()
	b := backupFile{Format: BackupFormat, TakenAt: now.UTC(), Node: node, Admin: st.Admin, Runtimes: st.Runtimes, RouteUsers: st.RouteUsers,
		Modules: st.Modules, Settings: st.Settings, Tokens: st.Tokens, ModuleData: st.ModuleData, Bases: st.Bases, Branches: st.Branches}
	zw := gzip.NewWriter(w)
	if err := json.NewEncoder(zw).Encode(b); err != nil {
		return err
	}
	return zw.Close()
}

// RestoreBackup applies a backup's configuration to this cluster through
// the log: settings (merged, the backup's values win), tokens, the admin
// password, modules and their data, route users and engines. Bases and
// branches are not restored, since their data is not in the backup; the
// summary lists them.
func (s *Store) RestoreBackup(ctx context.Context, r io.Reader) (domain.RestoreSummary, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return domain.RestoreSummary{}, fmt.Errorf("not a db-branch backup: %w", err)
	}
	defer zr.Close()
	lr := &io.LimitedReader{R: zr, N: MaxBackupBytes + 1}
	var b backupFile
	if err := json.NewDecoder(lr).Decode(&b); err != nil {
		if lr.N <= 0 {
			return domain.RestoreSummary{}, fmt.Errorf("%w: a backup expands to at most %d bytes", domain.ErrTooLarge, MaxBackupBytes)
		}
		return domain.RestoreSummary{}, fmt.Errorf("reading backup: %w", err)
	}
	if b.Format != BackupFormat {
		return domain.RestoreSummary{}, fmt.Errorf("%w %d (this version reads %d)", ErrBackupFormat, b.Format, BackupFormat)
	}
	sum := domain.RestoreSummary{TakenAt: b.TakenAt, Node: b.Node, SkippedBases: []string{}, SkippedBranches: []string{}}
	var errs []error
	apply := func(err error, n *int) {
		if err != nil {
			errs = append(errs, err)
		} else if n != nil {
			*n++
		}
	}
	if b.Admin != nil {
		err := s.Apply(ctx, SetAdmin(*b.Admin))
		sum.Admin = err == nil
		apply(err, nil)
	}
	current := s.fsm.State().Settings
	for scope, v := range b.Settings {
		merged := map[string]string{}
		for k, x := range current[scope] {
			merged[k] = x
		}
		for k, x := range v {
			merged[k] = x
		}
		apply(s.PutSettings(ctx, scope, merged), &sum.Settings)
	}
	for _, t := range b.Tokens {
		apply(s.PutToken(ctx, t), &sum.Tokens)
	}
	for _, m := range b.Modules {
		apply(s.PutModule(ctx, m), &sum.Modules)
	}
	for module, records := range b.ModuleData {
		for k, v := range records {
			apply(s.PutModuleData(ctx, module, k, v), &sum.ModuleData)
		}
	}
	for _, u := range b.RouteUsers {
		apply(s.PutRouteUser(ctx, u), &sum.RouteUsers)
	}
	for rt, on := range b.Runtimes {
		ref, err := domain.ParseRuntimeRef(rt)
		if on && err == nil {
			apply(s.AddRuntime(ctx, ref), &sum.Runtimes)
		}
	}
	for n := range b.Bases {
		sum.SkippedBases = append(sum.SkippedBases, n)
	}
	for n := range b.Branches {
		sum.SkippedBranches = append(sum.SkippedBranches, n)
	}
	sort.Strings(sum.SkippedBases)
	sort.Strings(sum.SkippedBranches)
	return sum, errors.Join(errs...)
}
