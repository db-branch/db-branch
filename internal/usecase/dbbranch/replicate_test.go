package dbbranch_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
)

// fakeStreams models zfs send/recv: a stream is "snap1,snap2,..." and
// receiving it adds those snapshots to the dataset.
type fakeStreams struct {
	j       *journal
	storage *fakeStorage
	snaps   map[string][]string
	failOn  string
}

func (f *fakeStreams) Send(_ context.Context, ds, from, to string, w io.Writer) error {
	f.j.add("send %s %s..%s", ds, from, to)
	if f.failOn == "send" {
		return errors.New("send failed")
	}
	var out []string
	started := from == ""
	for _, s := range f.snaps[ds] {
		if started {
			out = append(out, s)
		}
		if s == from {
			started = true
		}
		if s == to {
			break
		}
	}
	_, err := io.WriteString(w, strings.Join(out, ","))
	return err
}

func (f *fakeStreams) Receive(_ context.Context, ds string, r io.Reader) error {
	b, _ := io.ReadAll(r)
	f.j.add("recv %s %s", ds, b)
	if f.failOn == "recv" {
		return errors.New("recv failed")
	}
	f.storage.datasets[ds] = true
	for _, s := range strings.Split(string(b), ",") {
		f.snaps[ds] = append(f.snaps[ds], s)
		f.storage.snaps[ds+"@"+s] = true
	}
	return nil
}

func (f *fakeStreams) Children(_ context.Context, parent string) ([]string, error) {
	if f.failOn == "children" {
		return nil, errors.New("children failed")
	}
	var out []string
	for ds := range f.storage.datasets {
		if rest, ok := strings.CutPrefix(ds, parent+"/"); ok && !strings.Contains(rest, "/") {
			out = append(out, rest)
		}
	}
	return out, nil
}

func (f *fakeStreams) Snapshots(_ context.Context, ds string) ([]string, error) {
	if f.failOn == "snapshots" {
		return nil, errors.New("snapshots failed")
	}
	var out []string
	for _, s := range f.snaps[ds] {
		if f.storage.snaps[ds+"@"+s] {
			out = append(out, s)
		}
	}
	return out, nil
}

// fakeSource reads streams straight from another env's service.
type fakeSource struct {
	origin *dbbranch.Service
	err    error
}

func (f *fakeSource) Open(ctx context.Context, node string, base domain.Name, from, to string) (io.ReadCloser, error) {
	if f.err != nil {
		return nil, f.err
	}
	var buf bytes.Buffer
	if err := f.origin.SendBase(ctx, base, from, to, &buf); err != nil {
		return nil, err
	}
	return io.NopCloser(&buf), nil
}

// pair is two nodes sharing one store: node1 holds base "prod", node2 replicates it.
type pair struct {
	origin, replica *env
	streams         *fakeStreams
	source          *fakeSource
}

func newPair(t *testing.T) *pair {
	t.Helper()
	o := newEnv(t)
	o.clustered()
	r := newEnv(t)
	r.store = o.store
	r.svc.Store = o.store
	r.svc.Node = "node2"
	r.clustered()
	o.svc.Streams = &fakeStreams{j: o.j, storage: o.storage, snaps: map[string][]string{}}
	rs := &fakeStreams{j: r.j, storage: r.storage, snaps: map[string][]string{}}
	src := &fakeSource{origin: o.svc}
	r.svc.Streams, r.svc.Source = rs, src
	return &pair{origin: o, replica: r, streams: rs, source: src}
}

// snapshot takes a base snapshot on the origin and records it for the fake send.
func (p *pair) snapshot(t *testing.T) string {
	t.Helper()
	p.origin.now = p.origin.now.Add(60e9)
	snap, err := p.origin.svc.SnapshotBase(ctx, name("prod"), nil)
	if err != nil {
		t.Fatal(err)
	}
	return snap.Name
}

func (p *pair) withBase(t *testing.T) domain.Base {
	b := p.origin.withBase(t)
	o := p.origin.svc.Streams.(*fakeStreams)
	o.snaps["tank/db-branch/bases/prod"] = []string{b.Snapshots[0].Name}
	return b
}

func (p *pair) syncOriginSnaps(t *testing.T) {
	b, _ := p.origin.store.GetBase(ctx, name("prod"))
	o := p.origin.svc.Streams.(*fakeStreams)
	o.snaps["tank/db-branch/bases/prod"] = nil
	for _, s := range b.Snapshots {
		o.snaps["tank/db-branch/bases/prod"] = append(o.snaps["tank/db-branch/bases/prod"], s.Name)
	}
}

func TestReplicate_初回は全体を受け取り保持を記録する(t *testing.T) {
	p := newPair(t)
	base := p.withBase(t)
	if err := p.replica.svc.Replicate(ctx); err != nil {
		t.Fatal(err)
	}
	snap := base.Snapshots[0].Name
	if !p.replica.j.has(fmt.Sprintf("recv tank/db-branch/replicas/prod %s", snap)) {
		t.Errorf("journal = %v", p.replica.j.entries)
	}
	got, _ := p.origin.store.GetBase(ctx, name("prod"))
	if !got.HasReplica(snap, "node2") {
		t.Errorf("replicas = %v", got.Replicas)
	}
	if !p.replica.storage.datasets["tank/db-branch/replicas"] {
		t.Error("the replicas parent should be created")
	}
}

func TestReplicate_次からは持っている最新から差分だけ受け取る(t *testing.T) {
	p := newPair(t)
	base := p.withBase(t)
	_ = p.replica.svc.Replicate(ctx)
	s2 := p.snapshot(t)
	s3 := p.snapshot(t)
	p.syncOriginSnaps(t)
	p.replica.j.entries = nil
	if err := p.replica.svc.Replicate(ctx); err != nil {
		t.Fatal(err)
	}
	if !p.replica.j.has(fmt.Sprintf("recv tank/db-branch/replicas/prod %s,%s", s2, s3)) {
		t.Errorf("journal = %v", p.replica.j.entries)
	}
	got, _ := p.origin.store.GetBase(ctx, name("prod"))
	for _, s := range []string{base.Snapshots[0].Name, s2, s3} {
		if !got.HasReplica(s, "node2") {
			t.Errorf("%s not recorded: %v", s, got.Replicas)
		}
	}
	p.replica.j.entries = nil
	_ = p.replica.svc.Replicate(ctx)
	if len(p.replica.j.entries) != 0 {
		t.Errorf("up to date, but: %v", p.replica.j.entries)
	}
}

func TestReplicate_自分のベースや未完成のベースは扱わない(t *testing.T) {
	p := newPair(t)
	p.withBase(t)
	_ = p.origin.store.PutBase(ctx, domain.Base{Name: name("empty"), Node: "node1", Status: domain.BaseProvisioning})
	if err := p.origin.svc.Replicate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.replica.svc.Replicate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, e := range append(p.origin.j.entries, p.replica.j.entries...) {
		if strings.Contains(e, "empty") || strings.HasPrefix(e, "recv tank/db-branch/replicas/prod") && p.origin.j.has(e) {
			t.Errorf("unexpected %s", e)
		}
	}
}

func TestReplicate_手元のコピーが合わなければ作り直す(t *testing.T) {
	p := newPair(t)
	base := p.withBase(t)
	// A copy of an older base with the same name.
	p.replica.storage.datasets["tank/db-branch/replicas"] = true
	p.replica.storage.datasets["tank/db-branch/replicas/prod"] = true
	p.streams.snaps["tank/db-branch/replicas/prod"] = []string{"s-old"}
	p.replica.storage.snaps["tank/db-branch/replicas/prod@s-old"] = true
	if err := p.replica.svc.Replicate(ctx); err != nil {
		t.Fatal(err)
	}
	if !p.replica.j.has("zfs destroy tank/db-branch/replicas/prod") || !p.replica.j.has("recv tank/db-branch/replicas/prod "+base.Snapshots[0].Name) {
		t.Errorf("journal = %v", p.replica.j.entries)
	}
}

func TestReplicate_削除されたベースのコピーを消す(t *testing.T) {
	p := newPair(t)
	p.withBase(t)
	_ = p.replica.svc.Replicate(ctx)
	if err := p.origin.svc.DeleteBase(ctx, name("prod")); err != nil {
		t.Fatal(err)
	}
	if err := p.replica.svc.Replicate(ctx); err != nil {
		t.Fatal(err)
	}
	if !p.replica.j.has("zfs destroy tank/db-branch/replicas/prod") {
		t.Errorf("journal = %v", p.replica.j.entries)
	}
}

func TestReplicate_失敗は理由を返し他のベースは続ける(t *testing.T) {
	for _, failOn := range []string{"recv", "snapshots", "children"} {
		p := newPair(t)
		p.withBase(t)
		p.streams.failOn = failOn
		if err := p.replica.svc.Replicate(ctx); err == nil {
			t.Errorf("%s: want error", failOn)
		}
	}
	p := newPair(t)
	p.withBase(t)
	p.source.err = errors.New("node1 unreachable")
	if err := p.replica.svc.Replicate(ctx); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("source: %v", err)
	}
	p = newPair(t)
	p.replica.svc.Streams = nil
	if err := p.replica.svc.Replicate(ctx); err != nil {
		t.Errorf("not configured: %v", err)
	}
}

func TestSendBase_自分のベースの既知のスナップショットだけ送る(t *testing.T) {
	p := newPair(t)
	base := p.withBase(t)
	var buf bytes.Buffer
	if err := p.replica.svc.SendBase(ctx, name("prod"), "", base.Snapshots[0].Name, &buf); !errors.Is(err, dbbranch.ErrOtherNode) {
		t.Errorf("not the holder: %v", err)
	}
	if err := p.origin.svc.SendBase(ctx, name("prod"), "", "s-nope", &buf); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown to: %v", err)
	}
	if err := p.origin.svc.SendBase(ctx, name("prod"), "s-nope", base.Snapshots[0].Name, &buf); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown from: %v", err)
	}
	if err := p.origin.svc.SendBase(ctx, name("nope"), "", "s1", &buf); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown base: %v", err)
	}
	p.origin.svc.Streams = nil
	if err := p.origin.svc.SendBase(ctx, name("prod"), "", base.Snapshots[0].Name, &buf); err == nil {
		t.Error("no streams: want error")
	}
}

func TestPlaceBranch_コピーを持つノードにも置ける(t *testing.T) {
	p := newPair(t)
	p.withBase(t)
	_ = p.replica.svc.Replicate(ctx)
	// node1 has no memory left; node2 holds a copy.
	p.replica.svc.Nodes.(*fakeNodes).nodes[0].MemoryMB = 400
	node, err := p.replica.svc.PlaceBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod")})
	if err != nil || node != "node2" {
		t.Fatalf("PlaceBranch = %q, %v", node, err)
	}
	b, err := p.replica.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.Node != "node2" || !p.replica.j.has(fmt.Sprintf("zfs clone tank/db-branch/replicas/prod@%s tank/db-branch/branches/x", b.Snapshot)) {
		t.Errorf("branch %+v journal %v", b, p.replica.j.entries)
	}
}
