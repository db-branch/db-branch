package dbbranch

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

// MergeBranchRequest asks to logically 3-way merge a source line (or checkpoint)
// within a branch into the branch's current live line.
type MergeBranchRequest struct {
	Name           domain.Name
	SourceLine     *int
	Checkpoint     string
	DryRun         bool
	Tables         []string
	AllowDDL       bool
	IgnoreTriggers bool
}

const (
	// mergeTimeout bounds the 3-way diff and apply phase (matches docs/branch-merge.md §7).
	mergeTimeout = 15 * time.Minute
	// mergeCloneMemMB caps each temporary clone server's buffer pool / shared buffers.
	mergeCloneMemMB = 64
	// mergeCloneOverheadMB estimates non-buffer memory overhead for a temporary clone server.
	mergeCloneOverheadMB = 64
)

var mergeIDSeq uint64

func (s *Service) newMergeShortID() string {
	n := atomic.AddUint64(&mergeIDSeq, 1)
	v := ((uint64(s.Clock().Unix()) & 0xffff) << 16) | (n & 0xffff)
	return strconv.FormatUint(v, 36)
}

func (s *Service) workRoot() string { return s.Pool + "/db-branch/work" }

func (s *Service) checkNotDryRun(n domain.Name) error {
	s.dryRunMu.Lock()
	active := s.mergingDryRun[n]
	s.dryRunMu.Unlock()
	if active {
		return fmt.Errorf("%w: branch %s has a merge dry-run in progress", domain.ErrConflict, n)
	}
	return nil
}

func (s *Service) setDryRun(n domain.Name, on bool) {
	s.dryRunMu.Lock()
	defer s.dryRunMu.Unlock()
	if s.mergingDryRun == nil {
		s.mergingDryRun = map[domain.Name]bool{}
	}
	if on {
		s.mergingDryRun[n] = true
		return
	}
	delete(s.mergingDryRun, n)
}

// clusterAllowsBranchMerge reports whether every cluster node speaks ProtocolBranchMerge.
func (s *Service) clusterAllowsBranchMerge(ctx context.Context) error {
	if s.Nodes == nil {
		return nil
	}
	nodes, err := s.Nodes.ListNodes(ctx)
	if err != nil {
		return err
	}
	if domain.ClusterProtocol(nodes, domain.ProtocolBranchMerge) < domain.ProtocolBranchMerge {
		return fmt.Errorf("%w: branch merge is available once every node runs the new version", domain.ErrClusterUpgrading)
	}
	return nil
}

// checkMergeCapacity checks that this node has memory budget to run the 3
// temporary maintenance clone instances needed by a 3-way merge.
func (s *Service) checkMergeCapacity(ctx context.Context, br domain.Branch, dryRun bool) error {
	nodes, err := s.nodes(ctx)
	if err != nil {
		return err
	}
	targetNode := br.Node
	if targetNode == "" {
		targetNode = s.Node
	}
	var hostNode *domain.Node
	for i := range nodes {
		if nodes[i].Name == targetNode {
			hostNode = &nodes[i]
			break
		}
	}
	if hostNode == nil || hostNode.MemoryMB == 0 {
		return nil
	}
	branches, err := s.Store.ListBranches(ctx)
	if err != nil {
		return err
	}
	running := 0
	for _, b := range branches {
		bNode := b.Node
		if bNode == "" {
			bNode = s.Node
		}
		if bNode == targetNode && b.Status != domain.BranchPaused {
			running++
		}
	}
	if !dryRun && br.Status == domain.BranchReady && running > 0 {
		// In normal mode the live branch server is stopped cleanly before the 3 temp clones start.
		running--
	}
	usedMB := running * (s.memory() + ServerOverheadMB)
	cloneMem := min(max(s.memory(), mergeCloneMemMB), mergeCloneMemMB)
	tempMB := 3 * (cloneMem + mergeCloneOverheadMB)
	if float64(usedMB+tempMB) > hostNode.BranchBudgetMB() {
		return fmt.Errorf("%w: node %s lacks memory for 3 temporary merge instances (%d MiB needed)", domain.ErrNoCapacity, targetNode, tempMB)
	}
	return nil
}

// ValidateMerge checks all synchronous preconditions for a MergeBranch request.
func (s *Service) ValidateMerge(ctx context.Context, req MergeBranchRequest) (domain.Branch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.validateMergeLocked(ctx, req)
}

func (s *Service) validateMergeLocked(ctx context.Context, req MergeBranchRequest) (domain.Branch, error) {
	if (req.SourceLine == nil && req.Checkpoint == "") || (req.SourceLine != nil && req.Checkpoint != "") {
		return domain.Branch{}, fmt.Errorf("%w: specify either source_line or checkpoint, not both", domain.ErrInvalidMergeSource)
	}
	if req.SourceLine != nil && *req.SourceLine < 0 {
		return domain.Branch{}, fmt.Errorf("%w: source_line must be >= 0", domain.ErrInvalidMergeSource)
	}
	if req.Checkpoint != "" && req.Checkpoint != domain.ResetPoint {
		if _, err := domain.ParseCheckpointName(req.Checkpoint); err != nil {
			return domain.Branch{}, fmt.Errorf("%w: %v", domain.ErrInvalidMergeSource, err)
		}
	}
	if err := s.requireLineage(); err != nil {
		return domain.Branch{}, err
	}
	if err := s.clusterAllowsBranchMerge(ctx); err != nil {
		return domain.Branch{}, err
	}
	br, err := s.ownBranch(ctx, req.Name)
	if err != nil {
		return br, err
	}
	if err := s.checkNotDryRun(br.Name); err != nil {
		return br, err
	}
	if err := br.CanMerge(); err != nil {
		return br, err
	}
	if err := s.checkPoolRoom(ctx); err != nil {
		return br, err
	}
	if err := s.checkMergeCapacity(ctx, br, req.DryRun); err != nil {
		return br, err
	}
	if _, err := s.server(br.Runtime); err != nil {
		return br, err
	}
	return br, nil
}

// mergeIntent persists the pre-merge state and rollback checkpoint name in PropMerge
// on <pool>/db-branch/history/<name>.
// Format: <prevStatus>/<checkpoint|->/<newLine|->/<runtimeProp|->/<prevHead|->
type mergeIntent struct {
	PrevStatus domain.BranchStatus
	Checkpoint string
	NewLine    int
	Runtime    domain.RuntimeRef
	PrevHead   string
}

func (in mergeIntent) value() string {
	ckpt := in.Checkpoint
	if ckpt == "" {
		ckpt = "-"
	}
	nl := "-"
	if in.NewLine > 0 {
		nl = strconv.Itoa(in.NewLine)
	}
	return fmt.Sprintf("%s/%s/%s/%s/%s",
		in.PrevStatus, ckpt, nl, orNone(domain.RuntimeProp(in.Runtime)), orNone(in.PrevHead))
}

func parseMergeIntent(v string) (mergeIntent, bool) {
	parts := strings.Split(v, "/")
	if len(parts) != 5 {
		return mergeIntent{}, false
	}
	prev := domain.BranchStatus(parts[0])
	if prev != domain.BranchReady && prev != domain.BranchPaused {
		return mergeIntent{}, false
	}
	ckpt := parts[1]
	if ckpt == "-" {
		ckpt = ""
	}
	var nl int
	if parts[2] != "-" {
		var err error
		nl, err = strconv.Atoi(parts[2])
		if err != nil || nl <= 0 {
			return mergeIntent{}, false
		}
	}
	rt, _ := domain.ParseRuntimeProp(parts[3])
	head := parts[4]
	if head == "-" {
		head = ""
	}
	return mergeIntent{PrevStatus: prev, Checkpoint: ckpt, NewLine: nl, Runtime: rt, PrevHead: head}, true
}

func (s *Service) writeMergeIntent(ctx context.Context, n domain.Name, in mergeIntent) error {
	return s.setHistoryProps(ctx, n, map[string]string{domain.PropMerge: in.value()})
}

func (s *Service) readMergeIntent(ctx context.Context, n domain.Name) (mergeIntent, bool, error) {
	if s.Lineage == nil {
		return mergeIntent{}, false, nil
	}
	root := s.historyRoot(n)
	ok, err := s.Storage.Exists(ctx, root)
	if err != nil || !ok {
		return mergeIntent{}, false, err
	}
	p, err := s.Lineage.Props(ctx, root)
	if err != nil {
		return mergeIntent{}, false, err
	}
	in, ok := parseMergeIntent(p[domain.PropMerge])
	return in, ok, nil
}

func (s *Service) clearMergeIntent(ctx context.Context, n domain.Name) error {
	return s.unsetHistoryProps(ctx, n, domain.PropMerge)
}

func mergeErrorCode(err error) string {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return "not_found"
	case errors.Is(err, domain.ErrInvalidMergeSource):
		return "invalid_request"
	case errors.Is(err, domain.ErrNoCommonAncestor):
		return "no_common_ancestor"
	case errors.Is(err, domain.ErrRuntimeNotInstalled):
		return "runtime_not_installed"
	case errors.Is(err, domain.ErrSchemaNoPrimaryKey):
		return "schema_no_primary_key"
	case errors.Is(err, domain.ErrTriggersNotSupported):
		return "triggers_not_supported"
	case errors.Is(err, domain.ErrMergeConflict):
		return "merge_conflict"
	default:
		return "merge_failed"
	}
}

func (s *Service) beginMerge(ctx context.Context, req MergeBranchRequest) (domain.Branch, domain.BranchStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	br, err := s.validateMergeLocked(ctx, req)
	if err != nil {
		return br, "", err
	}
	if s.Merger == nil {
		return br, "", fmt.Errorf("logical merger is not configured")
	}
	prevStatus := br.Status
	if req.DryRun {
		s.setDryRun(br.Name, true)
		return br, prevStatus, nil
	}
	if err := s.writeMergeIntent(ctx, br.Name, mergeIntent{PrevStatus: prevStatus, Runtime: br.Runtime}); err != nil {
		return br, prevStatus, err
	}
	br.Status = domain.BranchMerging
	if err := s.Store.PutBranch(ctx, br); err != nil {
		_ = s.clearMergeIntent(context.WithoutCancel(ctx), br.Name)
		return br, prevStatus, err
	}
	return br, prevStatus, nil
}

// MergeBranch merges changes from req.SourceLine (or req.Checkpoint) into the
// current live line of branch req.Name via 3-way logical comparison.
func (s *Service) MergeBranch(ctx context.Context, req MergeBranchRequest, report Reporter) (domain.MergeResult, error) {
	br, prevStatus, err := s.beginMerge(ctx, req)
	if err != nil {
		return domain.MergeResult{}, err
	}
	if req.DryRun {
		defer s.setDryRun(br.Name, false)
	}

	res := domain.MergeResult{
		Branch:           br.Name.String(),
		SourceCheckpoint: req.Checkpoint,
		DryRun:           req.DryRun,
	}
	if req.SourceLine != nil {
		res.SourceLine = *req.SourceLine
	}

	mergeCtx, cancel := context.WithTimeout(ctx, mergeTimeout)
	defer cancel()

	// Helper to restore the branch state untouched when diff/validation fails before Apply.
	restoreUntouched := func(cause error) (domain.MergeResult, error) {
		res.Status = domain.MergeFailed
		res.Code = mergeErrorCode(cause)
		res.Error = cause.Error()
		if req.DryRun {
			return res, cause
		}
		wctx := context.WithoutCancel(ctx)
		var restoreErrs []error
		if prevStatus == domain.BranchReady {
			restoreErrs = append(restoreErrs, s.ensureRunning(wctx, br))
		}
		br.Status = prevStatus
		if err := s.saveBranch(wctx, br); err != nil {
			restoreErrs = append(restoreErrs, err)
		} else {
			restoreErrs = append(restoreErrs, s.clearMergeIntent(wctx, br.Name))
		}
		s.collectIfSettled(wctx, br.Name)
		return res, errors.Join(append([]error{cause}, restoreErrs...)...)
	}

	// Step 1: In non-dry-run mode, stop running live server BEFORE taking live snapshot to prevent TOCTOU writes.
	srv, err := s.server(br.Runtime)
	if err != nil {
		return restoreUntouched(err)
	}
	liveDS := s.branchDataset(br.Name)
	if !req.DryRun && prevStatus == domain.BranchReady {
		spec, err := s.branchSpec(mergeCtx, br, liveDS, false, false)
		if err != nil {
			return restoreUntouched(err)
		}
		report.report("stopping")
		if err := srv.Stop(mergeCtx, spec, domain.StopClean); err != nil {
			return restoreUntouched(fmt.Errorf("clean stop before merge: %w", err))
		}
	}

	// Step 2: Resolve lineage plan (Source + Common Ancestor).
	lines, err := s.family(mergeCtx, br.Name)
	if err != nil {
		return restoreUntouched(err)
	}
	liveLine, ok := liveOf(lines)
	if !ok {
		return restoreUntouched(fmt.Errorf("live dataset of branch %s: %w", br.Name, domain.ErrNotFound))
	}
	lPlan, err := domain.ResolveLineAncestor(lines, req.SourceLine, req.Checkpoint)
	if err != nil {
		return restoreUntouched(err)
	}
	res.SourceLine = lPlan.SourceLine
	res.TargetLine = lPlan.TargetLine
	res.Ancestor = lPlan.AncestorLabel

	// Determine runtimes for A, S, T clones and verify they are installed.
	rtA := s.runtimeOf(mergeCtx, br, lPlan.AncestorRuntime)
	rtS := s.runtimeOf(mergeCtx, br, lPlan.SourceRuntime)
	rtT := br.Runtime
	for _, rt := range []domain.RuntimeRef{rtA, rtS, rtT} {
		if err := s.requireInstalled(mergeCtx, rt); err != nil {
			return restoreUntouched(err)
		}
	}

	// Step 3: Snapshot live (and source dataset if tip) + clone A, S, T into temporary work clones and Diff.
	plan, diffErr := s.runCloneDiff(mergeCtx, br, req, lPlan, liveLine, rtA, rtS, rtT, report)
	if diffErr != nil {
		return restoreUntouched(diffErr)
	}

	res.Inserted = plan.Inserted
	res.Updated = plan.Updated
	res.Deleted = plan.Deleted
	res.DDL = plan.DDL
	res.Tables = plan.Tables
	res.TotalConflicts = plan.TotalConflicts
	res.Truncated = plan.Truncated
	res.Conflicts = plan.Conflicts

	if plan.TotalConflicts > 0 {
		res.Status = domain.MergeConflicted
		res.Code = "merge_conflict"
		if plan.Message != "" {
			res.Error = plan.Message
		} else {
			res.Error = fmt.Sprintf("merge aborted: %d conflict(s) detected", plan.TotalConflicts)
		}
		if !req.DryRun {
			wctx := context.WithoutCancel(ctx)
			if prevStatus == domain.BranchReady {
				if serr := s.ensureRunning(wctx, br); serr != nil {
					res.Error = fmt.Sprintf("%s; restart failed: %v", res.Error, serr)
				}
			}
			br.Status = prevStatus
			if s.saveBranch(wctx, br) == nil {
				_ = s.clearMergeIntent(wctx, br.Name)
			}
			s.collectIfSettled(wctx, br.Name)
		}
		return res, fmt.Errorf("%w: %s", domain.ErrMergeConflict, res.Error)
	}

	if req.DryRun {
		res.Status = domain.MergePreview
		return res, nil
	}

	// Step 4: Take automatic pre-merge checkpoint and persist merge intent for atomic rollback.
	ckptName := domain.UniqueCheckpointName(lines, domain.AutoCheckpointName("merge", s.Clock()))
	in := mergeIntent{
		PrevStatus: prevStatus,
		Checkpoint: ckptName,
		NewLine:    domain.NextLine(lines),
		Runtime:    br.Runtime,
		PrevHead:   liveLine.Props[domain.PropHead],
	}
	if err := s.writeMergeIntent(mergeCtx, br.Name, in); err != nil {
		return restoreUntouched(err)
	}
	report.report("checkpointing")
	if err := s.ensureAuto(mergeCtx, liveDS, ckptName, liveLine.Props, br.Runtime); err != nil {
		return restoreUntouched(err)
	}
	res.Checkpoint = ckptName

	// Step 5: Apply DDL/DML to live dataset in maintenance mode, update login ownership, then clean stop.
	maintSpec, err := s.branchSpec(mergeCtx, br, liveDS, true, false)
	if err != nil {
		return s.failMergeAndRollback(ctx, br, in, res, err, report)
	}
	report.report("applying")
	if err := srv.Start(mergeCtx, maintSpec); err != nil {
		return s.failMergeAndRollback(ctx, br, in, res, err, report)
	}
	applyErr := s.Merger.Apply(mergeCtx, srv.Admin(maintSpec), plan, func(phase string) {
		report.report(phase)
	})
	if applyErr == nil {
		applyErr = s.putLoginRight(mergeCtx, br, srv, maintSpec)
	}
	stopErr := s.stopServer(context.WithoutCancel(ctx), srv, maintSpec)
	if applyErr != nil || stopErr != nil {
		return s.failMergeAndRollback(ctx, br, in, res, errors.Join(applyErr, stopErr), report)
	}

	// Step 6: Restore branch to pre-merge status, then clear merge intent.
	if prevStatus == domain.BranchReady {
		normalSpec, err := s.branchSpec(mergeCtx, br, liveDS, false, false)
		if err != nil {
			return s.failMergeAndRollback(ctx, br, in, res, err, report)
		}
		report.report("starting")
		if err := srv.Start(mergeCtx, normalSpec); err != nil {
			return s.failMergeAndRollback(ctx, br, in, res, err, report)
		}
	}
	br.Status, br.Error = prevStatus, ""
	if err := s.saveBranch(mergeCtx, br); err != nil {
		return res, err
	}
	_ = s.clearMergeIntent(mergeCtx, br.Name)
	res.Status = domain.MergeMerged
	s.rotateAuto(ctx, br.Name)
	s.collectIfSettled(ctx, br.Name)
	return res, nil
}

func (s *Service) runCloneDiff(
	ctx context.Context, br domain.Branch, req MergeBranchRequest,
	lPlan domain.LineMergePlan, liveLine domain.Line,
	rtA, rtS, rtT domain.RuntimeRef, report Reporter,
) (domain.MergePlan, error) {
	shortID := s.newMergeShortID()
	workRoot := s.workRoot()
	workDir := s.workDataset("merge-" + shortID)

	var createdWorkDir bool
	var tempSnaps []string
	var tempClones []string
	var tempServers []struct {
		srv  domain.DBServer
		spec domain.Server
	}

	defer func() {
		wctx := context.WithoutCancel(ctx)
		for i := len(tempServers) - 1; i >= 0; i-- {
			ts := tempServers[i]
			_ = ts.srv.Stop(wctx, ts.spec, domain.StopImmediate)
			_ = ts.srv.Remove(wctx, ts.spec)
		}
		for i := len(tempClones) - 1; i >= 0; i-- {
			_ = s.destroyIfExists(wctx, tempClones[i])
		}
		if createdWorkDir {
			_ = s.destroyIfExists(wctx, workDir)
		}
		if len(tempSnaps) > 0 {
			if curLines, ferr := s.family(wctx, br.Name); ferr == nil {
				for _, snName := range tempSnaps {
					if ds, ok := domain.FindSnapshot(curLines, snName); ok {
						_ = s.Storage.DestroySnapshot(wctx, domain.Snapshot{Dataset: ds, Name: snName})
					}
				}
			}
		}
	}()

	report.report("snapshotting")
	liveSnapName := "merge-live-" + shortID
	liveSnap := domain.Snapshot{Dataset: liveLine.Dataset, Name: liveSnapName}
	if err := s.Storage.Snapshot(ctx, liveSnap); err != nil {
		return domain.MergePlan{}, err
	}
	tempSnaps = append(tempSnaps, liveSnapName)

	srcSnap := lPlan.SourceSnapshot
	if srcSnap.Name == "" {
		srcSnapName := "merge-src-" + shortID
		srcSnap = domain.Snapshot{Dataset: lPlan.SourceDataset, Name: srcSnapName}
		if err := s.Storage.Snapshot(ctx, srcSnap); err != nil {
			return domain.MergePlan{}, err
		}
		tempSnaps = append(tempSnaps, srcSnapName)
	}

	// Ensure work root and work/merge-<shortID> container datasets exist (canmount=off).
	if ok, err := s.Storage.Exists(ctx, workRoot); err != nil {
		return domain.MergePlan{}, err
	} else if !ok {
		if err := s.Storage.Create(ctx, workRoot, map[string]string{"canmount": "off"}); err != nil {
			return domain.MergePlan{}, err
		}
	}
	if err := s.Storage.Create(ctx, workDir, map[string]string{"canmount": "off"}); err != nil {
		return domain.MergePlan{}, err
	}
	createdWorkDir = true

	// Allocate 3 distinct ports for A, S, T.
	s.mu.Lock()
	ports, err := s.allocatePorts(ctx, 3)
	s.mu.Unlock()
	if err != nil {
		return domain.MergePlan{}, err
	}

	report.report("starting_clones")
	startClone := func(suffix string, origin domain.Snapshot, rt domain.RuntimeRef, port int) (domain.AdminAccess, error) {
		srv, err := s.server(rt)
		if err != nil {
			return domain.AdminAccess{}, err
		}
		ds := workDir + "/" + suffix
		if err := s.Storage.Clone(ctx, origin, ds); err != nil {
			return domain.AdminAccess{}, err
		}
		tempClones = append(tempClones, ds)
		if err := s.applyEngineProps(ctx, rt, ds); err != nil {
			return domain.AdminAccess{}, err
		}
		if err := s.Lineage.SetProps(ctx, ds, map[string]string{"canmount": "on"}); err != nil {
			return domain.AdminAccess{}, err
		}
		if err := s.Lineage.Mount(ctx, ds); err != nil {
			return domain.AdminAccess{}, err
		}
		srvID := "m-" + shortID + "-" + suffix
		spec, err := s.spec(ctx, srvID, ds, rt, port)
		if err != nil {
			return domain.AdminAccess{}, err
		}
		spec.Maintenance = true
		spec.LegacyAuth = br.LegacyAuth && rt == br.Runtime
		spec.MemoryMB = min(max(s.memory(), mergeCloneMemMB), mergeCloneMemMB)
		tempServers = append(tempServers, struct {
			srv  domain.DBServer
			spec domain.Server
		}{srv: srv, spec: spec})
		if err := srv.PrepareClone(ctx, spec); err != nil {
			return domain.AdminAccess{}, err
		}
		if err := srv.Start(ctx, spec); err != nil {
			return domain.AdminAccess{}, err
		}
		return srv.Admin(spec), nil
	}

	accA, err := startClone("a", lPlan.AncestorSnapshot, rtA, ports[0])
	if err != nil {
		return domain.MergePlan{}, err
	}
	accS, err := startClone("s", srcSnap, rtS, ports[1])
	if err != nil {
		return domain.MergePlan{}, err
	}
	accT, err := startClone("t", liveSnap, rtT, ports[2])
	if err != nil {
		return domain.MergePlan{}, err
	}

	report.report("diffing")
	return s.Merger.Diff(ctx, domain.MergeDiffRequest{
		Ancestor:       accA,
		Source:         accS,
		Target:         accT,
		Tables:         req.Tables,
		AllowDDL:       req.AllowDDL,
		IgnoreTriggers: req.IgnoreTriggers,
	})
}

func (s *Service) failMergeAndRollback(
	ctx context.Context, br domain.Branch, in mergeIntent,
	res domain.MergeResult, cause error, report Reporter,
) (domain.MergeResult, error) {
	wctx := context.WithoutCancel(ctx)
	res.Error = cause.Error()
	res.Code = mergeErrorCode(cause)
	if rerr := s.rollbackMerge(wctx, br, in, report); rerr != nil {
		res.Status = domain.MergeFailed
		msg := fmt.Sprintf("merge failed (%v) and rollback to %s failed: %v", cause, in.Checkpoint, rerr)
		res.Error = msg
		return res, errors.Join(errors.New(msg), s.markFailed(wctx, br, errors.New(msg)))
	}
	res.Status = domain.MergeRolledBack
	return res, fmt.Errorf("merge failed and branch was rolled back to %s: %w", in.Checkpoint, cause)
}

// rollbackMerge idempotently swaps before-merge-<time> back into branches/<name>
// and returns the branch to in.PrevStatus (ready or paused) while staying in
// BranchMerging until complete.
func (s *Service) rollbackMerge(ctx context.Context, br domain.Branch, in mergeIntent, report Reporter) error {
	report.report("rolling_back")
	_ = s.stopBranchImmediate(ctx, br)

	srv, err := s.server(in.Runtime)
	if err != nil {
		return err
	}
	restIn := restoreIntent{
		Op:      opRestore,
		Source:  domain.CheckpointSnapshot(in.Checkpoint),
		NewLine: in.NewLine,
		Runtime: in.Runtime,
	}
	liveDS := s.branchDataset(br.Name)
	done, exists, err := s.swapped(ctx, br.Name, restIn)
	if err != nil {
		return err
	}
	if !done {
		if err := s.setAside(ctx, br, restIn, srv, exists, nil); err != nil {
			return err
		}
	}
	if err := s.finishDatasetSwap(ctx, br, liveDS, in.NewLine, in.PrevHead, nil); err != nil {
		return err
	}
	br.Runtime = in.Runtime
	spec, err := s.branchSpec(ctx, br, liveDS, false, false)
	if err != nil {
		return err
	}
	if err := srv.PrepareClone(ctx, spec); err != nil {
		return err
	}
	if in.PrevStatus == domain.BranchReady {
		if err := s.ensureRunning(ctx, br); err != nil {
			return err
		}
	} else {
		_ = s.stopBranchImmediate(ctx, br)
	}
	br.Status, br.Error = in.PrevStatus, ""
	if err := s.saveBranch(ctx, br); err != nil {
		return err
	}
	if err := s.clearMergeIntent(ctx, br.Name); err != nil {
		return err
	}
	s.collectIfSettled(ctx, br.Name)
	return nil
}

// resumeFailed finishes what a failed branch still owes: a merge whose
// rollback did not complete (its intent is still recorded) or, else, a
// restore that failed after the swap began.
func (s *Service) resumeFailed(ctx context.Context, br domain.Branch) error {
	if _, ok, err := s.readMergeIntent(ctx, br.Name); err != nil {
		return err
	} else if ok {
		return s.resumeMerge(ctx, br)
	}
	return s.resumeFailedRestore(ctx, br)
}

// resumeMerge recovers a branch left in BranchMerging across an agent restart.
func (s *Service) resumeMerge(ctx context.Context, br domain.Branch) error {
	in, ok, err := s.readMergeIntent(ctx, br.Name)
	if err != nil {
		return err
	}
	if ok && in.Checkpoint != "" {
		lines, err := s.family(ctx, br.Name)
		if err != nil {
			return err
		}
		if _, _, saved := domain.FindCheckpoint(lines, in.Checkpoint); saved {
			return s.rollbackMerge(ctx, br, in, nil)
		}
	}
	// Interrupted before the pre-merge checkpoint was taken: live data was not touched.
	prev := domain.BranchReady
	if ok {
		prev = in.PrevStatus
	}
	if prev == domain.BranchReady {
		if err := s.recoverRunning(ctx, br, false); err != nil {
			return err
		}
	} else {
		_ = s.stopBranchImmediate(ctx, br)
	}
	br.Status, br.Error = prev, ""
	if err := s.saveBranch(ctx, br); err != nil {
		return err
	}
	if err := s.clearMergeIntent(ctx, br.Name); err != nil {
		return err
	}
	s.collectIfSettled(ctx, br.Name)
	return nil
}

// sweepMergeWork removes leftover work/merge-* clone directories/servers and
// uncloned @merge-* temporary snapshots left by an agent crash.
func (s *Service) sweepMergeWork(ctx context.Context) {
	if s.Lineage == nil {
		return
	}
	root := s.workRoot()
	ok, err := s.Storage.Exists(ctx, root)
	if err != nil || !ok {
		return
	}
	dirs, err := s.Lineage.Datasets(ctx, root)
	if err != nil {
		s.logf("sweeping merge work directory: %v", err)
		return
	}
	for _, ds := range dirs {
		base := path.Base(ds)
		if !strings.HasPrefix(base, "merge-") {
			continue
		}
		shortID := strings.TrimPrefix(base, "merge-")
		if children, err := s.Lineage.Datasets(ctx, ds); err == nil {
			for _, ch := range children {
				suffix := path.Base(ch)
				srvID := "m-" + shortID + "-" + suffix
				for _, srv := range s.Servers {
					_ = srv.Stop(ctx, domain.Server{ID: srvID}, domain.StopImmediate)
					_ = srv.Remove(ctx, domain.Server{ID: srvID})
				}
				_ = s.destroyIfExists(ctx, ch)
			}
		}
		_ = s.destroyIfExists(ctx, ds)
	}
}

// orphanedMergeSnapshots returns uncloned @merge-* snapshots to reap once branch n is settled.
func (s *Service) orphanedMergeSnapshots(ctx context.Context, n domain.Name, lines []domain.Line, dropping []string) []domain.Snapshot {
	if err := s.checkNotDryRun(n); err != nil {
		return nil
	}
	if br, err := s.Store.GetBranch(ctx, n); err == nil && br.Status == domain.BranchMerging {
		return nil
	}
	dropSet := make(map[string]bool, len(dropping))
	for _, ds := range dropping {
		dropSet[ds] = true
	}
	var out []domain.Snapshot
	for _, l := range lines {
		if l.Pending || dropSet[l.Dataset] {
			continue
		}
		for _, sn := range l.Snapshots {
			if domain.IsMergeSnapshot(sn.Name) && len(sn.Clones) == 0 {
				out = append(out, domain.Snapshot{Dataset: l.Dataset, Name: sn.Name})
			}
		}
	}
	return out
}
