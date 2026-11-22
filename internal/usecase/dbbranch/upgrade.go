package dbbranch

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/db-branch/db-branch/internal/domain"
)

// Upgrading a branch moves it to a newer runtime of its engine while its
// base and every other branch stay as they are:
//
//  1. validate (same engine, upgrade only, no LTS series skipped, target
//     installed), mark the branch upgrading
//  2. pre-check on the running server (accounts, configuration, space)
//  3. stop the server cleanly, save the automatic checkpoint
//     "before-upgrade-<time>" (which records the old runtime)
//  4. switch the branch's runtime and start the server on the new binaries;
//     the engine upgrades its data dictionary on that start
//  5. on any failure from step 3 on, restore the checkpoint (which switches
//     back to the old runtime) unless the caller asked to keep the failure
//
// An upgrade in progress is recorded on the history container, so an agent
// restart rolls it back (or, when nothing had changed yet, forgets it).

// UpgradeRequest asks for one branch to move to another runtime.
type UpgradeRequest struct {
	Name domain.Name
	To   domain.RuntimeRef
	// CheckOnly runs the pre-check and changes nothing.
	CheckOnly bool
	// NoRollback keeps a failed upgrade as it is, for investigation.
	NoRollback bool
	// Force upgrades although the pre-check found errors.
	Force bool
	// PoolFree is the pool's free bytes (0 or MaxUint64: unknown).
	PoolFree uint64
}

// requireInstalled fails with a hint when rt is not installed on this node.
func (s *Service) requireInstalled(ctx context.Context, rt domain.RuntimeRef) error {
	if s.Runtimes == nil {
		return nil
	}
	ok, err := s.Runtimes.Has(ctx, rt)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s is not installed on node %s; install it with: db-branch runtime install %s", domain.ErrRuntimeNotInstalled, rt, s.Node, rt)
	}
	return nil
}

// runtimeOf is the runtime data needs: the one a snapshot property names,
// else the base's (the reset point, and checkpoints from before runtimes were
// recorded, are made by it).
func (s *Service) runtimeOf(ctx context.Context, br domain.Branch, prop string) domain.RuntimeRef {
	if rt, ok := domain.ParseRuntimeProp(prop); ok {
		return rt
	}
	if base, err := s.Store.GetBase(ctx, br.Base); err == nil {
		return base.Runtime
	}
	return br.Runtime
}

// switchRuntime makes the branch run what the restored data needs.
func (s *Service) switchRuntime(ctx context.Context, br *domain.Branch, in restoreIntent) error {
	changed := false
	if in.Runtime != (domain.RuntimeRef{}) && in.Runtime != br.Runtime {
		br.Runtime, changed = in.Runtime, true
	}
	if in.Op == opReset && br.LegacyAuth {
		br.LegacyAuth, changed = false, true
	}
	if !changed {
		return nil
	}
	return s.saveBranch(ctx, *br)
}

// clusterAllowsBranchRuntime: every node knows that a branch may run another
// runtime than its base.
func (s *Service) clusterAllowsBranchRuntime(ctx context.Context) error {
	if s.Nodes == nil {
		return nil
	}
	nodes, err := s.Nodes.ListNodes(ctx)
	if err != nil {
		return err
	}
	if domain.ClusterProtocol(nodes, domain.ProtocolBranchRuntime) < domain.ProtocolBranchRuntime {
		return fmt.Errorf("%w: a branch can only run another runtime than its base once every node runs the new version", domain.ErrClusterUpgrading)
	}
	return nil
}

func (s *Service) upgrader(rt domain.RuntimeRef) (domain.DBServer, domain.Upgrader, error) {
	srv, err := s.server(rt)
	if err != nil {
		return nil, nil, err
	}
	up, ok := srv.(domain.Upgrader)
	if !ok {
		return nil, nil, fmt.Errorf("%w: %s servers cannot be upgraded", domain.ErrUnsupportedEngine, rt.Engine)
	}
	return srv, up, nil
}

// ValidateTarget checks that a branch on from may move to to on this node.
func (s *Service) ValidateTarget(ctx context.Context, from, to domain.RuntimeRef) error {
	if err := domain.CheckUpgradePath(from, to); err != nil {
		return err
	}
	if _, _, err := s.upgrader(from); err != nil {
		return err
	}
	return s.requireInstalled(ctx, to)
}

// ValidateUpgrade answers at once what an upgrade request would be refused
// for, so the API can say so instead of failing an operation.
func (s *Service) ValidateUpgrade(ctx context.Context, n domain.Name, to domain.RuntimeRef, checkOnly bool) (domain.Branch, error) {
	br, err := s.ownBranch(ctx, n)
	if err != nil {
		return br, err
	}
	if err := br.CanUpgrade(); err != nil {
		return br, err
	}
	if err := s.ValidateTarget(ctx, br.Runtime, to); err != nil {
		return br, err
	}
	if checkOnly {
		return br, nil
	}
	if err := s.requireLineage(); err != nil {
		return br, err
	}
	return br, s.clusterAllowsBranchRuntime(ctx)
}

func newResult(br domain.Branch, req UpgradeRequest) domain.UpgradeResult {
	return domain.UpgradeResult{Branch: br.Name.String(), From: br.Runtime, To: req.To, CheckOnly: req.CheckOnly, PreCheck: []domain.Finding{}, Log: []domain.Finding{}}
}

// UpgradeBranch runs the upgrade (or only its pre-check). The result is
// returned with the error too: it says what happened and what was found.
func (s *Service) UpgradeBranch(ctx context.Context, req UpgradeRequest, report Reporter) (domain.UpgradeResult, error) {
	br, err := s.beginUpgrade(ctx, req)
	if err != nil {
		return domain.UpgradeResult{}, err
	}
	res := newResult(br, req)
	srv, up, _ := s.upgrader(br.Runtime) // beginUpgrade has checked that there is one
	report.report("checking")
	plan, findings, err := s.preCheck(ctx, br, req, up)
	res.PreCheck = findings
	switch {
	case err != nil:
		return res, errors.Join(err, s.settleUpgrade(ctx, br, req))
	case req.CheckOnly:
		res.Status = domain.UpgradeChecked
		return res, nil
	case domain.HasErrors(findings) && !req.Force:
		res.Status = domain.UpgradeRefused
		return res, errors.Join(fmt.Errorf("%w: the pre-check found errors; fix them or upgrade with --force", domain.ErrUpgradeRefused), s.settleUpgrade(ctx, br, req))
	}
	return s.doUpgrade(ctx, br, req, upgradeTools{srv, up, plan}, res, report)
}

// beginUpgrade validates the request and, unless it only checks, reserves the branch.
func (s *Service) beginUpgrade(ctx context.Context, req UpgradeRequest) (domain.Branch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	br, err := s.ValidateUpgrade(ctx, req.Name, req.To, req.CheckOnly)
	if err != nil || req.CheckOnly {
		return br, err
	}
	br.Status = domain.BranchUpgrading
	return br, s.Store.PutBranch(ctx, br)
}

// settleUpgrade puts a branch whose upgrade did not start back to ready.
func (s *Service) settleUpgrade(ctx context.Context, br domain.Branch, req UpgradeRequest) error {
	if req.CheckOnly {
		return nil
	}
	br.Status, br.Error = domain.BranchReady, ""
	return s.saveBranch(ctx, br)
}

const (
	// upgradeSpaceBase and upgradeSpaceShare estimate the space an upgrade
	// needs besides the checkpoint (which costs nothing until data changes):
	// the data dictionary and redo log are rewritten.
	upgradeSpaceBase  = 512 << 20
	upgradeSpaceShare = 20 // one twentieth of the branch's data
)

func (s *Service) preCheck(ctx context.Context, br domain.Branch, req UpgradeRequest, up domain.Upgrader) (domain.UpgradePlan, []domain.Finding, error) {
	out := []domain.Finding{{Level: domain.LevelInfo, Code: domain.CodeUpgradePath, Message: fmt.Sprintf("%s to %s on node %s", br.Runtime, req.To, s.Node)}}
	ds := s.branchDataset(br.Name)
	if f, err := s.spaceFinding(ctx, ds, req.PoolFree); err != nil {
		return domain.UpgradePlan{}, out, err
	} else if f.Code != "" {
		out = append(out, f)
	}
	if err := s.ensureRunning(ctx, br); err != nil {
		return domain.UpgradePlan{}, out, fmt.Errorf("the branch's server is not running: %w", err)
	}
	spec, err := s.branchSpec(ctx, br, ds, false, false)
	if err != nil {
		return domain.UpgradePlan{}, out, err
	}
	plan, err := up.CheckUpgrade(ctx, spec, req.To, domain.Login{User: br.User, Password: br.Password})
	if err != nil {
		return plan, out, fmt.Errorf("pre-check: %w", err)
	}
	return plan, append(out, plan.Findings...), nil
}

func (s *Service) spaceFinding(ctx context.Context, ds string, free uint64) (domain.Finding, error) {
	info, err := s.Storage.Info(ctx, ds)
	if err != nil {
		return domain.Finding{}, err
	}
	need := info.Referenced/upgradeSpaceShare + upgradeSpaceBase
	if free == 0 || free == math.MaxUint64 {
		return domain.Finding{Level: domain.LevelInfo, Code: domain.CodeDiskSpace, Message: fmt.Sprintf("The upgrade needs about %s of free space; this node's free space is unknown.", humanBytes(need))}, nil
	}
	f := domain.Finding{Level: domain.LevelInfo, Code: domain.CodeDiskSpace}
	switch {
	case free < need:
		f.Level = domain.LevelError
		f.Message = fmt.Sprintf("The upgrade needs about %s of free space and the pool has %s.", humanBytes(need), humanBytes(free))
	case free < 2*need:
		f.Level = domain.LevelWarning
		f.Message = fmt.Sprintf("The upgrade needs about %s of free space and the pool has only %s.", humanBytes(need), humanBytes(free))
	default:
		f.Message = fmt.Sprintf("The upgrade needs about %s of free space; the pool has %s.", humanBytes(need), humanBytes(free))
	}
	return f, nil
}

func humanBytes(n uint64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	v, i := float64(n), 0
	for v >= 1024 && i < len(units)-1 {
		v, i = v/1024, i+1
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

// upgradeIntent is the record of an upgrade in progress.
type upgradeIntent struct {
	From, To   domain.RuntimeRef
	Checkpoint string
}

func (in upgradeIntent) value() string {
	return domain.RuntimeProp(in.From) + "/" + domain.RuntimeProp(in.To) + "/" + in.Checkpoint
}

func parseUpgradeIntent(v string) (upgradeIntent, bool) {
	parts := strings.Split(v, "/")
	if len(parts) != 3 || parts[2] == "" {
		return upgradeIntent{}, false
	}
	from, ok1 := domain.ParseRuntimeProp(parts[0])
	to, ok2 := domain.ParseRuntimeProp(parts[1])
	return upgradeIntent{From: from, To: to, Checkpoint: parts[2]}, ok1 && ok2
}

func (s *Service) writeUpgradeIntent(ctx context.Context, n domain.Name, in upgradeIntent) error {
	root := s.historyRoot(n)
	ok, err := s.Storage.Exists(ctx, root)
	if err != nil {
		return err
	}
	if !ok {
		if err := s.Storage.Create(ctx, root, map[string]string{"canmount": "off"}); err != nil {
			return err
		}
	}
	return s.Lineage.SetProps(ctx, root, map[string]string{domain.PropUpgrade: in.value()})
}

func (s *Service) readUpgradeIntent(ctx context.Context, n domain.Name) (upgradeIntent, bool, error) {
	root := s.historyRoot(n)
	ok, err := s.Storage.Exists(ctx, root)
	if err != nil || !ok {
		return upgradeIntent{}, false, err
	}
	p, err := s.Lineage.Props(ctx, root)
	if err != nil {
		return upgradeIntent{}, false, err
	}
	in, ok := parseUpgradeIntent(p[domain.PropUpgrade])
	return in, ok, nil
}

func (s *Service) clearUpgradeIntent(ctx context.Context, n domain.Name) error {
	root := s.historyRoot(n)
	ok, err := s.Storage.Exists(ctx, root)
	if err != nil || !ok {
		return err
	}
	return s.Lineage.Unset(ctx, root, domain.PropUpgrade)
}

// upgradeTools is what an upgrade works with: the engine's server and its
// upgrade support, and what the pre-check found.
type upgradeTools struct {
	srv  domain.DBServer
	up   domain.Upgrader
	plan domain.UpgradePlan
}

// upgradeSetup reads what the upgrade needs before it changes anything: the
// old server's spec, the branch's live line and the checkpoint's name.
func (s *Service) upgradeSetup(ctx context.Context, br domain.Branch) (domain.Server, domain.Line, string, error) {
	oldSpec, err := s.branchSpec(ctx, br, s.branchDataset(br.Name), false, false)
	if err != nil {
		return oldSpec, domain.Line{}, "", err
	}
	lines, err := s.family(ctx, br.Name)
	if err != nil {
		return oldSpec, domain.Line{}, "", err
	}
	live, ok := liveOf(lines)
	if !ok {
		return oldSpec, live, "", fmt.Errorf("branch %s: %w: its dataset is missing", br.Name, domain.ErrNotFound)
	}
	return oldSpec, live, domain.UniqueCheckpointName(lines, domain.AutoCheckpointName("upgrade", s.Clock())), nil
}

func (s *Service) doUpgrade(ctx context.Context, br domain.Branch, req UpgradeRequest, tools upgradeTools, res domain.UpgradeResult, report Reporter) (domain.UpgradeResult, error) {
	from, ds := br.Runtime, s.branchDataset(br.Name)
	srv, up, plan := tools.srv, tools.up, tools.plan
	oldSpec, live, name, err := s.upgradeSetup(ctx, br)
	if err != nil {
		return res, errors.Join(err, s.settleUpgrade(ctx, br, req))
	}
	in := upgradeIntent{From: from, To: req.To, Checkpoint: name}

	// Until the checkpoint exists nothing has changed: put the branch back as it was.
	untouched := func(cause error) (domain.UpgradeResult, error) {
		res.Status, res.Error = domain.UpgradeAborted, cause.Error()
		errs := []error{cause, s.clearUpgradeIntent(ctx, br.Name), s.ensureRunning(ctx, br), s.settleUpgrade(ctx, br, req)}
		return res, errors.Join(errs...)
	}
	if err := s.writeUpgradeIntent(ctx, br.Name, in); err != nil {
		return untouched(err)
	}
	report.report("stopping")
	if err := srv.Stop(ctx, oldSpec, domain.StopClean); err != nil {
		return untouched(fmt.Errorf("the old server did not shut down cleanly: %w", err))
	}
	report.report("checkpointing")
	if err := s.ensureAuto(ctx, ds, name, live.Props, from); err != nil {
		return untouched(err)
	}
	res.Checkpoint = name

	prevLegacy := br.LegacyAuth
	br.Runtime, br.LegacyAuth = req.To, plan.LegacyAuth
	failure := s.saveBranch(ctx, br)
	var log domain.UpgradeLog
	var spec domain.Server
	if failure == nil {
		spec, failure = s.branchSpec(ctx, br, ds, false, false)
	}
	if failure == nil {
		report.report("upgrading")
		log, failure = up.StartUpgraded(ctx, spec, domain.Login{User: br.User, Password: br.Password})
	}
	if log.Findings != nil {
		res.Log = log.Findings
	}
	if failure != nil {
		res.Error, res.LogExcerpt = failure.Error(), log.Excerpt
		return s.failUpgrade(ctx, br, req, res, prevLegacy, failure, report)
	}
	// The server runs: forget the intent first, so a crash from here on keeps the new runtime.
	if err := s.clearUpgradeIntent(ctx, br.Name); err != nil {
		_ = srv.Stop(context.WithoutCancel(ctx), spec, domain.StopImmediate)
		return res, err
	}
	br.Status, br.Error = domain.BranchReady, ""
	if err := s.saveBranch(ctx, br); err != nil {
		_ = srv.Stop(context.WithoutCancel(ctx), spec, domain.StopImmediate)
		return res, err
	}
	res.Status = domain.UpgradeUpgraded
	s.rotateAuto(ctx, br.Name)
	return res, nil
}

// failUpgrade deals with an upgrade that failed after the checkpoint was saved.
func (s *Service) failUpgrade(ctx context.Context, br domain.Branch, req UpgradeRequest, res domain.UpgradeResult, prevLegacy bool, cause error, report Reporter) (domain.UpgradeResult, error) {
	srv, _ := s.server(br.Runtime)
	_ = srv.Stop(context.WithoutCancel(ctx), domain.Server{ID: branchServerID(br.Name), Runtime: br.Runtime}, domain.StopImmediate)
	if req.NoRollback {
		msg := fmt.Sprintf("%s%v; the branch was left as it is on %s; restore checkpoint %s to go back to %s, or delete the branch", domain.UpgradeFailedPrefix, cause, br.Runtime, res.Checkpoint, res.From)
		res.Status = domain.UpgradeFailed
		return res, errors.Join(errors.New(msg), s.clearUpgradeIntent(ctx, br.Name), s.markFailed(ctx, br, errors.New(msg)))
	}
	report.report("rolling_back")
	back := br
	back.LegacyAuth = prevLegacy
	rolled, err := s.rollbackUpgrade(ctx, back, res.From, res.Checkpoint, report)
	if err != nil {
		res.Status = domain.UpgradeFailed
		msg := fmt.Sprintf("%supgrade to %s failed (%v) and the rollback to %s failed too: %v", domain.UpgradeFailedPrefix, req.To, cause, res.Checkpoint, err)
		return res, errors.Join(errors.New(msg), s.markFailed(ctx, rolled, errors.New(msg)))
	}
	res.Status = domain.UpgradeRolledBack
	return res, fmt.Errorf("upgrade to %s failed and the branch was rolled back to %s on %s: %w", req.To, res.Checkpoint, res.From, cause)
}

// rollbackUpgrade restores the checkpoint an upgrade saved; the restore
// switches the branch back to the runtime the checkpoint records.
func (s *Service) rollbackUpgrade(ctx context.Context, br domain.Branch, from domain.RuntimeRef, checkpoint string, report Reporter) (domain.Branch, error) {
	lines, err := s.family(ctx, br.Name)
	if err != nil {
		return br, err
	}
	in := restoreIntent{
		Op: opRestore, Source: domain.CheckpointSnapshot(checkpoint), NewLine: domain.NextLine(lines),
		Auto: domain.UniqueCheckpointName(lines, domain.AutoCheckpointName("restore", s.Clock())), Runtime: from,
	}
	if err := s.requireInstalled(ctx, from); err != nil {
		return br, err
	}
	if err := s.writeIntent(ctx, br.Name, in); err != nil {
		return br, err
	}
	// The restore intent now carries the rollback; an agent restart finishes it as a restore.
	br.Status = domain.BranchRestoring
	if err := s.saveBranch(ctx, br); err != nil {
		return br, err
	}
	if err := s.clearUpgradeIntent(ctx, br.Name); err != nil {
		return br, err
	}
	return s.runRestore(ctx, br, in, report)
}

// resumeUpgrade settles an upgrade the agent was interrupted in: when the
// checkpoint was saved it is rolled back, else nothing had changed.
func (s *Service) resumeUpgrade(ctx context.Context, br domain.Branch) error {
	in, ok, err := s.readUpgradeIntent(ctx, br.Name)
	if err != nil {
		return err
	}
	if ok {
		lines, err := s.family(ctx, br.Name)
		if err != nil {
			return err
		}
		if _, _, saved := domain.FindCheckpoint(lines, in.Checkpoint); saved {
			_, err := s.rollbackUpgrade(ctx, br, in.From, in.Checkpoint, nil)
			return err
		}
		br.Runtime = in.From
	}
	if err := s.ensureRunning(ctx, br); err != nil {
		return err
	}
	if ok {
		if err := s.clearUpgradeIntent(ctx, br.Name); err != nil {
			return err
		}
	}
	br.Status, br.Error = domain.BranchReady, ""
	return s.saveBranch(ctx, br)
}

// CreateBranchUpgraded creates a branch and upgrades it right away when the
// request names a runtime (the result is nil otherwise). A branch whose
// upgrade fails stays, on its base's runtime when it was rolled back.
func (s *Service) CreateBranchUpgraded(ctx context.Context, req CreateBranchRequest, report Reporter) (domain.Branch, *domain.UpgradeResult, error) {
	if req.Runtime == (domain.RuntimeRef{}) {
		br, err := s.CreateBranch(ctx, req, report)
		return br, nil, err
	}
	base, err := s.Store.GetBase(ctx, req.Base)
	if err != nil {
		return domain.Branch{}, nil, err
	}
	if err := s.ValidateTarget(ctx, base.Runtime, req.Runtime); err != nil {
		return domain.Branch{}, nil, err
	}
	if err := s.clusterAllowsBranchRuntime(ctx); err != nil {
		return domain.Branch{}, nil, err
	}
	br, err := s.CreateBranch(ctx, req, report)
	if err != nil {
		return br, nil, err
	}
	res, err := s.UpgradeBranch(ctx, UpgradeRequest{Name: br.Name, To: req.Runtime, Force: req.Force, PoolFree: req.PoolFree}, report)
	if cur, gerr := s.Store.GetBranch(ctx, br.Name); gerr == nil {
		br = cur
	}
	return br, &res, err
}
