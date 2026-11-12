package zfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/execx"
)

var _ domain.CheckpointStorage = (*Driver)(nil)

// userPropPattern also allows hyphens, which user properties (db-branch:line) use.
var userPropPattern = regexp.MustCompile(`^[a-z][a-z0-9_:.-]*$`)

// snapshotProps are what SnapshotInfos asks zfs for.
var snapshotProps = "createtxg,creation,used,clones," + domain.PropLine + "," + domain.PropAuto + "," + domain.PropFrom + "," + domain.PropParent

// checkTarget accepts a dataset or a snapshot (dataset@name).
func checkTarget(target string) error {
	ds, snap, isSnap := strings.Cut(target, "@")
	if err := checkDataset(ds); err != nil {
		return err
	}
	if isSnap && !component.MatchString(snap) {
		return fmt.Errorf("%w: snapshot name %q", ErrInvalidDataset, snap)
	}
	return nil
}

func checkProps(props map[string]string) ([]string, error) {
	keys := make([]string, 0, len(props))
	for k, v := range props {
		if !userPropPattern.MatchString(k) || !valuePattern.MatchString(v) {
			return nil, fmt.Errorf("%w: property %q=%q", ErrInvalidDataset, k, v)
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%w: no properties", ErrInvalidDataset)
	}
	sort.Strings(keys)
	return keys, nil
}

// Datasets lists the filesystems directly under parent by full name.
func (d *Driver) Datasets(ctx context.Context, parent string) ([]string, error) {
	if err := checkDataset(parent); err != nil {
		return nil, err
	}
	out, err := d.zfs(ctx, "list", "-H", "-d", "1", "-t", "filesystem", "-o", "name", parent)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" && line != parent {
			names = append(names, line)
		}
	}
	return names, nil
}

// SnapshotInfos lists the snapshots directly on dataset, oldest first, with
// the user properties set on each snapshot itself.
func (d *Driver) SnapshotInfos(ctx context.Context, dataset string) ([]domain.SnapshotInfo, error) {
	if err := checkDataset(dataset); err != nil {
		return nil, err
	}
	out, err := d.zfs(ctx, "get", "-Hp", "-d", "1", "-t", "snapshot", "-o", "name,property,value,source", snapshotProps, dataset)
	if err != nil {
		return nil, err
	}
	byName := map[string]*domain.SnapshotInfo{}
	txg := map[string]uint64{}
	var order []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		_, snap, ok := strings.Cut(f[0], "@")
		if len(f) != 4 || !ok {
			return nil, fmt.Errorf("zfs get %s: unexpected line %q", dataset, line)
		}
		s := byName[snap]
		if s == nil {
			s = &domain.SnapshotInfo{Name: snap}
			byName[snap] = s
			order = append(order, snap)
		}
		if err := fillSnapshot(s, txg, snap, f[1], f[2], f[3]); err != nil {
			return nil, fmt.Errorf("zfs get %s: %w", dataset, err)
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return txg[order[i]] < txg[order[j]] })
	list := make([]domain.SnapshotInfo, 0, len(order))
	for _, n := range order {
		list = append(list, *byName[n])
	}
	return list, nil
}

func fillSnapshot(s *domain.SnapshotInfo, txg map[string]uint64, snap, prop, value, source string) error {
	var err error
	switch prop {
	case "createtxg":
		txg[snap], err = strconv.ParseUint(value, 10, 64)
	case "creation":
		var secs int64
		secs, err = strconv.ParseInt(value, 10, 64)
		s.Created = time.Unix(secs, 0).UTC()
	case "used":
		s.Used, err = strconv.ParseUint(value, 10, 64)
	case "clones":
		if value != "-" {
			s.Clones = strings.Split(value, ",")
		}
	default:
		if source == "local" {
			if s.Props == nil {
				s.Props = map[string]string{}
			}
			s.Props[prop] = value
		}
	}
	if err != nil {
		return fmt.Errorf("%s of %s: %w", prop, snap, err)
	}
	return nil
}

// Props returns the user properties set on dataset itself.
func (d *Driver) Props(ctx context.Context, dataset string) (map[string]string, error) {
	if err := checkDataset(dataset); err != nil {
		return nil, err
	}
	out, err := d.zfs(ctx, "get", "-H", "-s", "local", "-o", "property,value", "all", dataset)
	if err != nil {
		return nil, err
	}
	props := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if k, v, ok := strings.Cut(line, "\t"); ok && strings.Contains(k, ":") {
			props[k] = v
		}
	}
	return props, nil
}

// SetProps sets properties on a dataset or snapshot in one zfs call.
func (d *Driver) SetProps(ctx context.Context, target string, props map[string]string) error {
	if err := checkTarget(target); err != nil {
		return err
	}
	keys, err := checkProps(props)
	if err != nil {
		return err
	}
	args := []string{"set"}
	for _, k := range keys {
		args = append(args, k+"="+props[k])
	}
	_, err = d.zfs(ctx, append(args, target)...)
	return err
}

// Unset removes user properties from dataset.
func (d *Driver) Unset(ctx context.Context, dataset string, keys ...string) error {
	if err := checkDataset(dataset); err != nil {
		return err
	}
	for _, k := range keys {
		if !userPropPattern.MatchString(k) {
			return fmt.Errorf("%w: property %q", ErrInvalidDataset, k)
		}
		if _, err := d.zfs(ctx, "inherit", k, dataset); err != nil {
			return err
		}
	}
	return nil
}

// CloneWith clones origin into dataset (whose parent must exist) with props.
func (d *Driver) CloneWith(ctx context.Context, origin domain.Snapshot, dataset string, props map[string]string) error {
	if err := checkSnapshot(origin); err != nil {
		return err
	}
	if err := checkDataset(dataset); err != nil {
		return err
	}
	args := []string{"clone"}
	if len(props) > 0 {
		keys, err := checkProps(props)
		if err != nil {
			return err
		}
		for _, k := range keys {
			args = append(args, "-o", k+"="+props[k])
		}
	}
	_, err := d.zfs(ctx, append(args, origin.String(), dataset)...)
	return err
}

func checkMovable(ds string) error {
	if err := checkDataset(ds); err != nil {
		return err
	}
	if !strings.Contains(ds, "/") {
		return fmt.Errorf("%w: %q is a pool root", ErrInvalidDataset, ds)
	}
	return nil
}

// Rename moves a dataset (and its snapshots) to another name.
func (d *Driver) Rename(ctx context.Context, from, to string) error {
	if err := checkMovable(from); err != nil {
		return err
	}
	if err := checkMovable(to); err != nil {
		return err
	}
	_, err := d.zfs(ctx, "rename", "-p", from, to)
	return err
}

// RenameSnapshot gives a snapshot another name.
func (d *Driver) RenameSnapshot(ctx context.Context, s domain.Snapshot, name string) error {
	if err := checkSnapshot(s); err != nil {
		return err
	}
	to := domain.Snapshot{Dataset: s.Dataset, Name: name}
	if err := checkSnapshot(to); err != nil {
		return err
	}
	_, err := d.zfs(ctx, "rename", s.String(), to.String())
	return err
}

// Promote makes a clone the owner of the snapshots it was cloned from.
func (d *Driver) Promote(ctx context.Context, dataset string) error {
	if err := checkMovable(dataset); err != nil {
		return err
	}
	_, err := d.zfs(ctx, "promote", dataset)
	return err
}

// Origin returns the snapshot dataset was cloned from, or "".
func (d *Driver) Origin(ctx context.Context, dataset string) (string, error) {
	if err := checkDataset(dataset); err != nil {
		return "", err
	}
	out, err := d.zfs(ctx, "get", "-H", "-o", "value", "origin", dataset)
	if err != nil {
		return "", err
	}
	if v := strings.TrimSpace(string(out)); v != "-" {
		return v, nil
	}
	return "", nil
}

// Mount mounts dataset; one that is mounted already is fine.
func (d *Driver) Mount(ctx context.Context, dataset string) error {
	if err := checkDataset(dataset); err != nil {
		return err
	}
	_, err := d.zfs(ctx, "mount", dataset)
	var ee *execx.Error
	if errors.As(err, &ee) && strings.Contains(ee.Stderr, "already mounted") {
		return nil
	}
	return err
}

// linesDir is where read-only mounts of lines and checkpoints are made.
const linesDir = "/run"

// MountReadOnly mounts target (a dataset, or dataset@snapshot) read-only in
// a new directory and returns it. A dataset that a restore set aside is not
// mounted by zfs (canmount=off); the kernel mounts it all the same.
func (d *Driver) MountReadOnly(ctx context.Context, target string) (string, error) {
	if err := checkTarget(target); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(orDefault(d.RunDir, linesDir), "db-branch-line-")
	if err != nil {
		return "", err
	}
	opts := "ro,zfsutil"
	if strings.Contains(target, "@") {
		opts = "ro"
	}
	if _, err := d.run.Run(ctx, "mount", "-t", "zfs", "-o", opts, target, dir); err != nil {
		_ = os.Remove(dir)
		return "", fmt.Errorf("mount %s: %w", target, err)
	}
	return dir, nil
}

// Unmount undoes MountReadOnly. It touches nothing else: dir must be one
// MountReadOnly made.
func (d *Driver) Unmount(ctx context.Context, dir string) error {
	if filepath.Dir(dir) != filepath.Clean(orDefault(d.RunDir, linesDir)) || !strings.HasPrefix(filepath.Base(dir), "db-branch-line-") {
		return fmt.Errorf("%w: %q is not a read-only mount of ours", ErrInvalidDataset, dir)
	}
	if _, err := d.run.Run(ctx, "umount", dir); err != nil {
		return fmt.Errorf("umount %s: %w", dir, err)
	}
	return os.Remove(dir)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
