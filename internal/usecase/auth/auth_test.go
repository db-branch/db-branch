package auth_test

import (
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

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
