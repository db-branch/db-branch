package dbbranch_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

// errCrash is what every operation returns once the simulated agent has died.
var errCrash = errors.New("simulated crash")

// crasher kills the simulated agent before its Nth side effect (storage,
// server or state change): from then on every side effect fails, as it would
// in a process that is gone.
type crasher struct {
	at, steps int
	dead      bool
}

func (c *crasher) step() error {
	if c.dead {
		return errCrash
	}
	c.steps++
	if c.at > 0 && c.steps == c.at {
		c.dead = true
		return errCrash
	}
	return nil
}

// revive is the agent starting again: it counts steps from zero and no longer dies.
func (c *crasher) revive() { c.dead, c.at, c.steps = false, 0, 0 }

type zsnap struct {
	name    string
	props   map[string]string
	created time.Time
	data    string
}

type zds struct {
	origin  string // "dataset@snapshot"
	props   map[string]string
	snaps   []*zsnap
	mounted bool
	data    string // what the database would hold: tests compare it
}

// zsim is a small model of ZFS: datasets with snapshots, clones, rename and
// promote with ZFS's own rules (a snapshot with clones cannot be destroyed;
// promote moves the origin's older snapshots to the clone).
type zsim struct {
	j     *journal
	root  string
	ds    map[string]*zds
	clock func() time.Time
	crash *crasher
	// failOn makes the operations whose journal entry starts with it fail.
	failOn string
	// readFail makes the named read (Exists, Props, SnapshotInfos, Datasets,
	// Origin, Info) fail.
	readFail string
	// failRead makes the Nth read (counted in reads) fail, once.
	failRead, reads int
	// mounts are the read-only mounts open now.
	mounts []string
}

func (z *zsim) rf(op string) error {
	z.reads++
	if z.readFail == op || (z.failRead > 0 && z.reads == z.failRead) {
		return errors.New("injected: " + op)
	}
	return nil
}

func newZsim(j *journal, root string, clock func() time.Time, c *crasher) *zsim {
	return &zsim{j: j, root: root, ds: map[string]*zds{}, clock: clock, crash: c}
}

func (z *zsim) mutate(format string, args ...any) error {
	if err := z.crash.step(); err != nil {
		return err
	}
	entry := fmt.Sprintf(format, args...)
	if z.failOn != "" && strings.HasPrefix(entry, z.failOn) {
		return errors.New("injected: " + entry)
	}
	z.j.add("%s", entry)
	return nil
}

func (z *zsim) get(name string) (*zds, error) {
	d, ok := z.ds[name]
	if !ok {
		return nil, fmt.Errorf("cannot open '%s': dataset does not exist", name)
	}
	return d, nil
}

func (z *zsim) snap(dataset, name string) (*zds, *zsnap, error) {
	d, err := z.get(dataset)
	if err != nil {
		return nil, nil, err
	}
	for _, s := range d.snaps {
		if s.name == name {
			return d, s, nil
		}
	}
	return d, nil, fmt.Errorf("cannot open '%s@%s': snapshot does not exist", dataset, name)
}

func splitSnap(target string) (string, string) {
	ds, snap, _ := strings.Cut(target, "@")
	return ds, snap
}

// clonesOf lists the datasets cloned from dataset@snapshot.
func (z *zsim) clonesOf(dataset, snapshot string) []string {
	var out []string
	for n, d := range z.ds {
		if d.origin == dataset+"@"+snapshot {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func (z *zsim) Create(_ context.Context, name string, props map[string]string) error {
	if err := z.mutate("zfs create %s", name); err != nil {
		return err
	}
	if _, ok := z.ds[name]; ok {
		return fmt.Errorf("dataset already exists: %s", name)
	}
	z.ds[name] = &zds{props: map[string]string{}, mounted: props["canmount"] != "off"}
	for k, v := range props {
		if strings.Contains(k, ":") {
			z.ds[name].props[k] = v
		}
	}
	return nil
}

func (z *zsim) Snapshot(_ context.Context, s domain.Snapshot) error {
	if err := z.mutate("zfs snapshot %s", s); err != nil {
		return err
	}
	d, err := z.get(s.Dataset)
	if err != nil {
		return err
	}
	for _, x := range d.snaps {
		if x.name == s.Name {
			return fmt.Errorf("snapshot %s already exists", s)
		}
	}
	d.snaps = append(d.snaps, &zsnap{name: s.Name, props: map[string]string{}, created: z.clock(), data: d.data})
	return nil
}

func (z *zsim) Clone(ctx context.Context, origin domain.Snapshot, name string) error {
	return z.CloneWith(ctx, origin, name, nil)
}

func (z *zsim) CloneWith(_ context.Context, origin domain.Snapshot, name string, props map[string]string) error {
	if err := z.mutate("zfs clone %s %s", origin, name); err != nil {
		return err
	}
	_, s, err := z.snap(origin.Dataset, origin.Name)
	if err != nil {
		return err
	}
	if _, ok := z.ds[name]; ok {
		return fmt.Errorf("dataset already exists: %s", name)
	}
	d := &zds{origin: origin.String(), props: map[string]string{}, mounted: props["canmount"] != "off", data: s.data}
	for k, v := range props {
		if strings.Contains(k, ":") {
			d.props[k] = v
		}
	}
	z.ds[name] = d
	return nil
}

func (z *zsim) Rollback(_ context.Context, s domain.Snapshot) error {
	if err := z.mutate("zfs rollback %s", s); err != nil {
		return err
	}
	d, snap, err := z.snap(s.Dataset, s.Name)
	if err != nil {
		return err
	}
	if d.snaps[len(d.snaps)-1] != snap {
		return fmt.Errorf("cannot rollback to '%s': more recent snapshots exist", s)
	}
	d.data = snap.data
	return nil
}

func (z *zsim) destroyable(name string, d *zds) error {
	for _, s := range d.snaps {
		if c := z.clonesOf(name, s.name); len(c) > 0 {
			return fmt.Errorf("cannot destroy '%s@%s': snapshot has dependent clones %v", name, s.name, c)
		}
	}
	return nil
}

func (z *zsim) Destroy(_ context.Context, name string) error {
	if err := z.mutate("zfs destroy %s", name); err != nil {
		return err
	}
	d, err := z.get(name)
	if err != nil {
		return err
	}
	if err := z.destroyable(name, d); err != nil {
		return err
	}
	for n := range z.ds {
		if strings.HasPrefix(n, name+"/") {
			return fmt.Errorf("cannot destroy '%s': filesystem has children", name)
		}
	}
	delete(z.ds, name)
	return nil
}

func (z *zsim) DestroySnapshot(_ context.Context, s domain.Snapshot) error {
	if err := z.mutate("zfs destroy %s", s); err != nil {
		return err
	}
	d, _, err := z.snap(s.Dataset, s.Name)
	if err != nil {
		return err
	}
	if c := z.clonesOf(s.Dataset, s.Name); len(c) > 0 {
		return fmt.Errorf("cannot destroy '%s': snapshot has dependent clones", s)
	}
	for i, x := range d.snaps {
		if x.name == s.Name {
			d.snaps = append(d.snaps[:i], d.snaps[i+1:]...)
			break
		}
	}
	return nil
}

func (z *zsim) Exists(_ context.Context, name string) (bool, error) {
	_, ok := z.ds[name]
	return ok, z.rf("Exists")
}

func (z *zsim) Info(_ context.Context, name string) (domain.DatasetInfo, error) {
	if err := z.rf("Info"); err != nil {
		return domain.DatasetInfo{}, err
	}
	if _, err := z.get(name); err != nil {
		return domain.DatasetInfo{}, err
	}
	return domain.DatasetInfo{Mountpoint: filepath.Join(z.root, name), Used: 1 << 20, Referenced: 500 << 20}, nil
}

func (z *zsim) Datasets(_ context.Context, parent string) ([]string, error) {
	if err := z.rf("Datasets"); err != nil {
		return nil, err
	}
	if _, err := z.get(parent); err != nil {
		return nil, err
	}
	var out []string
	for n := range z.ds {
		if rest, ok := strings.CutPrefix(n, parent+"/"); ok && !strings.Contains(rest, "/") {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (z *zsim) SnapshotInfos(_ context.Context, name string) ([]domain.SnapshotInfo, error) {
	if err := z.rf("SnapshotInfos"); err != nil {
		return nil, err
	}
	d, err := z.get(name)
	if err != nil {
		return nil, err
	}
	var out []domain.SnapshotInfo
	for _, s := range d.snaps {
		props := map[string]string{}
		for k, v := range s.props {
			props[k] = v
		}
		out = append(out, domain.SnapshotInfo{Name: s.name, Created: s.created, Used: uint64(len(s.data)) + 100, Clones: z.clonesOf(name, s.name), Props: props})
	}
	return out, nil
}

func (z *zsim) Props(_ context.Context, name string) (map[string]string, error) {
	if err := z.rf("Props"); err != nil {
		return nil, err
	}
	d, err := z.get(name)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for k, v := range d.props {
		out[k] = v
	}
	return out, nil
}

func (z *zsim) SetProps(_ context.Context, target string, props map[string]string) error {
	if err := z.mutate("zfs set %v %s", sortedPairs(props), target); err != nil {
		return err
	}
	name, snapName := splitSnap(target)
	d, err := z.get(name)
	if err != nil {
		return err
	}
	dst := d.props
	if snapName != "" {
		_, s, err := z.snap(name, snapName)
		if err != nil {
			return err
		}
		dst = s.props
	}
	for k, v := range props {
		switch {
		case strings.Contains(k, ":"):
			dst[k] = v
		case k == "canmount" && snapName == "":
			d.props["canmount"] = v
			if v == "off" {
				d.mounted = false
			}
		}
	}
	return nil
}

func sortedPairs(m map[string]string) string {
	var out []string
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func (z *zsim) Unset(_ context.Context, name string, keys ...string) error {
	if err := z.mutate("zfs inherit %v %s", keys, name); err != nil {
		return err
	}
	d, err := z.get(name)
	if err != nil {
		return err
	}
	for _, k := range keys {
		delete(d.props, k)
	}
	return nil
}

func (z *zsim) Rename(_ context.Context, from, to string) error {
	if err := z.mutate("zfs rename %s %s", from, to); err != nil {
		return err
	}
	d, err := z.get(from)
	if err != nil {
		return err
	}
	if _, ok := z.ds[to]; ok {
		return fmt.Errorf("cannot rename to '%s': dataset already exists", to)
	}
	delete(z.ds, from)
	z.ds[to] = d
	d.mounted = false // a renamed dataset is mounted again by Mount
	for _, o := range z.ds {
		if rest, ok := strings.CutPrefix(o.origin, from+"@"); ok {
			o.origin = to + "@" + rest
		}
	}
	return nil
}

func (z *zsim) RenameSnapshot(_ context.Context, s domain.Snapshot, name string) error {
	if err := z.mutate("zfs rename %s @%s", s, name); err != nil {
		return err
	}
	d, snap, err := z.snap(s.Dataset, s.Name)
	if err != nil {
		return err
	}
	for _, x := range d.snaps {
		if x.name == name {
			return fmt.Errorf("snapshot %s@%s already exists", s.Dataset, name)
		}
	}
	snap.name = name
	for _, o := range z.ds {
		if o.origin == s.String() {
			o.origin = s.Dataset + "@" + name
		}
	}
	return nil
}

func (z *zsim) Promote(_ context.Context, name string) error {
	if err := z.mutate("zfs promote %s", name); err != nil {
		return err
	}
	clone, err := z.get(name)
	if err != nil {
		return err
	}
	if clone.origin == "" {
		return fmt.Errorf("cannot promote '%s': not a cloned filesystem", name)
	}
	parentName, snapName := splitSnap(clone.origin)
	parent, _, err := z.snap(parentName, snapName)
	if err != nil {
		return err
	}
	cut := -1
	for i, s := range parent.snaps {
		if s.name == snapName {
			cut = i
		}
	}
	moving := parent.snaps[:cut+1]
	for _, m := range moving {
		for _, c := range clone.snaps {
			if c.name == m.name {
				return fmt.Errorf("cannot promote '%s': conflicting snapshot %s", name, m.name)
			}
		}
	}
	for _, m := range moving {
		for _, o := range z.ds {
			if o.origin == parentName+"@"+m.name {
				o.origin = name + "@" + m.name
			}
		}
	}
	clone.snaps = append(append([]*zsnap{}, moving...), clone.snaps...)
	parent.snaps = append([]*zsnap{}, parent.snaps[cut+1:]...)
	clone.origin, parent.origin = parent.origin, name+"@"+snapName
	return nil
}

func (z *zsim) Origin(_ context.Context, name string) (string, error) {
	if err := z.rf("Origin"); err != nil {
		return "", err
	}
	d, err := z.get(name)
	if err != nil {
		return "", err
	}
	return d.origin, nil
}

func (z *zsim) Mount(_ context.Context, name string) error {
	if err := z.mutate("zfs mount %s", name); err != nil {
		return err
	}
	d, err := z.get(name)
	if err != nil {
		return err
	}
	if d.props["canmount"] == "off" {
		return fmt.Errorf("cannot mount '%s': 'canmount' property is set to 'off'", name)
	}
	d.mounted = true
	return nil
}

// write changes what the database on dataset holds (a stand-in for SQL).
func (z *zsim) write(name, data string) { z.ds[name].data = data }

// names lists every dataset, sorted.
func (z *zsim) names() []string {
	var out []string
	for n := range z.ds {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// MountReadOnly records a read-only mount; mounts lists the directories open now.
func (z *zsim) MountReadOnly(_ context.Context, target string) (string, error) {
	if err := z.mutate("mount ro %s", target); err != nil {
		return "", err
	}
	ds, _ := splitSnap(target)
	if _, err := z.get(ds); err != nil {
		return "", err
	}
	dir := filepath.Join(z.root, fmt.Sprintf("ro-%d", len(z.mounts)+1))
	z.mounts = append(z.mounts, dir)
	return dir, nil
}

func (z *zsim) Unmount(_ context.Context, dir string) error {
	if err := z.mutate("unmount %s", filepath.Base(dir)); err != nil {
		return err
	}
	for i, m := range z.mounts {
		if m == dir {
			z.mounts = append(z.mounts[:i], z.mounts[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("%s is not mounted", dir)
}
