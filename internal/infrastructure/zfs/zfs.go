// Package zfs implements domain.Storage with the zfs command.
package zfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/execx"
)

// ErrInvalidDataset is returned for names that zfs could misread as options,
// snapshots or nested paths, and for operations that would hit a whole pool.
var ErrInvalidDataset = errors.New("invalid zfs dataset")

// Runner runs an external command and returns its standard output, or
// streams it for zfs send / recv.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	Stream(ctx context.Context, in io.Reader, out io.Writer, name string, args ...string) error
}

// Driver talks to ZFS through the zfs command.
type Driver struct{ run Runner }

var (
	_ domain.Storage         = (*Driver)(nil)
	_ domain.SnapshotStreams = (*Driver)(nil)
)

// New returns a Driver that runs zfs through r.
func New(r Runner) *Driver { return &Driver{run: r} }

// A path component may contain what zfs allows except a leading hyphen.
var component = regexp.MustCompile(`^[A-Za-z0-9_.:][A-Za-z0-9_.:-]*$`)

func checkDataset(ds string) error {
	for _, c := range strings.Split(ds, "/") {
		if !component.MatchString(c) {
			return fmt.Errorf("%w: %q", ErrInvalidDataset, ds)
		}
	}
	return nil
}

func checkSnapshot(s domain.Snapshot) error {
	if err := checkDataset(s.Dataset); err != nil {
		return err
	}
	if !component.MatchString(s.Name) {
		return fmt.Errorf("%w: snapshot name %q", ErrInvalidDataset, s.Name)
	}
	return nil
}

func (d *Driver) zfs(ctx context.Context, args ...string) ([]byte, error) {
	out, err := d.run.Run(ctx, "zfs", args...)
	if err != nil {
		return nil, fmt.Errorf("zfs %s: %w", args[0], err)
	}
	return out, nil
}

var propPattern = regexp.MustCompile(`^[a-z][a-z0-9_:.]*$`)
var valuePattern = regexp.MustCompile(`^[A-Za-z0-9_.:/=-]+$`)

// Create makes dataset (and missing parents) with the given properties.
func (d *Driver) Create(ctx context.Context, dataset string, props map[string]string) error {
	if err := checkDataset(dataset); err != nil {
		return err
	}
	keys := make([]string, 0, len(props))
	for k, v := range props {
		if !propPattern.MatchString(k) || !valuePattern.MatchString(v) {
			return fmt.Errorf("%w: property %q=%q", ErrInvalidDataset, k, v)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	args := []string{"create", "-p"}
	for _, k := range keys {
		args = append(args, "-o", k+"="+props[k])
	}
	_, err := d.zfs(ctx, append(args, dataset)...)
	return err
}

// Snapshot creates s.
func (d *Driver) Snapshot(ctx context.Context, s domain.Snapshot) error {
	if err := checkSnapshot(s); err != nil {
		return err
	}
	_, err := d.zfs(ctx, "snapshot", s.String())
	return err
}

// Clone creates dataset from origin, creating missing parents.
func (d *Driver) Clone(ctx context.Context, origin domain.Snapshot, dataset string) error {
	if err := checkSnapshot(origin); err != nil {
		return err
	}
	if err := checkDataset(dataset); err != nil {
		return err
	}
	_, err := d.zfs(ctx, "clone", "-p", origin.String(), dataset)
	return err
}

// Rollback returns s.Dataset to the state captured by s.
func (d *Driver) Rollback(ctx context.Context, s domain.Snapshot) error {
	if err := checkSnapshot(s); err != nil {
		return err
	}
	_, err := d.zfs(ctx, "rollback", s.String())
	return err
}

// Destroy removes dataset and its snapshots. It refuses to destroy a pool's root dataset.
func (d *Driver) Destroy(ctx context.Context, dataset string) error {
	if err := checkDataset(dataset); err != nil {
		return err
	}
	if !strings.Contains(dataset, "/") {
		return fmt.Errorf("%w: refusing to destroy pool root %q", ErrInvalidDataset, dataset)
	}
	_, err := d.zfs(ctx, "destroy", "-r", dataset)
	return err
}

// DestroySnapshot removes one snapshot (never its clones: zfs refuses while
// a clone depends on it).
func (d *Driver) DestroySnapshot(ctx context.Context, s domain.Snapshot) error {
	if err := checkSnapshot(s); err != nil {
		return err
	}
	_, err := d.zfs(ctx, "destroy", s.String())
	return err
}

// Exists reports whether dataset exists.
func (d *Driver) Exists(ctx context.Context, dataset string) (bool, error) {
	if err := checkDataset(dataset); err != nil {
		return false, err
	}
	_, err := d.zfs(ctx, "list", "-H", "-o", "name", dataset)
	var ee *execx.Error
	if errors.As(err, &ee) && strings.Contains(ee.Stderr, "dataset does not exist") {
		return false, nil
	}
	return err == nil, err
}

// Info returns the mount point and space usage of dataset.
func (d *Driver) Info(ctx context.Context, dataset string) (domain.DatasetInfo, error) {
	if err := checkDataset(dataset); err != nil {
		return domain.DatasetInfo{}, err
	}
	out, err := d.zfs(ctx, "get", "-Hp", "-o", "value", "mountpoint,used,referenced", dataset)
	if err != nil {
		return domain.DatasetInfo{}, err
	}
	fields := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(fields) != 3 {
		return domain.DatasetInfo{}, fmt.Errorf("zfs get %s: unexpected output %q", dataset, out)
	}
	used, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return domain.DatasetInfo{}, fmt.Errorf("zfs get %s: used: %w", dataset, err)
	}
	referenced, err := strconv.ParseUint(fields[2], 10, 64)
	if err != nil {
		return domain.DatasetInfo{}, fmt.Errorf("zfs get %s: referenced: %w", dataset, err)
	}
	return domain.DatasetInfo{Mountpoint: fields[0], Used: used, Referenced: referenced}, nil
}

// Usage returns the space used by each direct child of parent, by its last
// path component.
func (d *Driver) Usage(ctx context.Context, parent string) (map[string]domain.Usage, error) {
	if err := checkDataset(parent); err != nil {
		return nil, err
	}
	out, err := d.zfs(ctx, "list", "-Hp", "-d", "1", "-o", "name,used,referenced", parent)
	if err != nil {
		return nil, err
	}
	usage := map[string]domain.Usage{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 3 || f[0] == parent || !strings.HasPrefix(f[0], parent+"/") {
			continue
		}
		used, err1 := strconv.ParseUint(f[1], 10, 64)
		ref, err2 := strconv.ParseUint(f[2], 10, 64)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("zfs list %s: unexpected line %q", parent, line)
		}
		usage[strings.TrimPrefix(f[0], parent+"/")] = domain.Usage{Used: used, Referenced: ref}
	}
	return usage, nil
}

// Send writes a replication stream of dataset@to: with from, an incremental
// stream of every snapshot after from (-I); without, the whole dataset with
// all its snapshots up to to (-R).
func (d *Driver) Send(ctx context.Context, dataset, from, to string, w io.Writer) error {
	target := domain.Snapshot{Dataset: dataset, Name: to}
	if err := checkSnapshot(target); err != nil {
		return err
	}
	args := []string{"send", "-R", target.String()}
	if from != "" {
		if err := checkSnapshot(domain.Snapshot{Dataset: dataset, Name: from}); err != nil {
			return err
		}
		args = []string{"send", "-I", "@" + from, target.String()}
	}
	if err := d.run.Stream(ctx, nil, w, "zfs", args...); err != nil {
		return fmt.Errorf("zfs send: %w", err)
	}
	return nil
}

// Receive creates or advances dataset from a Send stream. The copy is not
// mounted, and is rolled back to its latest snapshot first if anything
// touched it (-F).
func (d *Driver) Receive(ctx context.Context, dataset string, r io.Reader) error {
	if err := checkDataset(dataset); err != nil {
		return err
	}
	if err := d.run.Stream(ctx, r, io.Discard, "zfs", "recv", "-u", "-F", dataset); err != nil {
		return fmt.Errorf("zfs recv: %w", err)
	}
	return nil
}

// Children lists the direct children of parent by their last path component.
func (d *Driver) Children(ctx context.Context, parent string) ([]string, error) {
	usage, err := d.Usage(ctx, parent)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(usage))
	for name := range usage {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// Snapshots lists the snapshot names of dataset, oldest first.
func (d *Driver) Snapshots(ctx context.Context, dataset string) ([]string, error) {
	if err := checkDataset(dataset); err != nil {
		return nil, err
	}
	out, err := d.zfs(ctx, "list", "-H", "-t", "snapshot", "-s", "createtxg", "-o", "name", "-d", "1", dataset)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if _, snap, ok := strings.Cut(line, "@"); ok {
			names = append(names, snap)
		}
	}
	return names, nil
}
