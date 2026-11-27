package postgres

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

var (
	_ domain.Upgrader            = (*Server)(nil)
	_ domain.MajorUpgrader       = (*Server)(nil)
	_ domain.MissingStatsUpdater = (*Server)(nil)
)

// Scale warning thresholds for PostgreSQL major upgrades.
// Empirically measured on branchdb (PG 18.6 -> 19.0 pg_upgrade --swap):
//   - 20,415 relations took ~10.6s (~0.5ms/relation); warn at 10,000+ relations.
//   - 20,000 large objects took ~2.0s (~0.1ms/LO); warn at 50,000+ LOs.
//   - Large databases (10 GiB+) keep --swap fast, but post-upgrade ANALYZE scales with size.
const (
	ManyRelationsThreshold    int64 = 10_000
	ManyLargeObjectsThreshold int64 = 50_000
	LargeDatabaseThreshold    int64 = 10 << 30 // 10 GiB
)

type streamDirRunner interface {
	StreamDir(ctx context.Context, dir string, out io.Writer, name string, args ...string) error
}

// UpgradeTimeout computes the upgrade operation deadline from object counts.
// Base is 15m, adding 5s per 1,000 relations and 5s per 10,000 large objects,
// capped at 4h.
func UpgradeTimeout(relations, largeObjects int64) time.Duration {
	const (
		base = 15 * time.Minute
		capD = 4 * time.Hour
	)
	extra := time.Duration(max(relations, 0)/1000)*5*time.Second + time.Duration(max(largeObjects, 0)/10000)*5*time.Second
	return min(base+extra, capD)
}

func pgMajorStr(v string) string {
	major, _, _ := strings.Cut(v, ".")
	return major
}

func pgMajorNum(v string) int {
	n, _ := strconv.Atoi(pgMajorStr(v))
	return n
}

// CheckUpgrade inspects the running PostgreSQL cluster before an upgrade.
// Minor upgrades check PG_VERSION compatibility; major upgrades inspect
// extensions, replication state, object counts, locale/checksums, and run
// `pg_upgrade --check`.
func (p *Server) CheckUpgrade(ctx context.Context, s domain.Server, to domain.RuntimeRef, _ domain.Login) (domain.UpgradePlan, error) {
	b, err := os.ReadFile(filepath.Join(s.DataDir, "PG_VERSION"))
	if err != nil {
		return domain.UpgradePlan{}, fmt.Errorf("read PG_VERSION: %w", err)
	}
	have := strings.TrimSpace(string(b))
	wantFrom := pgMajorStr(s.Runtime.Version)
	wantTo := pgMajorStr(to.Version)
	if have != wantFrom {
		return domain.UpgradePlan{Findings: []domain.Finding{{
			Level:   domain.LevelError,
			Code:    domain.CodeUpgradePath,
			Message: fmt.Sprintf("The data directory is PostgreSQL %s, which does not match the branch's recorded runtime %s.", have, s.Runtime),
		}}}, nil
	}
	if have == wantTo {
		return domain.UpgradePlan{Findings: []domain.Finding{{
			Level:   domain.LevelInfo,
			Code:    domain.CodeUpgradePath,
			Message: fmt.Sprintf("Minor upgrade within PostgreSQL %s: the server restarts on the new binaries; the data files are unchanged.", have),
		}}}, nil
	}
	return p.checkMajorUpgrade(ctx, s, to)
}

func (p *Server) checkMajorUpgrade(ctx context.Context, s domain.Server, to domain.RuntimeRef) (domain.UpgradePlan, error) {
	var plan domain.UpgradePlan
	add := func(level domain.FindingLevel, code, msg string) {
		plan.Findings = append(plan.Findings, domain.Finding{Level: level, Code: code, Message: msg})
	}

	checksums, err := p.readChecksums(ctx, s)
	if err != nil {
		return plan, fmt.Errorf("read checksum setting: %w", err)
	}
	meta, err := p.readClusterMeta(ctx, s)
	if err != nil {
		return plan, fmt.Errorf("read cluster locale: %w", err)
	}
	plan.PGMeta = &meta

	totalBytes, relations, largeObjects, extFindings, replCount := p.inspectCluster(ctx, s, to)
	plan.Timeout = UpgradeTimeout(relations, largeObjects)
	plan.Findings = append(plan.Findings, extFindings...)

	if replCount > 0 {
		add(domain.LevelWarning, domain.CodeLogicalReplication,
			fmt.Sprintf("The cluster has %d logical replication slot(s) or subscription(s); verify slot/subscription state after upgrading.", replCount))
	}
	if relations >= ManyRelationsThreshold {
		add(domain.LevelWarning, domain.CodeManyRelations,
			fmt.Sprintf("The cluster has %d relations (tables, indexes, sequences); pg_upgrade schema dump/restore may take longer.", relations))
	}
	if largeObjects >= ManyLargeObjectsThreshold {
		add(domain.LevelWarning, domain.CodeManyLargeObjects,
			fmt.Sprintf("The cluster has %d large objects; pg_upgrade transfers them one by one and may take longer.", largeObjects))
	}
	if totalBytes >= LargeDatabaseThreshold {
		add(domain.LevelWarning, domain.CodeLargeDatabase,
			fmt.Sprintf("The cluster holds %.1f GiB of data; the branch becomes ready after pg_upgrade, and optimizer statistics update in the background.", float64(totalBytes)/(1<<30)))
	}

	checkFindings, err := p.runPGUpgradeCheck(ctx, s, to, checksums, meta)
	if err != nil {
		return plan, fmt.Errorf("pg_upgrade --check: %w", err)
	}
	plan.Findings = append(plan.Findings, checkFindings...)
	return plan, nil
}

func (p *Server) psqlQuery(ctx context.Context, s domain.Server, db, sql string) (string, error) {
	name, args := p.tool(s, "psql", "-h", p.sockDir(s), "-p", fmt.Sprint(s.Port), "-U", Superuser, "-d", db,
		"-X", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-c", sql)
	out, err := p.Run.Run(ctx, name, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (p *Server) readChecksums(ctx context.Context, s domain.Server) (bool, error) {
	name, args := p.tool(s, "pg_controldata", s.DataDir)
	out, err := p.Run.Run(ctx, name, args...)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "Data page checksum version" {
			n, _ := strconv.Atoi(strings.TrimSpace(v))
			return n != 0, nil
		}
	}
	return false, nil
}

func (p *Server) readClusterMeta(ctx context.Context, s domain.Server) (domain.PGClusterMeta, error) {
	meta := domain.PGClusterMeta{Encoding: "UTF8", LCCollate: "C.UTF-8", LCCtype: "C.UTF-8", LocaleProvider: "c"}
	out, err := p.psqlQuery(ctx, s, "template0", "SELECT pg_encoding_to_char(encoding), datcollate, datctype FROM pg_database WHERE datname = 'template0'")
	if err != nil {
		return meta, err
	}
	if fields := strings.Split(out, "|"); len(fields) == 3 {
		if fields[0] != "" {
			meta.Encoding = fields[0]
		}
		if fields[1] != "" {
			meta.LCCollate = fields[1]
		}
		if fields[2] != "" {
			meta.LCCtype = fields[2]
		}
	}
	if provOut, err := p.psqlQuery(ctx, s, "template0", "SELECT datlocprovider, COALESCE(datlocale, '') FROM pg_database WHERE datname = 'template0'"); err == nil {
		if fields := strings.Split(provOut, "|"); len(fields) == 2 {
			if fields[0] != "" {
				meta.LocaleProvider = fields[0]
			}
			meta.Locale = fields[1]
		}
	}
	return meta, nil
}

func (p *Server) inspectCluster(ctx context.Context, s domain.Server, to domain.RuntimeRef) (totalBytes, relations, largeObjects int64, extFindings []domain.Finding, replCount int64) {
	if out, err := p.psqlQuery(ctx, s, "postgres", "SELECT COALESCE(SUM(pg_database_size(oid)), 0) FROM pg_database"); err == nil {
		totalBytes, _ = strconv.ParseInt(out, 10, 64)
	}
	if out, err := p.psqlQuery(ctx, s, "postgres", "SELECT COUNT(*) FROM pg_replication_slots WHERE slot_type = 'logical'"); err == nil {
		n, _ := strconv.ParseInt(out, 10, 64)
		replCount += n
	}
	if out, err := p.psqlQuery(ctx, s, "postgres", "SELECT COUNT(*) FROM pg_subscription"); err == nil {
		n, _ := strconv.ParseInt(out, 10, 64)
		replCount += n
	}
	dbs, err := p.databases(ctx, s)
	if err != nil {
		return
	}
	extSeen := map[string]bool{}
	for _, db := range dbs {
		if out, err := p.psqlQuery(ctx, s, db, "SELECT COUNT(*) FROM pg_class"); err == nil {
			n, _ := strconv.ParseInt(out, 10, 64)
			relations += n
		}
		if out, err := p.psqlQuery(ctx, s, db, "SELECT COUNT(*) FROM pg_largeobject_metadata"); err == nil {
			n, _ := strconv.ParseInt(out, 10, 64)
			largeObjects += n
		}
		out, err := p.psqlQuery(ctx, s, db, "SELECT extname, extversion FROM pg_extension WHERE extname <> 'plpgsql' ORDER BY extname")
		if err != nil || out == "" {
			continue
		}
		for _, line := range strings.Split(out, "\n") {
			name, ver, ok := strings.Cut(line, "|")
			if !ok || name == "" {
				continue
			}
			key := db + ":" + name
			if extSeen[key] {
				continue
			}
			extSeen[key] = true
			if f, ok := p.checkExtensionInTarget(to, db, name, ver); ok {
				extFindings = append(extFindings, f)
			}
		}
	}
	return
}

func (p *Server) checkExtensionInTarget(to domain.RuntimeRef, db, extName, oldVer string) (domain.Finding, bool) {
	targetRoot := p.RuntimeDir(to)
	major := pgMajorStr(to.Version)
	candidates := []string{
		filepath.Join(targetRoot, "usr", "share", "postgresql", major, "extension", extName+".control"),
		filepath.Join(targetRoot, "share", "extension", extName+".control"),
	}
	if matches, _ := filepath.Glob(filepath.Join(targetRoot, "usr", "share", "postgresql", "*", "extension", extName+".control")); len(matches) > 0 {
		candidates = append([]string{matches[0]}, candidates...)
	}
	var ctrlPath string
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			ctrlPath = c
			break
		}
	}
	if ctrlPath == "" {
		hint := ""
		if extName == "anon" {
			hint = " (install postgresql_anonymizer for PostgreSQL " + major + " or drop the anon extension)"
		}
		return domain.Finding{
			Level:   domain.LevelError,
			Code:    domain.CodeExtensionMissing,
			Message: fmt.Sprintf("Extension %q (installed in database %q) is not present in %s%s.", extName, db, to, hint),
		}, true
	}
	raw, err := os.ReadFile(ctrlPath)
	if err != nil {
		return domain.Finding{}, false
	}
	if defVer := parseControlDefaultVersion(string(raw)); defVer != "" && defVer != oldVer {
		return domain.Finding{
			Level:   domain.LevelInfo,
			Code:    domain.CodeExtensionUpdate,
			Message: fmt.Sprintf("Extension %q in database %q is at version %s; %s provides %s. Run ALTER EXTENSION %q UPDATE after upgrading.", extName, db, oldVer, to, defVer, extName),
		}, true
	}
	return domain.Finding{}, false
}

func parseControlDefaultVersion(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != "default_version" {
			continue
		}
		v = strings.TrimSpace(v)
		v = strings.Trim(v, "'\"")
		return v
	}
	return ""
}

func initdbArgs(dataDir string, checksums bool, meta domain.PGClusterMeta) []string {
	args := []string{
		"-D", dataDir,
		"-U", Superuser,
		"--auth-local=trust",
		"--auth-host=scram-sha-256",
	}
	enc := meta.Encoding
	if enc == "" {
		enc = "UTF8"
	}
	args = append(args, "-E", enc)
	switch {
	case meta.LocaleProvider == "b" && meta.Locale != "":
		args = append(args, "--locale-provider=builtin", "--builtin-locale="+meta.Locale)
	case meta.LocaleProvider == "i" && meta.Locale != "":
		args = append(args, "--locale-provider=icu", "--icu-locale="+meta.Locale)
	case meta.LCCollate != "" || meta.LCCtype != "":
		if meta.LCCollate != "" {
			args = append(args, "--lc-collate="+meta.LCCollate)
		}
		if meta.LCCtype != "" {
			args = append(args, "--lc-ctype="+meta.LCCtype)
		}
	default:
		args = append(args, "--locale=C.UTF-8")
	}
	if checksums {
		args = append(args, "--data-checksums")
	} else {
		args = append(args, "--no-data-checksums")
	}
	return args
}

func (p *Server) runAsUser(ctx context.Context, workDir string, onLine func(string), envLib string, bin string, args ...string) ([]byte, error) {
	cmdName := "env"
	cmdArgs := append([]string{"LD_LIBRARY_PATH=" + envLib, bin}, args...)
	if p.User != "" {
		cmdArgs = append([]string{"-u", p.User, "--", cmdName}, cmdArgs...)
		cmdName = "runuser"
	}
	if sr, ok := p.Run.(streamDirRunner); ok {
		var buf bytes.Buffer
		lw := &lineWriter{buf: &buf, onLine: onLine}
		err := sr.StreamDir(ctx, workDir, lw, cmdName, cmdArgs...)
		lw.flush()
		return buf.Bytes(), err
	}
	out, err := p.Run.Run(ctx, cmdName, cmdArgs...)
	if onLine != nil {
		for _, line := range strings.Split(string(out), "\n") {
			if t := strings.TrimSpace(line); t != "" {
				onLine(t)
			}
		}
	}
	return out, err
}

type lineWriter struct {
	mu      sync.Mutex
	buf     *bytes.Buffer
	pending string
	onLine  func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if w.onLine == nil {
		return len(p), nil
	}
	w.pending += string(p)
	for {
		idx := strings.IndexByte(w.pending, '\n')
		if idx < 0 {
			break
		}
		line := strings.TrimSpace(w.pending[:idx])
		w.pending = w.pending[idx+1:]
		if line != "" {
			w.onLine(line)
		}
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.onLine != nil && strings.TrimSpace(w.pending) != "" {
		w.onLine(strings.TrimSpace(w.pending))
	}
	w.pending = ""
}

func (p *Server) libPath(oldRT, newRT domain.RuntimeRef) string {
	newLib := filepath.Join(p.RuntimeDir(newRT), "lib")
	oldLib := filepath.Join(p.RuntimeDir(oldRT), "lib")
	if newLib == oldLib || oldLib == "" {
		return newLib
	}
	return newLib + ":" + oldLib
}

func freeTCPPort(fallback int) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fallback
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func (p *Server) runPGUpgradeCheck(ctx context.Context, s domain.Server, to domain.RuntimeRef, checksums bool, meta domain.PGClusterMeta) ([]domain.Finding, error) {
	tmpDir, err := os.MkdirTemp("", "db-branch-pgcheck-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)
	newDir := filepath.Join(tmpDir, "new")
	if err := os.MkdirAll(newDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(tmpDir, 0o700); err != nil {
		return nil, err
	}
	if err := p.chown(tmpDir, newDir); err != nil {
		return nil, err
	}

	newBin := filepath.Join(p.RuntimeDir(to), "bin")
	oldBin := filepath.Join(p.RuntimeDir(s.Runtime), "bin")
	libPath := p.libPath(s.Runtime, to)

	iArgs := initdbArgs(newDir, checksums, meta)
	if _, err := p.runAsUser(ctx, tmpDir, nil, libPath, filepath.Join(newBin, "initdb"), iArgs...); err != nil {
		return nil, fmt.Errorf("initdb for check: %w", err)
	}

	newPort := freeTCPPort(s.Port + 1000)
	pgArgs := []string{
		"--check",
		"-U", Superuser,
		"-b", oldBin,
		"-B", newBin,
		"-d", s.DataDir,
		"-D", newDir,
		"-s", p.sockDir(s),
		"-p", fmt.Sprint(s.Port),
		"-P", fmt.Sprint(newPort),
		"-O", "-c listen_addresses=",
	}
	out, checkErr := p.runAsUser(ctx, tmpDir, nil, libPath, filepath.Join(newBin, "pg_upgrade"), pgArgs...)
	text := string(out)
	if checkErr != nil {
		detail := readPGUpgradeOutputFiles(newDir)
		if !strings.Contains(text, "failed") && !strings.Contains(text, "error") {
			text = strings.TrimSpace(text + "\n" + checkErr.Error())
		}
		if detail != "" {
			text = strings.TrimSpace(text + "\n" + detail)
		}
		fs := ParsePGUpgradeOutput(text, true)
		if !domain.HasErrors(fs) {
			fs = append(fs, domain.Finding{Level: domain.LevelError, Code: domain.CodePGCheckError, Message: strings.TrimSpace(text)})
		}
		return fs, nil
	}
	return ParsePGUpgradeOutput(text, true), nil
}

func readPGUpgradeOutputFiles(newDir string) string {
	pattern := filepath.Join(newDir, "pg_upgrade_output.d", "*", "*.txt")
	files, _ := filepath.Glob(pattern)
	var parts []string
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(raw))
		if s == "" {
			continue
		}
		if len(s) > 1024 {
			s = s[:1024] + "..."
		}
		parts = append(parts, filepath.Base(f)+": "+s)
	}
	return strings.Join(parts, "\n")
}

// ParsePGUpgradeOutput converts pg_upgrade --check (or upgrade) stdout/stderr
// into structured findings.
func ParsePGUpgradeOutput(out string, isCheck bool) []domain.Finding {
	var fs []domain.Finding
	var errBuf []string
	inErrorBlock := false
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "WARNING:") {
			fs = append(fs, domain.Finding{
				Level:   domain.LevelWarning,
				Code:    domain.CodePGCheckWarning,
				Message: strings.TrimSpace(strings.TrimPrefix(line, "WARNING:")),
			})
			continue
		}
		if strings.Contains(line, "failed check:") || strings.HasPrefix(line, "fatal") || strings.Contains(line, "Failure, exiting") {
			inErrorBlock = true
		}
		if inErrorBlock {
			if !strings.HasPrefix(line, "Failure, exiting") {
				errBuf = append(errBuf, line)
			}
			continue
		}
		if !isCheck && (strings.HasSuffix(line, " ok") || line == "Upgrade Complete") {
			fs = append(fs, domain.Finding{
				Level:   domain.LevelInfo,
				Code:    domain.CodeLogProgress,
				Message: line,
			})
		}
	}
	if len(errBuf) > 0 {
		fs = append(fs, domain.Finding{
			Level:   domain.LevelError,
			Code:    domain.CodePGCheckError,
			Message: strings.Join(errBuf, " "),
		})
	} else if isCheck && strings.Contains(out, "*Clusters are compatible*") {
		fs = append(fs, domain.Finding{
			Level:   domain.LevelInfo,
			Code:    domain.CodeUpgradePath,
			Message: "pg_upgrade --check passed: clusters are compatible.",
		})
	}
	return fs
}

// StartUpgraded starts the server on the new binaries (minor upgrade).
func (p *Server) StartUpgraded(ctx context.Context, s domain.Server, _ domain.Login) (domain.UpgradeLog, error) {
	err := p.Start(ctx, s)
	return domain.UpgradeLog{Excerpt: p.journalFresh(ctx, s)}, err
}

// StartMajorUpgraded runs pg_upgrade (for major version changes) and starts the
// server on the new runtime, or delegates to StartUpgraded for minor updates.
func (p *Server) StartMajorUpgraded(ctx context.Context, s domain.Server, login domain.Login, up domain.UpgradeContext) (domain.UpgradeLog, error) {
	if pgMajorStr(up.From.Version) == pgMajorStr(s.Runtime.Version) {
		return p.StartUpgraded(ctx, s, login)
	}
	if p.CheckOwner != nil {
		if err := p.CheckOwner(s.DataDir); err != nil {
			return domain.UpgradeLog{}, err
		}
	}
	b, err := os.ReadFile(filepath.Join(s.DataDir, "PG_VERSION"))
	if err != nil {
		return domain.UpgradeLog{}, fmt.Errorf("read PG_VERSION: %w", err)
	}
	if have := strings.TrimSpace(string(b)); have != pgMajorStr(up.From.Version) {
		return domain.UpgradeLog{}, fmt.Errorf("data directory is PostgreSQL %s, expected %s", have, up.From)
	}
	checksums, err := p.readChecksums(ctx, domain.Server{ID: s.ID, Runtime: up.From, DataDir: s.DataDir})
	if err != nil {
		return domain.UpgradeLog{}, fmt.Errorf("read checksum setting: %w", err)
	}
	meta := domain.PGClusterMeta{Encoding: "UTF8", LCCollate: "C.UTF-8", LCCtype: "C.UTF-8", LocaleProvider: "c"}
	if up.Plan.PGMeta != nil {
		meta = *up.Plan.PGMeta
	}

	report := func(phase string) {
		if up.Report != nil {
			up.Report(phase)
		}
	}

	oldDir := filepath.Join(s.DataDir, ".pg_upgrade_old")
	newDir := filepath.Join(s.DataDir, ".pg_upgrade_new")
	sockDir := filepath.Join(s.DataDir, ".pg_upgrade_sock")
	if err := p.stageUpgradeDirs(s.DataDir, oldDir, newDir, sockDir); err != nil {
		return domain.UpgradeLog{}, err
	}

	oldBin := filepath.Join(p.RuntimeDir(up.From), "bin")
	newBin := filepath.Join(p.RuntimeDir(s.Runtime), "bin")
	libPath := p.libPath(up.From, s.Runtime)

	report("pg_upgrade: initdb")
	iArgs := initdbArgs(newDir, checksums, meta)
	if out, err := p.runAsUser(ctx, sockDir, nil, libPath, filepath.Join(newBin, "initdb"), iArgs...); err != nil {
		return domain.UpgradeLog{Excerpt: lastBytes(string(out)+"\n"+err.Error(), 4096)}, fmt.Errorf("initdb for new cluster: %w", err)
	}

	p1 := freeTCPPort(50432)
	p2 := freeTCPPort(50433)
	if p2 == p1 {
		p2 = p1 + 1
	}
	pgArgs := pgUpgradeArgs(s.Runtime, oldBin, newBin, oldDir, newDir, sockDir, p1, p2)

	report("pg_upgrade: checking")
	lastPhase := "pg_upgrade: checking"
	onLine := func(line string) {
		if ph := pgUpgradeStage(line); ph != "" && ph != lastPhase {
			lastPhase = ph
			report(ph)
		}
	}
	out, upErr := p.runAsUser(ctx, sockDir, onLine, libPath, filepath.Join(newBin, "pg_upgrade"), pgArgs...)
	text := string(out)
	if upErr != nil {
		detail := readPGUpgradeOutputFiles(newDir)
		excerpt := strings.TrimSpace(text + "\n" + detail + "\n" + upErr.Error())
		return domain.UpgradeLog{Findings: ParsePGUpgradeOutput(text, false), Excerpt: lastBytes(excerpt, 4096)}, fmt.Errorf("pg_upgrade: %w", upErr)
	}

	findings := ParsePGUpgradeOutput(text, false)
	if fileExists(filepath.Join(sockDir, "update_extensions.sql")) {
		findings = append(findings, domain.Finding{
			Level:   domain.LevelInfo,
			Code:    domain.CodeExtensionUpdate,
			Message: "Some extensions can be updated with ALTER EXTENSION ... UPDATE.",
		})
	}
	if err := p.promoteUpgradedCluster(s.DataDir, oldDir, newDir, sockDir); err != nil {
		return domain.UpgradeLog{Findings: findings}, err
	}

	report("starting")
	err = p.Start(ctx, s)
	return domain.UpgradeLog{
		Findings:   findings,
		Excerpt:    p.journalFresh(ctx, s),
		NeedsStats: err == nil,
	}, err
}

func (p *Server) stageUpgradeDirs(dataDir, oldDir, newDir, sockDir string) error {
	for _, d := range []string{oldDir, newDir, sockDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	if err := p.chown(oldDir, newDir, sockDir); err != nil {
		return err
	}
	if err := moveClusterInto(dataDir, oldDir); err != nil {
		return fmt.Errorf("stage old data directory: %w", err)
	}
	return nil
}

func pgUpgradeArgs(to domain.RuntimeRef, oldBin, newBin, oldDir, newDir, sockDir string, p1, p2 int) []string {
	var args []string
	if pgMajorNum(to.Version) >= 18 {
		args = append(args, "--swap", "--sync-method=fsync")
	} else {
		args = append(args, "--link")
	}
	return append(args,
		"-U", Superuser,
		"-b", oldBin, "-B", newBin,
		"-d", oldDir, "-D", newDir,
		"-s", sockDir,
		"-p", fmt.Sprint(p1), "-P", fmt.Sprint(p2),
		"-o", "-c listen_addresses=", "-O", "-c listen_addresses=",
	)
}

func (p *Server) promoteUpgradedCluster(dataDir, oldDir, newDir, sockDir string) error {
	if err := ensureHBARemote(filepath.Join(newDir, "pg_hba.conf")); err != nil {
		return fmt.Errorf("update pg_hba.conf: %w", err)
	}
	_ = os.RemoveAll(filepath.Join(newDir, "pg_upgrade_output.d"))
	_ = os.RemoveAll(oldDir)
	_ = os.RemoveAll(sockDir)
	if err := moveClusterOut(newDir, dataDir); err != nil {
		return fmt.Errorf("promote upgraded data directory: %w", err)
	}
	_ = os.RemoveAll(newDir)
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return err
	}
	return p.chown(dataDir)
}

func pgUpgradeStage(line string) string {
	switch {
	case strings.Contains(line, "Creating dump"):
		return "pg_upgrade: dumping"
	case strings.Contains(line, "Restoring "):
		return "pg_upgrade: restoring"
	case strings.Contains(line, "Swapping data directories"), strings.Contains(line, "Linking user relation"):
		return "pg_upgrade: swapping"
	case strings.Contains(line, "Sync data directory"):
		return "pg_upgrade: syncing"
	default:
		return ""
	}
}

func moveClusterInto(dataDir, dstDir string) error {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		n := e.Name()
		if n == ".zfs" || strings.HasPrefix(n, ".pg_upgrade_") {
			continue
		}
		if err := os.Rename(filepath.Join(dataDir, n), filepath.Join(dstDir, n)); err != nil {
			return err
		}
	}
	return nil
}

func moveClusterOut(srcDir, dataDir string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		n := e.Name()
		if err := os.Rename(filepath.Join(srcDir, n), filepath.Join(dataDir, n)); err != nil {
			return err
		}
	}
	return nil
}

func ensureHBARemote(hbaPath string) error {
	raw, err := os.ReadFile(hbaPath)
	if err != nil {
		return err
	}
	if strings.Contains(string(raw), "0.0.0.0/0 scram-sha-256") {
		return nil
	}
	f, err := os.OpenFile(hbaPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(hbaRemote)
	return err
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func lastBytes(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// UpdateMissingStats populates optimizer statistics in stages after a major
// upgrade without overwriting preserved statistics on PostgreSQL 18+.
func (p *Server) UpdateMissingStats(ctx context.Context, s domain.Server) error {
	args := []string{"-h", p.sockDir(s), "-p", fmt.Sprint(s.Port), "-U", Superuser, "--all", "--analyze-in-stages"}
	if pgMajorNum(s.Runtime.Version) >= 18 {
		args = append(args, "--missing-stats-only")
	}
	args = append(args, "--quiet")
	name, cmdArgs := p.tool(s, "vacuumdb", args...)
	_, err := p.Run.Run(ctx, name, cmdArgs...)
	return err
}
