package auth_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/auth"
)

type memStore struct {
	c         auth.Credentials
	ok        bool
	fail      error
	saveErr   error
	failAfter int // >0: Load fails after this many more calls
}

func (m *memStore) Load() (auth.Credentials, error) {
	if m.failAfter > 0 {
		m.failAfter--
		if m.failAfter == 0 {
			return auth.Credentials{}, errors.New("disk")
		}
	}
	if m.fail != nil {
		return auth.Credentials{}, m.fail
	}
	if !m.ok {
		return auth.Credentials{}, auth.ErrNoCredentials
	}
	return m.c, nil
}

func (m *memStore) Save(c auth.Credentials) error {
	if m.saveErr != nil {
		return m.saveErr
	}
	if m.fail != nil {
		return m.fail
	}
	m.c, m.ok = c, true
	return nil
}

// sha stands in for argon2id; the real hasher lives in infrastructure.
type sha struct{}

func (sha) Hash(password string, salt []byte) []byte {
	sum := sha256.Sum256(append(salt, password...))
	return sum[:]
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func newService(t *testing.T) (*auth.Service, *clock, *memStore) {
	t.Helper()
	c := &clock{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	st := &memStore{}
	s := &auth.Service{Store: st, Clock: c.Now, Hasher: sha{}}
	if err := s.SetPassword("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	return s, c, st
}

func TestLogin_正しいパスワードでセッションとCSRFトークンを発行する(t *testing.T) {
	s, c, st := newService(t)

	sess, err := s.Login("admin", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.Token) < 32 || len(sess.CSRF) < 32 || sess.Token == sess.CSRF || sess.User != "admin" {
		t.Errorf("session = %+v", sess)
	}
	if !sess.ExpiresAt.Equal(c.now.Add(auth.DefaultSessionTTL)) {
		t.Errorf("expires = %v", sess.ExpiresAt)
	}
	if string(st.c.Hash) == "correct horse battery" || len(st.c.Salt) < 16 {
		t.Error("password must be stored hashed with a salt")
	}
}

func TestLogin_ユーザー名かパスワードが違えば失敗する(t *testing.T) {
	s, _, _ := newService(t)
	for _, tc := range [][2]string{{"admin", "wrong"}, {"root", "correct horse battery"}} {
		if _, err := s.Login(tc[0], tc[1]); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Errorf("%v: error = %v", tc, err)
		}
	}
}

func TestLogin_失敗が続いたら一定時間締め出す(t *testing.T) {
	s, c, _ := newService(t)
	for i := 0; i < 5; i++ {
		_, _ = s.Login("admin", "wrong")
	}
	_, err := s.Login("admin", "correct horse battery")
	var rl *auth.RateLimitedError
	if !errors.As(err, &rl) || rl.RetryAfter <= 0 {
		t.Fatalf("error = %v, want RateLimitedError", err)
	}
	c.now = c.now.Add(time.Minute + time.Second)
	if _, err := s.Login("admin", "correct horse battery"); err != nil {
		t.Errorf("after lockout: %v", err)
	}
}

func TestSession_半分を過ぎたら同じセッションのまま延び期限切れは無効(t *testing.T) {
	s, c, _ := newService(t)
	sess, _ := s.Login("admin", "correct horse battery")

	c.now = c.now.Add(time.Hour)
	got, err := s.Session(sess.Token)
	if err != nil || got.Renewed || got.Token != sess.Token || got.CSRF != sess.CSRF {
		t.Fatalf("early: %+v %v", got, err)
	}
	c.now = c.now.Add(4 * 24 * time.Hour)
	got, err = s.Session(sess.Token)
	if err != nil || !got.Renewed || got.Token == sess.Token || got.CSRF != sess.CSRF || !got.ExpiresAt.Equal(c.now.Add(auth.DefaultSessionTTL)) {
		t.Fatalf("renewal: %+v %v", got, err)
	}
	c.now = c.now.Add(8 * 24 * time.Hour)
	if _, err := s.Session(got.Token); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("expired: %v", err)
	}
	for _, bad := range []string{"nope", "x.y", sess.Token + "x", "e30." + strings.Split(sess.Token, ".")[1]} {
		if _, err := s.Session(bad); !errors.Is(err, auth.ErrNoSession) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestSession_再起動しても別の窓口でも同じ鍵で通る(t *testing.T) {
	s, c, st := newService(t)
	sess, _ := s.Login("admin", "correct horse battery")
	restarted := &auth.Service{Store: st, Clock: c.Now, Hasher: sha{}}
	if got, err := restarted.Session(sess.Token); err != nil || got.User != "admin" {
		t.Errorf("after restart: %+v %v", got, err)
	}
}

func TestSession_期間は設定で変えられる(t *testing.T) {
	s, c, _ := newService(t)
	s.TTL = func() time.Duration { return time.Hour }
	sess, _ := s.Login("admin", "correct horse battery")
	if !sess.ExpiresAt.Equal(c.now.Add(time.Hour)) {
		t.Errorf("expires %v", sess.ExpiresAt)
	}
	s.TTL = func() time.Duration { return 0 }
	if sess, _ := s.Login("admin", "correct horse battery"); !sess.ExpiresAt.Equal(c.now.Add(auth.DefaultSessionTTL)) {
		t.Errorf("zero means default: %v", sess.ExpiresAt)
	}
}

func TestBegin_鍵のない古い資格情報には鍵を足す(t *testing.T) {
	s, _, st := newService(t)
	st.c.SessionKey = nil
	if _, err := s.Begin("admin", "admin", nil); err != nil {
		t.Fatal(err)
	}
	if len(st.c.SessionKey) != 32 {
		t.Errorf("key = %x", st.c.SessionKey)
	}
	if _, err := (&auth.Service{Store: &memStore{}, Clock: time.Now}).Begin("x", "x", nil); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("no credentials: %v", err)
	}
	if _, err := (&auth.Service{Store: &memStore{}, Clock: time.Now}).Session("a.b"); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("no credentials session: %v", err)
	}
}

func TestLogout_セッションを消す(t *testing.T) {
	s, _, _ := newService(t)
	sess, _ := s.Login("admin", "correct horse battery")
	s.Logout(sess.Token)
	if _, err := s.Session(sess.Token); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("after logout: %v", err)
	}
}

func TestChangePassword_今のセッションだけ残して他は無効にする(t *testing.T) {
	s, _, _ := newService(t)
	mine, _ := s.Login("admin", "correct horse battery")
	other, _ := s.Login("admin", "correct horse battery")

	fresh, err := s.ChangePassword(mine.Token, "correct horse battery", "a much better passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Session(fresh.Token); err != nil || got.CSRF != mine.CSRF {
		t.Errorf("own session should continue with the fresh token: %+v %v", got, err)
	}
	if _, err := s.Session(mine.Token); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("the old token ends: %v", err)
	}
	if _, err := s.Session(other.Token); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("other session should end: %v", err)
	}
	if _, err := s.Login("admin", "a much better passphrase"); err != nil {
		t.Errorf("new password: %v", err)
	}
}

func TestChangePassword_今のパスワードが違うか新しいパスワードが短ければ拒否する(t *testing.T) {
	s, _, _ := newService(t)
	sess, _ := s.Login("admin", "correct horse battery")
	if _, err := s.ChangePassword(sess.Token, "wrong", "a much better passphrase"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("wrong current: %v", err)
	}
	if _, err := s.ChangePassword(sess.Token, "correct horse battery", "short"); !errors.Is(err, auth.ErrWeakPassword) {
		t.Errorf("short: %v", err)
	}
	if _, err := s.ChangePassword("nope", "correct horse battery", "a much better passphrase"); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("no session: %v", err)
	}
}

func TestSetPassword_リセットすると全セッションが無効になる(t *testing.T) {
	s, _, st := newService(t)
	sess, _ := s.Login("admin", "correct horse battery")

	// A reset from the CLI writes the store directly; the agent sees the new generation.
	other := &auth.Service{Store: st, Clock: time.Now, Hasher: sha{}}
	if err := other.SetPassword("reset by root on the node"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(sess.Token); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("session after reset: %v", err)
	}
}

func Test保存先の失敗はエラーにする(t *testing.T) {
	s, _, st := newService(t)
	sess, _ := s.Login("admin", "correct horse battery")
	st.fail = errors.New("disk")
	if _, err := s.Login("admin", "correct horse battery"); err == nil {
		t.Error("Login: want error")
	}
	if _, err := s.Session(sess.Token); err == nil {
		t.Error("Session: want error")
	}
	if err := s.SetPassword("whatever whatever"); err == nil {
		t.Error("SetPassword: want error")
	}
	if _, err := s.ChangePassword(sess.Token, "correct horse battery", "a much better passphrase"); err == nil {
		t.Error("ChangePassword: want error")
	}
}

func TestLogin_まだパスワードがなければ失敗する(t *testing.T) {
	s := &auth.Service{Store: &memStore{}, Clock: time.Now, Hasher: sha{}}
	if _, err := s.Login("admin", "x"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("error = %v", err)
	}
}

func TestGeneratePassword_初期パスワードは十分に長い(t *testing.T) {
	a, b := auth.GeneratePassword(), auth.GeneratePassword()
	if len(a) < 20 || a == b {
		t.Errorf("%q %q", a, b)
	}
}

func TestBegin_モジュールが認証した人のセッションを始める(t *testing.T) {
	s, _, _ := newService(t)
	sess, err := s.Begin("oidc:alice", "Alice", []string{"viewer"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Session(sess.Token)
	if err != nil || got.User != "oidc:alice" || got.Name != "Alice" || got.Roles[0] != "viewer" {
		t.Errorf("session %+v %v", got, err)
	}
}

func TestBegin_資格情報を読めなければ始めない(t *testing.T) {
	s, _, st := newService(t)
	st.fail = errors.New("raft down")
	if _, err := s.Begin("admin", "admin", nil); err == nil {
		t.Error("want error")
	}
}

func TestLogout_無効なトークンは何もせず期限の過ぎた記録は片付ける(t *testing.T) {
	s, c, _ := newService(t)
	s.Logout("garbage")
	a, _ := s.Login("admin", "correct horse battery")
	s.Logout(a.Token)
	c.now = c.now.Add(8 * 24 * time.Hour)
	b, _ := s.Login("admin", "correct horse battery")
	s.Logout(b.Token) // prunes a's record
	if _, err := s.Session(b.Token); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("after logout: %v", err)
	}
}

func Test細かな失敗(t *testing.T) {
	if (&auth.RateLimitedError{RetryAfter: 30 * time.Second}).Error() == "" {
		t.Error("message")
	}
	s, _, st := newService(t)
	st.c.SessionKey = nil
	st.saveErr = errors.New("disk")
	if _, err := s.Begin("admin", "admin", nil); err == nil {
		t.Error("key save failure: want error")
	}
	s, _, st = newService(t)
	sess, _ := s.Login("admin", "correct horse battery")
	if _, err := s.Session("@@@." + strings.Split(sess.Token, ".")[1]); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("bad body: %v", err)
	}
	st.saveErr = errors.New("disk")
	if _, err := s.ChangePassword(sess.Token, "correct horse battery", "a much better passphrase"); err == nil {
		t.Error("save failure: want error")
	}
	st.saveErr = nil
	st.failAfter = 2 // Session and the first Load succeed, the reload fails
	if _, err := s.ChangePassword(sess.Token, "correct horse battery", "a much better passphrase"); err == nil {
		t.Error("reload failure: want error")
	}
}

func TestSetInitialPassword_最初のログインでパスワードの変更を求める(t *testing.T) {
	s, _, _ := newService(t)
	if err := s.SetInitialPassword("admin"); err != nil {
		t.Fatal(err)
	}
	sess, err := s.Login("admin", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if !sess.MustChange {
		t.Error("the first session must be asked to change the password")
	}
	again, err := s.Session(sess.Token)
	if err != nil || !again.MustChange {
		t.Errorf("session lookup: %+v %v", again, err)
	}
}

func TestSetInitialPassword_パスワードを変えたら求めなくなる(t *testing.T) {
	s, _, st := newService(t)
	_ = s.SetInitialPassword("admin")
	sess, _ := s.Login("admin", "admin")
	fresh, err := s.ChangePassword(sess.Token, "admin", "a much better passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.MustChange || st.c.MustChange {
		t.Errorf("still asked to change: session %v stored %v", fresh.MustChange, st.c.MustChange)
	}
	next, err := s.Login("admin", "a much better passphrase")
	if err != nil || next.MustChange {
		t.Errorf("login after the change: %+v %v", next, err)
	}
}

func TestSetPassword_ふつうのパスワード設定では変更を求めない(t *testing.T) {
	s, _, st := newService(t)
	_ = s.SetInitialPassword("admin")
	if err := s.SetPassword("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	sess, _ := s.Login("admin", "correct horse battery")
	if sess.MustChange || st.c.MustChange {
		t.Error("an ordinary reset must not ask for another change")
	}
}

func TestMustChange_管理者以外のセッションには付けない(t *testing.T) {
	s, _, _ := newService(t)
	_ = s.SetInitialPassword("admin")
	other, err := s.Begin("alice", "Alice", []string{"developer"})
	if err != nil {
		t.Fatal(err)
	}
	if other.MustChange {
		t.Error("only the admin session is asked to change the password")
	}
}

func TestVerifyFrom_特定IPの5回失敗は別IPの正規ログインをブロックしない(t *testing.T) {
	s, _, _ := newService(t)
	for i := 0; i < 5; i++ {
		_ = s.VerifyFrom("203.0.113.9", "admin", "wrong")
	}
	var rl *auth.RateLimitedError
	if err := s.VerifyFrom("203.0.113.9", "admin", "correct horse battery"); !errors.As(err, &rl) {
		t.Errorf("same client: %v, want RateLimitedError", err)
	}
	if err := s.VerifyFrom("198.51.100.1", "admin", "correct horse battery"); err != nil {
		t.Errorf("other client: %v", err)
	}
}

func TestVerifyFrom_失敗の記録は上限を超えて増えない(t *testing.T) {
	s, c, _ := newService(t)
	for i := 0; i < auth.MaxTrackedClients*2; i++ {
		_ = s.VerifyFrom(fmt.Sprintf("10.%d.%d.1", i/256, i%256), "admin", "wrong")
		c.now = c.now.Add(time.Second)
	}
	if n := s.TrackedClients(); n > auth.MaxTrackedClients {
		t.Errorf("tracked = %d", n)
	}
}

type countingHasher struct {
	sha
	calls atomic.Int32
}

func (h *countingHasher) Hash(p string, salt []byte) []byte {
	h.calls.Add(1)
	return h.sha.Hash(p, salt)
}

func TestVerify_極端に長いパスワードはハッシュせず拒否する(t *testing.T) {
	s, _, _ := newService(t)
	h := &countingHasher{}
	s.Hasher = h
	if err := s.Verify("admin", strings.Repeat("a", auth.MaxPasswordLen+1)); !errors.Is(err, auth.ErrInvalidCredentials) || h.calls.Load() != 0 {
		t.Errorf("err %v hashes %d", err, h.calls.Load())
	}
	s.Hasher = sha{}
	sess, _ := s.Login("admin", "correct horse battery")
	if _, err := s.ChangePassword(sess.Token, "correct horse battery", strings.Repeat("a", auth.MaxPasswordLen+1)); !errors.Is(err, auth.ErrWeakPassword) {
		t.Error("too long new password accepted")
	}
}

type gate struct {
	sha
	in      chan struct{}
	release chan struct{}
}

func (g *gate) Hash(p string, salt []byte) []byte {
	g.in <- struct{}{}
	<-g.release
	return g.sha.Hash(p, salt)
}

func TestVerify_Argon2計算中もSession検証がブロックされない(t *testing.T) {
	s, _, _ := newService(t)
	sess, err := s.Login("admin", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	g := &gate{in: make(chan struct{}, 16), release: make(chan struct{})}
	s.Hasher = g
	done := make(chan struct{})
	go func() { _ = s.VerifyFrom("a", "admin", "wrong"); close(done) }()
	<-g.in
	ok := make(chan error, 1)
	go func() { _, err := s.Session(sess.Token); ok <- err }()
	select {
	case err := <-ok:
		if err != nil {
			t.Errorf("session: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Session blocked while a password hash was running")
	}
	close(g.release)
	<-done
}

func TestVerify_同時ハッシュ数は上限があり超えた分は待たされる(t *testing.T) {
	s, _, _ := newService(t)
	g := &gate{in: make(chan struct{}, 64), release: make(chan struct{})}
	s.Hasher = g
	var wg sync.WaitGroup
	for i := 0; i < auth.MaxHashing+3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = s.VerifyFrom(fmt.Sprint(i), "admin", "x") }()
	}
	for i := 0; i < auth.MaxHashing; i++ {
		<-g.in
	}
	time.Sleep(50 * time.Millisecond)
	if len(g.in) != 0 {
		t.Errorf("%d hashes beyond the cap running", len(g.in))
	}
	close(g.release)
	wg.Wait()
}

func TestClient_コンテキストで呼び出し元を運ぶ(t *testing.T) {
	if got := auth.ClientFrom(auth.WithClient(context.Background(), "203.0.113.9")); got != "203.0.113.9" {
		t.Errorf("client = %q", got)
	}
	if got := auth.ClientFrom(context.Background()); got != "" {
		t.Errorf("client = %q, want empty", got)
	}
}

func TestVerifyFrom_全員が新しい失敗でも記録は上限を超えず古い方から捨てる(t *testing.T) {
	s, _, _ := newService(t)
	for i := 0; i < auth.MaxTrackedClients+10; i++ {
		_ = s.VerifyFrom(fmt.Sprintf("c%d", i), "admin", "wrong")
	}
	if n := s.TrackedClients(); n > auth.MaxTrackedClients {
		t.Errorf("tracked = %d", n)
	}
}

func TestVerifyFrom_同一IPの並行試行でも5回を超えてハッシュしない(t *testing.T) {
	s, _, _ := newService(t)
	h := &countingHasher{}
	s.Hasher = h
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = s.VerifyFrom("203.0.113.9", "admin", "wrong") }()
	}
	wg.Wait()
	if n := h.calls.Load(); n != 5 {
		t.Errorf("hashes = %d, want 5", n)
	}
}

func TestVerifyFrom_記録が満杯でもロック中のクライアントは残る(t *testing.T) {
	s, _, _ := newService(t)
	for i := 0; i < 6; i++ {
		_ = s.VerifyFrom("attacker", "admin", "wrong")
	}
	for i := 0; i < auth.MaxTrackedClients*2; i++ {
		_ = s.VerifyFrom(fmt.Sprintf("c%d", i), "admin", "wrong")
	}
	var rl *auth.RateLimitedError
	if err := s.VerifyFrom("attacker", "admin", "correct horse battery"); !errors.As(err, &rl) {
		t.Errorf("lock lost: %v", err)
	}
}

func TestVerifyFrom_全記録がロック中なら新規クライアントは共有枠にまとめ記録を増やさない(t *testing.T) {
	s, _, _ := newService(t)
	for i := 0; i < auth.MaxTrackedClients+50; i++ {
		for j := 0; j < 6; j++ {
			_ = s.VerifyFrom(fmt.Sprintf("c%d", i), "admin", "wrong")
		}
	}
	if n := s.TrackedClients(); n > auth.MaxTrackedClients+1 {
		t.Errorf("tracked = %d", n)
	}
}

// memRevocations stands in for the cluster-wide (or on-disk) revocation list.
type memRevocations struct {
	ids     map[string]time.Time
	failErr error
}

func (m *memRevocations) Revoke(id string, expires, _ time.Time) error {
	if m.failErr != nil {
		return m.failErr
	}
	if m.ids == nil {
		m.ids = map[string]time.Time{}
	}
	m.ids[id] = expires
	return nil
}

func (m *memRevocations) Revoked(id string) bool { _, ok := m.ids[id]; return ok }

func TestLogout_失効を共有の記録に残し別のノードでも使えなくする(t *testing.T) {
	shared := &memRevocations{}
	a, _, st := newService(t)
	a.Revocations = shared
	b := &auth.Service{Store: st, Hasher: sha{}, Clock: a.Clock, Revocations: shared}
	sess, _ := a.Login("admin", "correct horse battery")
	if _, err := b.Session(sess.Token); err != nil {
		t.Fatalf("before logout: %v", err)
	}
	if err := a.Logout(sess.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Session(sess.Token); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("other node after logout: %v", err)
	}
}

func TestLogout_共有の記録に書けなければセッションを残して再試行でき_クラスタ更新中はこのノードだけ失効させる(t *testing.T) {
	s, _, _ := newService(t)
	boom := errors.New("no leader")
	s.Revocations = &memRevocations{failErr: boom}
	sess, _ := s.Login("admin", "correct horse battery")
	if err := s.Logout(sess.Token); !errors.Is(err, boom) {
		t.Errorf("Logout error = %v, want the failure reported", err)
	}
	if _, err := s.Session(sess.Token); err != nil {
		t.Errorf("session after failed shared revoke: %v, want it left open for a retry", err)
	}
	rev := &memRevocations{}
	s.Revocations = rev
	if err := s.Logout(sess.Token); err != nil || !rev.Revoked(sessID(t, s, sess.Token)) {
		t.Errorf("retry: %v", err)
	}
	// A node below protocol 24: the logout holds here only.
	s.Revocations = &memRevocations{failErr: domain.ErrClusterUpgrading}
	other, _ := s.Login("admin", "correct horse battery")
	if err := s.Logout(other.Token); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Errorf("upgrading: %v", err)
	}
	if _, err := s.Session(other.Token); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("after upgrading logout: %v", err)
	}
}

// sessID finds the id the service recorded for token (the only one in a memRevocations).
func sessID(t *testing.T, s *auth.Service, _ string) string {
	t.Helper()
	for id := range s.Revocations.(*memRevocations).ids {
		return id
	}
	t.Fatal("nothing recorded")
	return ""
}

func TestLogout_失効の記録は更新済みトークンより長く残す(t *testing.T) {
	s, c, _ := newService(t)
	rev := &memRevocations{}
	s.Revocations = rev
	sess, _ := s.Login("admin", "correct horse battery")
	c.now = c.now.Add(auth.DefaultSessionTTL * 3 / 4) // past half: the next call would renew
	// Logging out with the old token near its end must outlast a copy renewed just now.
	if err := s.Logout(sess.Token); err != nil {
		t.Fatal(err)
	}
	for _, exp := range rev.ids {
		if exp.Before(c.now.Add(auth.DefaultSessionTTL)) {
			t.Errorf("record expires %v, before a token renewed now (%v)", exp, c.now.Add(auth.DefaultSessionTTL))
		}
	}
}

func TestLogout_更新完了時に保留していたクラスタ失効を流せる(t *testing.T) {
	rev := &memRevocations{failErr: domain.ErrClusterUpgrading}
	a, c, st := newService(t)
	a.Revocations = rev
	b := &auth.Service{Store: st, Hasher: sha{}, Clock: a.Clock, Revocations: rev}

	sess, _ := a.Login("admin", "correct horse battery")
	if err := a.Logout(sess.Token); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Fatalf("Logout during upgrade = %v, want ErrClusterUpgrading", err)
	}
	if _, err := a.Session(sess.Token); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("nodeA after logout = %v, want ErrNoSession", err)
	}
	if _, err := b.Session(sess.Token); err != nil {
		t.Fatalf("nodeB before upgrade completes = %v, want valid", err)
	}

	// Still upgrading: FlushRevocations keeps the pending entry.
	if err := a.FlushRevocations(); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Fatalf("FlushRevocations while upgrading = %v, want ErrClusterUpgrading", err)
	}

	// Upgrade finishes: FlushRevocations writes it to shared Revocations, so nodeB rejects it too.
	rev.failErr = nil
	if err := a.FlushRevocations(); err != nil {
		t.Fatalf("FlushRevocations after upgrade = %v", err)
	}
	id1 := sessID(t, a, sess.Token)
	if _, err := b.Session(sess.Token); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("nodeB after flush = %v, want ErrNoSession", err)
	}

	// Expired pending entries are pruned instead of written.
	sess2, _ := a.Login("admin", "correct horse battery")
	rev.failErr = domain.ErrClusterUpgrading
	if err := a.Logout(sess2.Token); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Fatalf("Logout(sess2) during upgrade = %v, want ErrClusterUpgrading", err)
	}
	c.now = c.now.Add(2 * auth.DefaultSessionTTL)
	rev.failErr = nil
	if err := a.FlushRevocations(); err != nil {
		t.Fatalf("FlushRevocations expired = %v", err)
	}
	if !rev.Revoked(id1) || len(rev.ids) != 1 {
		t.Errorf("expired pending entry should not be flushed, id1=%q ids=%v", id1, rev.ids)
	}

	// PendingSession & direct retry of Logout on the same node after upgrade.
	sess3, _ := a.Login("admin", "correct horse battery")
	if _, err := a.PendingSession(sess3.Token); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("PendingSession before logout = %v, want ErrNoSession", err)
	}
	rev.failErr = domain.ErrClusterUpgrading
	if err := a.Logout(sess3.Token); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Fatalf("Logout(sess3) = %v, want ErrClusterUpgrading", err)
	}
	if p, err := a.PendingSession(sess3.Token); err != nil || p.Renewed {
		t.Fatalf("PendingSession after upgrading logout = (%+v, %v), want valid non-renewed", p, err)
	}
	rev.failErr = nil
	if err := a.Logout(sess3.Token); err != nil {
		t.Fatalf("retry Logout(sess3) after upgrade = %v", err)
	}
	if _, err := a.PendingSession(sess3.Token); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("PendingSession after successful retry = %v, want ErrNoSession", err)
	}

	// Subsequent upgrading Logout prunes expired pendingRevoke entries, and nil Revocations FlushRevocations is a no-op.
	sess4, _ := a.Login("admin", "correct horse battery")
	rev.failErr = domain.ErrClusterUpgrading
	if err := a.Logout(sess4.Token); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Fatalf("Logout(sess4) = %v", err)
	}
	c.now = c.now.Add(2 * auth.DefaultSessionTTL)
	sess5, _ := a.Login("admin", "correct horse battery")
	if err := a.Logout(sess5.Token); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Fatalf("Logout(sess5) = %v", err)
	}
	noRev, _, _ := newService(t)
	if err := noRev.FlushRevocations(); err != nil {
		t.Errorf("FlushRevocations with nil Revocations = %v", err)
	}
}

type stampedStore struct {
	mu       sync.Mutex
	c        auth.Credentials
	ok       bool
	loads    int
	stamps   int
	version  uint64
	fail     error
	loadFail error
	bumpLoad bool
}

func (s *stampedStore) Stamp() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stamps++
	if s.fail != nil {
		return "", s.fail
	}
	if !s.ok {
		return "", auth.ErrNoCredentials
	}
	return fmt.Sprintf("v%d", s.version), nil
}

func (s *stampedStore) Load() (auth.Credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	if s.loadFail != nil {
		return auth.Credentials{}, s.loadFail
	}
	if s.fail != nil {
		return auth.Credentials{}, s.fail
	}
	if !s.ok {
		return auth.Credentials{}, auth.ErrNoCredentials
	}
	if s.bumpLoad {
		s.bumpLoad = false
		s.version++
	}
	return s.c, nil
}

func (s *stampedStore) Save(c auth.Credentials) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.c = c
	s.ok = true
	s.version++
	return nil
}

func TestSession_StampStoreキャッシュは毎回のLoadを省きつつ外部変更で即時失効する(t *testing.T) {
	clk := &clock{now: time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)}
	st := &stampedStore{}
	s := &auth.Service{Store: st, Clock: clk.Now, Hasher: sha{}}
	if err := s.SetPassword("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	sess, err := s.Login("admin", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	// Login warmed the cache on s (1 Load), so subsequent Session calls incur zero extra Loads.
	loadsBefore := st.loads
	for i := 0; i < 5; i++ {
		if _, err := s.Session(sess.Token); err != nil {
			t.Fatalf("Session on s %d: %v", i, err)
		}
	}
	if st.loads != loadsBefore {
		t.Fatalf("expected 0 Loads on s after Login warmed cache, got %d", st.loads-loadsBefore)
	}

	// A fresh Service instance warms its cache with exactly 1 Load across 10 Session calls.
	restarted := &auth.Service{Store: st, Clock: clk.Now, Hasher: sha{}}
	loadsBeforeRestarted := st.loads
	for i := 0; i < 10; i++ {
		if _, err := restarted.Session(sess.Token); err != nil {
			t.Fatalf("Session on restarted %d: %v", i, err)
		}
	}
	if got := st.loads - loadsBeforeRestarted; got != 1 {
		t.Fatalf("expected 1 Load to warm cache across 10 Session calls, got %d", got)
	}
	if st.stamps < 10 {
		t.Fatalf("expected Stamp called on each Session check, got %d", st.stamps)
	}

	// External password reset bumps stamp -> Session immediately fails without waiting for any TTL.
	other := &auth.Service{Store: st, Clock: clk.Now, Hasher: sha{}}
	if err := other.SetPassword("reset from cli immediately"); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Session(sess.Token); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("expected immediate revocation after external reset, got %v", err)
	}

	// Stamp race during Load does not poison cache; Load error clears cache and propagates.
	sess2, _ := restarted.Login("admin", "reset from cli immediately")
	fresh := &auth.Service{Store: st, Clock: clk.Now, Hasher: sha{}}
	st.bumpLoad = true
	if _, err := fresh.Session(sess2.Token); err != nil {
		t.Fatalf("Session during concurrent stamp bump: %v", err)
	}
	// Next call still warms cache cleanly because the raced load was not cached.
	if _, err := fresh.Session(sess2.Token); err != nil {
		t.Fatalf("Session after concurrent stamp bump: %v", err)
	}
	st.loadFail = errors.New("corrupt json")
	st.version++
	if _, err := fresh.Session(sess2.Token); err == nil {
		t.Fatal("expected error when Load fails after stamp change")
	}
	st.loadFail = nil

	// Store failure surfaces immediately rather than returning stale cache.
	_, _ = restarted.Session(sess2.Token)
	st.fail = errors.New("disk unreadable")
	if _, err := restarted.Session(sess2.Token); err == nil {
		t.Fatal("expected error when Stamp fails")
	}
	st.fail = nil
	st.ok = false
	if _, err := restarted.Session(sess2.Token); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("expected ErrNoSession when credentials disappear, got %v", err)
	}
}
