package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/config"
	"github.com/db-branch/db-branch/internal/infrastructure/filestore"
	"github.com/db-branch/db-branch/internal/infrastructure/notifier"
	"github.com/db-branch/db-branch/internal/interface/httpapi"
	"github.com/db-branch/db-branch/internal/usecase/arccap"
	"github.com/db-branch/db-branch/internal/usecase/auth"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
	"github.com/db-branch/db-branch/internal/usecase/notify"
	"github.com/db-branch/db-branch/internal/usecase/operation"
	"github.com/db-branch/db-branch/internal/usecase/settings"
)

func TestWireModulesAndSettings_通知URLのSSRF設定を拒否しAllowPrivateIPsに従う(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	setup := func(allowPrivate bool) *httpapi.Server {
		cfg := config.Default()
		cfg.DataDir = t.TempDir()
		cfg.Notify.AllowPrivateIPs = allowPrivate
		st := &filestore.Store{Path: filepath.Join(cfg.DataDir, "state.json")}
		svc := &dbbranch.Service{Store: st}
		srv := &httpapi.Server{Auth: &auth.Service{}}
		arc := &arcNode{ctl: &arccap.Controller{}}
		problems := &moduleProblems{m: map[string]string{}}
		wCtx, wCancel := context.WithCancel(ctx)
		_ = wireModulesAndSettings(wCtx, cfg, svc, srv, nil, nil, nil, nil,
			func() int { return 1 }, problems, arc, nil, func() bool { return true }, discardLog())
		wCancel()
		return srv
	}

	t.Cleanup(func() {
		notifySlack.Store("")
		notifyWebhook.Store("")
		notifySecret.Store("")
	})

	// Default (allow_private_ips = false)
	srv := setup(false)
	for _, key := range []string{"notify.slack_url", "notify.webhook_url"} {
		for _, bad := range []string{
			"http://hooks.slack.com/services/T1/B2/SECRET_TOKEN",
			"https://169.254.169.254/latest/meta-data/SECRET_TOKEN",
			"https://127.0.0.1/SECRET_TOKEN",
			"https://10.0.0.5/SECRET_TOKEN",
		} {
			err := srv.Settings.Put(ctx, settings.CoreScope, map[string]string{key: bad})
			if !errors.Is(err, settings.ErrInvalid) {
				t.Errorf("Put(%s=%q) = %v, want ErrInvalid", key, bad, err)
			} else if strings.Contains(err.Error(), "SECRET_TOKEN") {
				t.Errorf("Put(%s=%q) leaked secret in error: %v", key, bad, err)
			}
		}
		if err := srv.Settings.Put(ctx, settings.CoreScope, map[string]string{key: "https://hooks.slack.com/services/T1/B2/ok"}); err != nil {
			t.Errorf("Put(%s=valid) = %v", key, err)
		}
		if err := srv.Settings.Put(ctx, settings.CoreScope, map[string]string{key: ""}); err != nil {
			t.Errorf("Put(%s=empty) = %v", key, err)
		}
	}

	// Opt-in (allow_private_ips = true)
	srvPriv := setup(true)
	if err := srvPriv.Settings.Put(ctx, settings.CoreScope, map[string]string{"notify.webhook_url": "https://10.0.0.5/hook"}); err != nil {
		t.Errorf("Put(private with opt-in) = %v", err)
	}
	for _, alwaysBad := range []string{"https://127.0.0.1/hook", "https://169.254.169.254/latest/meta-data/", "http://10.0.0.5/hook"} {
		if err := srvPriv.Settings.Put(ctx, settings.CoreScope, map[string]string{"notify.webhook_url": alwaysBad}); !errors.Is(err, settings.ErrInvalid) {
			t.Errorf("Put(%q with opt-in) = %v, want ErrInvalid", alwaysBad, err)
		}
	}
}

type recordSender struct {
	mu      sync.Mutex
	notices []notify.Notice
}

func (r *recordSender) Send(_ context.Context, n notify.Notice) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notices = append(r.notices, n)
	return nil
}

func (r *recordSender) list() []notify.Notice {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]notify.Notice, len(r.notices))
	copy(out, r.notices)
	return out
}

func TestSingleNodeNotifyLoop_単一ノードで期限切れ間近と操作失敗を通知できる(t *testing.T) {
	ctx := context.Background()
	st := &filestore.Store{Path: filepath.Join(t.TempDir(), "state.json")}
	now := time.Now().UTC().Truncate(time.Second)
	exp := now.Add(30 * time.Minute)
	name, err := domain.ParseName("soon-branch")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutBranch(ctx, domain.Branch{Name: name, Owner: "alice", ExpiresAt: &exp}); err != nil {
		t.Fatal(err)
	}
	ops := &operation.Manager{Clock: func() time.Time { return now }, Node: "node1"}
	op, err := ops.Start("branch.create", operation.Target{Resource: "branch", Name: "bad-branch"}, func(context.Context, func(string)) error {
		return errors.New("boom")
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := ops.Get(op.ID)
		if err == nil && got.State == operation.Failed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("operation did not reach failed state")
		}
		time.Sleep(5 * time.Millisecond)
	}

	state := singleNotifyState{branches: st.ListBranches, ops: ops.List}
	if nodes, err := state.ListNodes(ctx); err != nil || len(nodes) != 0 {
		t.Fatalf("ListNodes = %v, %v", nodes, err)
	}

	svc := newNotifier(state, true, 0)
	if svc.DownAfter != 0 {
		t.Errorf("single-node DownAfter = %v, want 0", svc.DownAfter)
	}
	if len(svc.Senders) != 2 {
		t.Fatalf("len(Senders) = %d, want 2", len(svc.Senders))
	}
	if sl, ok := svc.Senders[0].(notifier.Slack); !ok || !sl.AllowPrivateIPs {
		t.Errorf("Slack sender = %#v, want AllowPrivateIPs=true", svc.Senders[0])
	}
	if wh, ok := svc.Senders[1].(notifier.Webhook); !ok || !wh.AllowPrivateIPs {
		t.Errorf("Webhook sender = %#v, want AllowPrivateIPs=true", svc.Senders[1])
	}
	svc.Since = now.Add(-time.Minute)
	svc.Clock = func() time.Time { return now }
	rec := &recordSender{}
	svc.Senders = []notify.Sender{rec}

	// When leading() is false, notifyLoop does not send notices.
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		notifyLoop(loopCtx, 5*time.Millisecond, func() bool { return false }, svc, discardLog())
	}()
	time.Sleep(25 * time.Millisecond)
	cancel()
	<-done
	if got := rec.list(); len(got) != 0 {
		t.Fatalf("non-leader sent %d notices, want 0", len(got))
	}

	// Single node (leading() == true) sends branch.expiring and operation.failed.
	loopCtx, cancel = context.WithCancel(ctx)
	done = make(chan struct{})
	go func() {
		defer close(done)
		notifyLoop(loopCtx, 5*time.Millisecond, func() bool { return true }, svc, discardLog())
	}()
	deadline = time.Now().Add(2 * time.Second)
	for len(rec.list()) < 2 {
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	got := rec.list()
	events := map[string]bool{}
	for _, notice := range got {
		events[notice.Event+":"+notice.Subject] = true
	}
	if !events["branch.expiring:soon-branch"] || !events["operation.failed:bad-branch"] {
		t.Errorf("got notices = %+v", got)
	}

	// Already-canceled context returns immediately without checking or logging warnings.
	logBuf := &syncBuf{}
	canceled, cancelNow := context.WithCancel(ctx)
	cancelNow()
	notifyLoop(canceled, 0, func() bool { return true }, svc, slog.New(slog.NewTextHandler(logBuf, nil)))
	if logBuf.String() != "" {
		t.Errorf("canceled notifyLoop logged: %s", logBuf.String())
	}

	// Legacy invalid URL stored in notifySlack produces a warning without leaking the secret URL.
	t.Cleanup(func() { notifySlack.Store("") })
	notifySlack.Store("http://hooks.slack.com/services/T1/B2/LEGACY_SECRET_TOKEN")
	badSvc := newNotifier(state, false, 0)
	badSvc.Since = now.Add(-time.Minute)
	badSvc.Clock = func() time.Time { return now }
	loopCtx, cancel = context.WithCancel(ctx)
	done = make(chan struct{})
	go func() {
		defer close(done)
		notifyLoop(loopCtx, 5*time.Millisecond, func() bool { return true }, badSvc, slog.New(slog.NewTextHandler(logBuf, nil)))
	}()
	deadline = time.Now().Add(2 * time.Second)
	for logBuf.String() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	logged := logBuf.String()
	if !strings.Contains(logged, "sending notices") {
		t.Errorf("expected warning log, got: %q", logged)
	}
	if strings.Contains(logged, "LEGACY_SECRET_TOKEN") {
		t.Errorf("notifyLoop warning log leaked secret URL: %s", logged)
	}
}

type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
