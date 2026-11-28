package auth_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/auth"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time        { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

type testHasher struct{}

func (testHasher) Hash(pw string, salt []byte) []byte {
	h := sha256.Sum256(append(append([]byte{}, salt...), pw...))
	return h[:]
}

type credMemStore struct {
	mu   sync.Mutex
	c    auth.Credentials
	have bool
	fail error
}

func (s *credMemStore) Load() (auth.Credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return auth.Credentials{}, s.fail
	}
	if !s.have {
		return auth.Credentials{}, auth.ErrNoCredentials
	}
	return s.c, nil
}

func (s *credMemStore) Save(c auth.Credentials) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.c = c
	s.have = true
	return nil
}

type memUserStore struct {
	mu       sync.Mutex
	users       map[string]domain.User
	protocol    int
	fail        error
	putFail     error
	insertRace  bool
}

func newMemUserStore() *memUserStore {
	return &memUserStore{users: map[string]domain.User{}, protocol: domain.ProtocolUsers}
}

func (s *memUserStore) ListUsers(context.Context) ([]domain.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return nil, s.fail
	}
	out := make([]domain.User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, u.Clone())
	}
	slices.SortFunc(out, func(a, b domain.User) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return out, nil
}

func (s *memUserStore) GetUser(_ context.Context, id string) (domain.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return domain.User{}, s.fail
	}
	u, ok := s.users[id]
	if !ok {
		return domain.User{}, domain.ErrNotFound
	}
	return u.Clone(), nil
}

func (s *memUserStore) InsertUser(_ context.Context, u domain.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	if s.protocol < domain.ProtocolUsers {
		return domain.ErrClusterUpgrading
	}
	if s.insertRace {
		s.insertRace = false
		s.users[u.ID] = domain.User{ID: u.ID, Salt: []byte("old"), Hash: []byte("old"), CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt}
		return domain.ErrAlreadyExists
	}
	if _, exists := s.users[u.ID]; exists {
		return domain.ErrAlreadyExists
	}
	s.users[u.ID] = u.Clone()
	return nil
}

func (s *memUserStore) PutUser(_ context.Context, u domain.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.putFail != nil {
		return s.putFail
	}
	if s.fail != nil {
		return s.fail
	}
	if s.protocol < domain.ProtocolUsers {
		return domain.ErrClusterUpgrading
	}
	if _, exists := s.users[u.ID]; !exists {
		return domain.ErrNotFound
	}
	if err := domain.EnsureActivePasswordUserAfterUpdate(s.users, u, false); err != nil {
		return err
	}
	s.users[u.ID] = u.Clone()
	return nil
}

func (s *memUserStore) DeleteUser(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	if s.protocol < domain.ProtocolUsers {
		return domain.ErrClusterUpgrading
	}
	if _, exists := s.users[id]; !exists {
		return domain.ErrNotFound
	}
	if err := domain.EnsureActivePasswordUserAfterDelete(s.users, id, false); err != nil {
		return err
	}
	delete(s.users, id)
	return nil
}

func newMultiUserService(t *testing.T) (*auth.Service, *fakeClock, *credMemStore, *memUserStore) {
	t.Helper()
	clk := &fakeClock{now: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}
	cs := &credMemStore{}
	us := newMemUserStore()
	svc := &auth.Service{
		Store:    cs,
		Users:    us,
		Hasher:   testHasher{},
		Clock:    clk.Now,
		Protocol: func() int { return us.protocol },
	}
	if err := svc.SetPassword("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	return svc, clk, cs, us
}

func TestMultiUser_VerifyNormalizeDisableEmptyAndRateLimit(t *testing.T) {
	svc, clk, _, us := newMultiUserService(t)
	ctx := context.Background()

	alice, err := svc.CreateUser(ctx, auth.CreateUserInput{
		Email:      "Alice@Example.COM",
		Name:       "Alice",
		Password:   "alice secret pass",
		MustChange: true,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if alice.ID != "user:alice@example.com" || alice.Email != "alice@example.com" {
		t.Fatalf("unexpected alice: %+v", alice)
	}

	got, err := svc.VerifyUserFrom("10.0.0.1", "alice@example.com", "alice secret pass")
	if err != nil || got.ID != alice.ID || !got.MustChange {
		t.Fatalf("VerifyUserFrom: got=%+v err=%v", got, err)
	}

	// Rate limit applies cleanly per client when user logins fail repeatedly.
	for i := 0; i < 5; i++ {
		if _, err := svc.VerifyUserFrom("10.0.0.2", "alice@example.com", "wrong"); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("attempt %d: want ErrInvalidCredentials, got %v", i, err)
		}
	}
	var rl *auth.RateLimitedError
	if _, err := svc.VerifyUserFrom("10.0.0.2", "alice@example.com", "alice secret pass"); !errors.As(err, &rl) {
		t.Fatalf("want RateLimitedError, got %v", err)
	}
	clk.Advance(2 * time.Minute)
	if _, err := svc.VerifyUserFrom("10.0.0.2", "alice@example.com", "alice secret pass"); err != nil {
		t.Fatalf("after lockout expiry: %v", err)
	}

	// SSO-only user with empty password cannot authenticate with password.
	if _, err := svc.CreateUser(ctx, auth.CreateUserInput{
		Email:         "sso@example.com",
		Name:          "SSO",
		EmptyPassword: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.VerifyUserFrom("10.0.0.3", "sso@example.com", ""); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("empty password hash should reject: %v", err)
	}

	// Disabled user cannot verify or start a session.
	disabled := true
	if _, err := svc.UpdateUser(ctx, "user:alice@example.com", auth.UpdateUserInput{Disabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.VerifyUserFrom("10.0.0.3", "alice@example.com", "alice secret pass"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("disabled user should fail verify: %v", err)
	}
	if _, err := svc.Begin(alice.ID, alice.Name, []string{"admin"}); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("disabled user should fail Begin: %v", err)
	}
	_ = us
}

func TestMultiUser_IsolatedSessionGenAndMustChange(t *testing.T) {
	svc, clk, cs, _ := newMultiUserService(t)
	ctx := context.Background()

	alice, err := svc.CreateUser(ctx, auth.CreateUserInput{
		Email:      "alice@example.com",
		Name:       "Alice",
		Password:   "alice initial secret",
		MustChange: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := svc.CreateUser(ctx, auth.CreateUserInput{
		Email:    "bob@example.com",
		Name:     "Bob",
		Password: "bob secret passphrase",
	})
	if err != nil {
		t.Fatal(err)
	}

	aliceSess, err := svc.Begin(alice.ID, alice.Name, []string{"admin"})
	if err != nil || !aliceSess.MustChange {
		t.Fatalf("alice Begin: sess=%+v err=%v", aliceSess, err)
	}
	aliceOther, err := svc.Begin(alice.ID, alice.Name, []string{"admin"})
	if err != nil {
		t.Fatal(err)
	}
	bobSess, err := svc.Begin(bob.ID, bob.Name, []string{"admin"})
	if err != nil || bobSess.MustChange {
		t.Fatalf("bob Begin: sess=%+v err=%v", bobSess, err)
	}
	adminSess, err := svc.Begin(auth.AdminUser, auth.AdminUser, []string{"admin"})
	if err != nil {
		t.Fatal(err)
	}

	// Alice changes her own password: clears her MustChange, keeps her current session (new token),
	// invalidates Alice's other session, and leaves Bob and Admin sessions valid.
	credBefore, _ := cs.Load()
	fresh, err := svc.ChangePassword(aliceSess.Token, "alice initial secret", "alice brand new secret")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.MustChange || fresh.CSRF != aliceSess.CSRF {
		t.Fatalf("fresh session mismatch: %+v", fresh)
	}
	if _, err := svc.Session(aliceOther.Token); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("alice's other session must be invalidated, got %v", err)
	}
	if _, err := svc.Session(aliceSess.Token); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("alice's old session must be invalidated, got %v", err)
	}
	if _, err := svc.Session(fresh.Token); err != nil {
		t.Fatalf("alice's fresh session must stay valid: %v", err)
	}
	if _, err := svc.Session(bobSess.Token); err != nil {
		t.Fatalf("bob's session must stay valid: %v", err)
	}
	if _, err := svc.Session(adminSess.Token); err != nil {
		t.Fatalf("admin's session must stay valid: %v", err)
	}
	credAfter, _ := cs.Load()
	if credAfter.Generation != credBefore.Generation {
		t.Fatalf("global Credentials.Generation must not increment in multi-user mode: before=%d after=%d", credBefore.Generation, credAfter.Generation)
	}

	// Renewing Bob's session past half TTL keeps Bob logged in.
	clk.Advance(4 * 24 * time.Hour)
	renewed, err := svc.Session(bobSess.Token)
	if err != nil || !renewed.Renewed {
		t.Fatalf("bob renew: sess=%+v err=%v", renewed, err)
	}

	// Disabling Bob invalidates both old and renewed sessions immediately.
	disabled := true
	if _, err := svc.UpdateUser(ctx, bob.ID, auth.UpdateUserInput{Disabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Session(renewed.Token); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("disabled bob must lose session, got %v", err)
	}

	// Deleting Alice invalidates Alice's session immediately.
	if err := svc.DeleteUser(ctx, alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Session(fresh.Token); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("deleted alice must lose session, got %v", err)
	}
}

func TestMultiUser_LegacyAdminCookieAndProtocol30Guard(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}
	cs := &credMemStore{}
	legacySvc := &auth.Service{Store: cs, Hasher: testHasher{}, Clock: clk.Now}
	if err := legacySvc.SetInitialPassword("legacy initial password"); err != nil {
		t.Fatal(err)
	}
	legacyCookie, err := legacySvc.Begin(auth.AdminUser, auth.AdminUser, []string{"admin"})
	if err != nil || !legacyCookie.MustChange {
		t.Fatal(err)
	}

	// Upgrade store to multi-user: admin migrated with matching SessionGen == c.Generation.
	us := newMemUserStore()
	c, _ := cs.Load()
	_ = us.InsertUser(context.Background(), domain.User{
		ID: domain.AdminUserID, Name: domain.AdminUserID,
		Salt: c.Salt, Hash: c.Hash, MustChange: true, SessionGen: c.Generation,
		CreatedAt: clk.Now(), UpdatedAt: clk.Now(),
	})
	svc := &auth.Service{Store: cs, Users: us, Hasher: testHasher{}, Clock: clk.Now, Protocol: func() int { return us.protocol }}

	// Legacy admin cookie (p.UGen == nil) remains valid while u.SessionGen <= c.Generation.
	sess, err := svc.Session(legacyCookie.Token)
	if err != nil || !sess.MustChange {
		t.Fatalf("legacy admin cookie should work before reset: sess=%+v err=%v", sess, err)
	}

	// While cluster protocol < 30, SetPassword on migrated State.Users["admin"] returns ErrClusterUpgrading
	// without advancing Credentials.Generation.
	us.protocol = domain.ProtocolUsers - 1
	genBefore := c.Generation
	if err := svc.SetPassword("should not advance admin"); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Fatalf("want ErrClusterUpgrading, got %v", err)
	}
	cAfterFail, _ := cs.Load()
	if cAfterFail.Generation != genBefore {
		t.Fatalf("Credentials.Generation changed despite ErrClusterUpgrading: %d -> %d", genBefore, cAfterFail.Generation)
	}

	// Once protocol reaches 30, SetPassword bumps u.SessionGen > c.Generation so legacy cookie is rejected.
	us.protocol = domain.ProtocolUsers
	if err := svc.SetPassword("new recovered admin pw"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Session(legacyCookie.Token); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("legacy admin cookie must be invalidated after SetPassword, got %v", err)
	}
	if _, err := svc.Login("admin", "new recovered admin pw"); err != nil {
		t.Fatalf("login with new admin password: %v", err)
	}
}

func TestMultiUser_LastActiveUserAndCRUDValidation(t *testing.T) {
	svc, _, _, _ := newMultiUserService(t)
	ctx := context.Background()

	// Only admin is active with password: disabling or deleting admin fails with ErrConflict.
	disabled := true
	if _, err := svc.UpdateUser(ctx, "admin", auth.UpdateUserInput{Disabled: &disabled}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("disable last user: want ErrConflict, got %v", err)
	}
	if err := svc.DeleteUser(ctx, "admin"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("delete last user: want ErrConflict, got %v", err)
	}

	// Weak password / invalid email / ResetUserPassword.
	if _, err := svc.CreateUser(ctx, auth.CreateUserInput{Email: "bad-email", Password: "long enough pw"}); !errors.Is(err, domain.ErrInvalidName) {
		t.Fatalf("invalid email: %v", err)
	}
	if _, err := svc.CreateUser(ctx, auth.CreateUserInput{Email: "ok@example.com", Password: "short"}); !errors.Is(err, auth.ErrWeakPassword) {
		t.Fatalf("weak password: %v", err)
	}
	alice, err := svc.CreateUser(ctx, auth.CreateUserInput{Email: "alice@example.com", Password: "first long pass"})
	if err != nil {
		t.Fatal(err)
	}
	_, pw, err := svc.ResetUserPassword(ctx, "alice@example.com", true)
	if err != nil || len(pw) != 24 {
		t.Fatalf("ResetUserPassword: pw=%q err=%v", pw, err)
	}
	got, err := svc.GetUser(ctx, "USER:ALICE@EXAMPLE.COM")
	if err == nil {
		t.Fatalf("expected invalid email/id error or check exact ResolveUserID for uppercase prefix: %+v", got)
	}
	got, err = svc.GetUser(ctx, "alice@example.com")
	if err != nil || !got.MustChange || got.ID != alice.ID {
		t.Fatalf("GetUser: got=%+v err=%v", got, err)
	}
	if _, err := svc.VerifyUserFrom("", "alice@example.com", pw); err != nil {
		t.Fatalf("verify reset password: %v", err)
	}
	users, err := svc.ListUsers(ctx)
	if err != nil || len(users) != 2 {
		t.Fatalf("ListUsers: len=%d err=%v", len(users), err)
	}
	// Now we can disable admin because alice is active with a password; SetPassword re-enables admin.
	if _, err := svc.UpdateUser(ctx, "admin", auth.UpdateUserInput{Disabled: &disabled}); err != nil {
		t.Fatalf("disable admin with alice present: %v", err)
	}
	if err := svc.SetPassword("re-enabled admin password"); err != nil {
		t.Fatalf("SetPassword should re-enable disabled admin: %v", err)
	}
	adm, _ := svc.GetUser(ctx, "admin")
	if adm.Disabled {
		t.Fatal("admin should be re-enabled after SetPassword")
	}
}

func TestMultiUser_UpdateClearPasswordNameAndErrors(t *testing.T) {
	svc, _, cs, us := newMultiUserService(t)
	ctx := context.Background()

	// CreateUser with empty Name falls back to email.
	bob, err := svc.CreateUser(ctx, auth.CreateUserInput{
		Email:    "bob@example.com",
		Password: "bob secret passphrase",
	})
	if err != nil || bob.Name != "bob@example.com" {
		t.Fatalf("CreateUser default name: %+v err=%v", bob, err)
	}

	// UpdateUser: Name, ClearPassword, weak Password, and missing/invalid targets.
	newName := "  Bob Updated  "
	weak := "short"
	if _, err := svc.UpdateUser(ctx, "bad-email", auth.UpdateUserInput{Name: &newName}); !errors.Is(err, domain.ErrInvalidName) {
		t.Fatalf("invalid id UpdateUser: %v", err)
	}
	if _, err := svc.UpdateUser(ctx, "bob@example.com", auth.UpdateUserInput{Password: &weak}); !errors.Is(err, auth.ErrWeakPassword) {
		t.Fatalf("weak Password UpdateUser: %v", err)
	}
	updated, err := svc.UpdateUser(ctx, "bob@example.com", auth.UpdateUserInput{
		Name:          &newName,
		ClearPassword: true,
	})
	if err != nil || updated.Name != "Bob Updated" || len(updated.Hash) != 0 {
		t.Fatalf("ClearPassword/Name: %+v err=%v", updated, err)
	}
	if _, err := svc.VerifyUserFrom("", "bob@example.com", "bob secret passphrase"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("cleared password must fail verify: %v", err)
	}

	// Missing user and protocol < 30 checks.
	if _, _, err := svc.ResetUserPassword(ctx, "missing@example.com", true); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("ResetUserPassword missing: %v", err)
	}
	if err := svc.DeleteUser(ctx, "bad-email"); !errors.Is(err, domain.ErrInvalidName) {
		t.Fatalf("DeleteUser bad id: %v", err)
	}
	us.protocol = domain.ProtocolUsers - 1
	if _, err := svc.UpdateUser(ctx, "bob@example.com", auth.UpdateUserInput{Name: &newName}); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Fatalf("UpdateUser upgrading: %v", err)
	}
	if err := svc.DeleteUser(ctx, "bob@example.com"); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Fatalf("DeleteUser upgrading: %v", err)
	}
	us.protocol = domain.ProtocolUsers

	// ChangePassword validation paths with Users != nil.
	admSess, err := svc.Login("admin", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ChangePassword(admSess.Token, "wrong", "new valid long pass"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("ChangePassword wrong current: %v", err)
	}
	if _, err := svc.ChangePassword(admSess.Token, "correct horse battery", "short"); !errors.Is(err, auth.ErrWeakPassword) {
		t.Fatalf("ChangePassword short next: %v", err)
	}
	us.protocol = domain.ProtocolUsers - 1
	if _, err := svc.ChangePassword(admSess.Token, "correct horse battery", "new valid long pass"); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Fatalf("ChangePassword upgrading: %v", err)
	}
	us.protocol = domain.ProtocolUsers

	// Empty Name fallback on Begin + external (non-store) user session validation.
	us.mu.Lock()
	u := us.users[bob.ID]
	u.Name = ""
	us.users[bob.ID] = u
	us.mu.Unlock()
	bobSess, err := svc.Begin(bob.ID, "", []string{"admin"})
	if err != nil || bobSess.Name != "bob@example.com" {
		t.Fatalf("Begin DisplayName fallback: %+v err=%v", bobSess, err)
	}
	extSess, err := svc.Begin("user:ext@example.com", "Ext", []string{"admin"})
	if err != nil || extSess.MustChange {
		t.Fatalf("Begin external user: %+v err=%v", extSess, err)
	}
	if _, err := svc.Session(extSess.Token); err != nil {
		t.Fatalf("Session external user: %v", err)
	}
	if _, err := svc.ChangePassword(extSess.Token, "anything", "new valid long pass"); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("ChangePassword external user: %v", err)
	}

	// Store error propagation paths.
	us.fail = errors.New("disk-u")
	if _, err := svc.VerifyUserFrom("", "bob@example.com", "pw"); err == nil || errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("expected user store error, got %v", err)
	}
	if _, err := svc.Begin(bob.ID, "", nil); err == nil {
		t.Fatal("Begin user store error expected")
	}
	if _, err := svc.Session(bobSess.Token); err == nil {
		t.Fatal("Session user store error expected")
	}
	if _, err := svc.Session(extSess.Token); err == nil {
		t.Fatal("Session ext user store error expected")
	}
	if _, err := svc.ChangePassword(admSess.Token, "correct horse battery", "new valid long pass"); err == nil {
		t.Fatal("ChangePassword user store error expected")
	}
	if err := svc.SetPassword("another admin password"); err == nil {
		t.Fatal("SetPassword user store error expected")
	}
	us.fail = nil

	cs.fail = errors.New("disk-c")
	if _, err := svc.VerifyUserFrom("", "admin", "pw"); err == nil {
		t.Fatal("VerifyUserFrom cred store error expected")
	}
	cs.fail = nil
}

func TestMultiUser_LegacyPreMigrationPathsAndEmptySessionKey(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}
	cs := &credMemStore{}
	us := newMemUserStore()
	svc := &auth.Service{Store: cs, Users: us, Hasher: testHasher{}, Clock: clk.Now, Protocol: func() int { return us.protocol }}

	// Seed legacy creds without SessionKey & without Users["admin"], while protocol < 30.
	hash := testHasher{}.Hash("legacy admin pw", []byte("0123456789abcdef"))
	_ = cs.Save(auth.Credentials{Salt: []byte("0123456789abcdef"), Hash: hash, Generation: 2, MustChange: true})
	us.protocol = domain.ProtocolUsers - 1

	// Missing Users["admin"] falls back to legacy cs on VerifyUserFrom, Begin, ChangePassword, and SetPassword.
	if _, err := svc.VerifyUserFrom("", "bad name!", "legacy admin pw"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("bad login name: %v", err)
	}
	if _, err := svc.VerifyUserFrom("", "admin", "wrong"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("legacy fallback wrong pw: %v", err)
	}
	if got, err := svc.VerifyUserFrom("", "admin", "legacy admin pw"); err != nil || got.ID != "admin" {
		t.Fatalf("legacy fallback verify: %+v err=%v", got, err)
	}
	sess, err := svc.Begin("admin", "admin", []string{"admin"})
	if err != nil || !sess.MustChange {
		t.Fatalf("legacy Begin: %+v err=%v", sess, err)
	}
	if _, err := svc.Session(sess.Token); err != nil {
		t.Fatalf("legacy Session while upgrading: %v", err)
	}
	fresh, err := svc.ChangePassword(sess.Token, "legacy admin pw", "upgrading admin pw")
	if err != nil {
		t.Fatalf("legacy ChangePassword: %v", err)
	}
	if _, err := svc.Session(sess.Token); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("old legacy gen should fail after ChangePassword: %v", err)
	}
	_ = fresh

	// SetPassword with empty SessionKey at protocol < 30 saves fallback legacy cred and fills key.
	c, _ := cs.Load()
	c.SessionKey = nil
	_ = cs.Save(c)
	if err := svc.SetPassword("legacy reset pw"); err != nil {
		t.Fatalf("SetPassword upgrading fallback: %v", err)
	}
	c, _ = cs.Load()
	if len(c.SessionKey) == 0 {
		t.Fatal("SessionKey should be populated")
	}

	// Once protocol >= 30, missing Users["admin"] means deleted -> Session rejected,
	// and SetPassword creates Users["admin"] with empty SessionKey populated too.
	us.protocol = domain.ProtocolUsers
	sess30, err := svc.Begin("admin", "admin", []string{"admin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Session(sess30.Token); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("deleted/missing admin at protocol 30 must fail Session: %v", err)
	}
	c.SessionKey = nil
	_ = cs.Save(c)
	if err := svc.SetInitialPassword("admin migrated password"); err != nil {
		t.Fatalf("SetInitialPassword migrating admin: %v", err)
	}
	adm, err := svc.GetUser(context.Background(), "admin")
	if err != nil || adm.Disabled || !adm.MustChange {
		t.Fatalf("expected Users[admin] created with MustChange: %+v err=%v", adm, err)
	}
	mirrored, _ := cs.Load()
	if !mirrored.MustChange || string(mirrored.Hash) != string(adm.Hash) {
		t.Fatalf("expected mirrored Credentials Salt/Hash/MustChange to match admin: cred=%+v adm=%+v", mirrored, adm)
	}
	prevGen := mirrored.Generation
	if err := svc.SetPassword("admin updated steady password"); err != nil {
		t.Fatalf("SetPassword updating existing admin: %v", err)
	}
	mirrored, _ = cs.Load()
	adm, _ = svc.GetUser(context.Background(), "admin")
	if mirrored.Generation != prevGen || mirrored.MustChange || string(mirrored.Hash) != string(adm.Hash) {
		t.Fatalf("SetPassword should mirror hash/MustChange without bumping Credentials.Generation: prev=%d cred=%+v", prevGen, mirrored)
	}
	// Deleted admin at protocol >= 30 with another user in Users cannot authenticate via stale Credentials.
	if _, err := svc.CreateUser(context.Background(), auth.CreateUserInput{Email: "keep@example.com", Password: "keep secret pass"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteUser(context.Background(), "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.VerifyUserFrom("", "admin", "admin updated steady password"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("deleted admin must not log in with stale Credentials: %v", err)
	}
	// Reissue preserves session CSRF while picking up updated SessionGen/MustChange.
	sKeep, err := svc.Begin("user:keep@example.com", "Keep", []string{"admin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ResetUserPassword(context.Background(), "user:keep@example.com", true); err != nil {
		t.Fatal(err)
	}
	reissued, err := svc.Reissue(sKeep)
	if err != nil || reissued.CSRF != sKeep.CSRF || !reissued.MustChange {
		t.Fatalf("Reissue failed: %+v err=%v", reissued, err)
	}
	if _, err := svc.Session(reissued.Token); err != nil {
		t.Fatalf("reissued session invalid: %v", err)
	}
	// Error branches in DeleteUser, SetPassword store failure, and UpdateUser missing user.
	if err := svc.DeleteUser(context.Background(), "not an email!"); err == nil {
		t.Fatal("DeleteUser invalid id should fail")
	}
	us.fail = errors.New("store-boom")
	if err := svc.SetPassword("admin pw should fail"); err == nil {
		t.Fatal("SetPassword should propagate user store error")
	}
	if _, err := svc.UpdateUser(context.Background(), "user:keep@example.com", auth.UpdateUserInput{}); err == nil {
		t.Fatal("UpdateUser should propagate user store error")
	}
	us.fail = nil
	svc.RevokeTokens = func(context.Context, string) error { return errors.New("revoke-boom") }
	if _, err := svc.CreateUser(context.Background(), auth.CreateUserInput{Email: "del@example.com", Password: "del strong password"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteUser(context.Background(), "del@example.com"); err == nil {
		t.Fatal("DeleteUser should propagate RevokeTokens error")
	}
	dis := true
	if _, err := svc.UpdateUser(context.Background(), "del@example.com", auth.UpdateUserInput{Disabled: &dis}); err == nil {
		t.Fatal("UpdateUser should propagate RevokeTokens error on disable")
	}
	svc.RevokeTokens = nil
	if err := svc.SetPassword("admin recreate pw!!"); err != nil {
		t.Fatal(err)
	}
	us.putFail = errors.New("put-boom")
	if err := svc.SetPassword("admin update pw fail"); err == nil {
		t.Fatal("SetPassword should propagate PutUser error")
	}
	us.putFail = nil
	// Exercise UpdateUser ClearPassword and DeleteUser clean path.
	if _, err := svc.UpdateUser(context.Background(), "keep@example.com", auth.UpdateUserInput{ClearPassword: true}); err != nil {
		t.Fatalf("ClearPassword failed: %v", err)
	}
	if err := svc.DeleteUser(context.Background(), "keep@example.com"); err != nil {
		t.Fatalf("DeleteUser clean: %v", err)
	}
	if _, err := svc.CreateUser(context.Background(), auth.CreateUserInput{Email: "bad", Password: "good long secret pw"}); err == nil {
		t.Fatal("CreateUser invalid email should fail")
	}
	if _, err := svc.CreateUser(context.Background(), auth.CreateUserInput{Email: "weak@example.com", Password: "short"}); !errors.Is(err, auth.ErrWeakPassword) {
		t.Fatalf("CreateUser weak pw = %v, want ErrWeakPassword", err)
	}
	// Simulate concurrent InsertUser ErrAlreadyExists race on admin SetPassword and blank u.Name normalization.
	delete(us.users, "admin")
	us.insertRace = true
	if err := svc.SetPassword("admin race recovery pw"); err != nil {
		t.Fatalf("SetPassword InsertUser ErrAlreadyExists recovery = %v", err)
	}
	if got, err := svc.GetUser(context.Background(), "admin"); err != nil || got.Name != "admin" {
		t.Fatalf("admin Name should normalize cleanly: %+v err=%v", got, err)
	}
}

type phcTestHasher struct{}

func (phcTestHasher) Hash(pw string, salt []byte) []byte {
	return []byte("$argon2id$v=19$m=19456,t=2,p=1$" + string(salt) + "$" + pw)
}

func (phcTestHasher) HashLegacy(pw string, salt []byte) []byte {
	return (testHasher{}).Hash(pw, salt)
}

func (phcTestHasher) Verify(pw string, salt, hash []byte) bool {
	s := string(hash)
	if strings.HasPrefix(s, "$argon2id$") {
		parts := strings.Split(s, "$")
		if len(parts) != 6 {
			return false
		}
		return parts[5] == pw
	}
	return string((testHasher{}).Hash(pw, salt)) == s
}

func (phcTestHasher) NeedsRehash(hash []byte) bool {
	return !strings.HasPrefix(string(hash), "$argon2id$v=19$m=19456,t=2,p=1$")
}

func TestPHC_レガシーハッシュは次回ログイン時にPHCへ再ハッシュされ混在クラスタでは猶予される(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)}
	cs := &credMemStore{}
	us := newMemUserStore()
	us.protocol = domain.ProtocolPHCPassword
	svc := &auth.Service{
		Store:    cs,
		Users:    us,
		Hasher:   phcTestHasher{},
		Clock:    clk.Now,
		Protocol: func() int { return us.protocol },
	}

	// 1. New user and SetPassword write PHC with Salt=nil when Protocol >= 32.
	if err := svc.SetPassword("admin strong password!"); err != nil {
		t.Fatal(err)
	}
	adminU, _ := svc.GetUser(context.Background(), "admin")
	if !strings.HasPrefix(string(adminU.Hash), "$argon2id$v=19$m=19456,t=2,p=1$") || len(adminU.Salt) != 0 {
		t.Fatalf("expected PHC hash and empty salt for admin, got hash=%q salt=%x", adminU.Hash, adminU.Salt)
	}
	if _, err := svc.VerifyUserFrom("", "admin", "admin strong password!"); err != nil {
		t.Fatalf("admin PHC verify failed: %v", err)
	}

	// 2. Legacy raw hash user logs in during rolling upgrade (Protocol 31) -> deferred.
	us.protocol = domain.ProtocolPGMajorUpgrade
	salt := []byte("0123456789abcdef")
	legHash := phcTestHasher{}.HashLegacy("legacy user pass!!", salt)
	if err := us.InsertUser(context.Background(), domain.User{
		ID: "user:leg@example.com", Email: "leg@example.com", Name: "Leg",
		Salt: salt, Hash: legHash, SessionGen: 1, CreatedAt: clk.Now(), UpdatedAt: clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.VerifyUserFrom("", "leg@example.com", "legacy user pass!!"); err != nil {
		t.Fatalf("legacy verify during upgrade failed: %v", err)
	}
	duringU, _ := svc.GetUser(context.Background(), "leg@example.com")
	if strings.HasPrefix(string(duringU.Hash), "$argon2id$") {
		t.Fatalf("rehash must be deferred while cluster protocol < 32, got %q", duringU.Hash)
	}
	// Password update during rolling upgrade also stays legacy so old nodes can verify.
	if _, err := svc.CreateUser(context.Background(), auth.CreateUserInput{Email: "mid@example.com", Password: "mid upgrade pass!!"}); err != nil {
		t.Fatal(err)
	}
	midU, _ := svc.GetUser(context.Background(), "mid@example.com")
	if strings.HasPrefix(string(midU.Hash), "$argon2id$") {
		t.Fatalf("CreateUser during upgrade must keep legacy format, got %q", midU.Hash)
	}

	// 3. Once cluster reaches Protocol 32, next login re-hashes both legacy user and single-node Credentials to PHC.
	us.protocol = domain.ProtocolPHCPassword
	if _, err := svc.VerifyUserFrom("", "leg@example.com", "legacy user pass!!"); err != nil {
		t.Fatalf("legacy verify at protocol 32 failed: %v", err)
	}
	afterU, _ := svc.GetUser(context.Background(), "leg@example.com")
	if !strings.HasPrefix(string(afterU.Hash), "$argon2id$v=19$m=19456,t=2,p=1$") || len(afterU.Salt) != 0 || afterU.SessionGen != 1 {
		t.Fatalf("expected transparent rehash to PHC preserving SessionGen=1, got %+v", afterU)
	}
	// Verify again with the rehashed PHC record.
	if _, err := svc.VerifyUserFrom("", "leg@example.com", "legacy user pass!!"); err != nil {
		t.Fatalf("post-rehash verify failed: %v", err)
	}

	// 4. Outdated-param PHC hash is also rehashed on login.
	oldPHC := []byte("$argon2id$v=19$m=8192,t=1,p=1$somesalt$outdated pass!!")
	if err := us.InsertUser(context.Background(), domain.User{
		ID: "user:oldphc@example.com", Email: "oldphc@example.com", Name: "OldPHC",
		Hash: oldPHC, SessionGen: 2, CreatedAt: clk.Now(), UpdatedAt: clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.VerifyUserFrom("", "oldphc@example.com", "outdated pass!!"); err != nil {
		t.Fatalf("outdated PHC verify failed: %v", err)
	}
	upU, _ := svc.GetUser(context.Background(), "oldphc@example.com")
	if !strings.HasPrefix(string(upU.Hash), "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("outdated PHC was not rehashed: %q", upU.Hash)
	}

	// 5. Single-node (Users == nil) legacy Credentials re-hash on next login preserving Generation.
	singleCred := &credMemStore{}
	_ = singleCred.Save(auth.Credentials{
		Salt: salt, Hash: phcTestHasher{}.HashLegacy("single legacy pw!", salt), Generation: 4, UpdatedAt: clk.Now(), MustChange: true,
	})
	singleSvc := &auth.Service{Store: singleCred, Hasher: phcTestHasher{}, Clock: clk.Now}
	if u, err := singleSvc.VerifyUserFrom("", "admin", "single legacy pw!"); err != nil || !u.MustChange {
		t.Fatalf("single-node legacy login failed: u=%+v err=%v", u, err)
	}
	cAfter, _ := singleCred.Load()
	if !strings.HasPrefix(string(cAfter.Hash), "$argon2id$v=19$m=19456,t=2,p=1$") || len(cAfter.Salt) != 0 || cAfter.Generation != 4 || !cAfter.MustChange {
		t.Fatalf("single-node Credentials not rehashed cleanly: %+v", cAfter)
	}

	// 6. Legacy admin fallback (Users empty, Protocol >= 32) also rehashes Credentials on login.
	fallbackCred := &credMemStore{}
	_ = fallbackCred.Save(auth.Credentials{
		Salt: salt, Hash: phcTestHasher{}.HashLegacy("fallback admin pw!", salt), Generation: 2, UpdatedAt: clk.Now(),
	})
	fallbackUsers := newMemUserStore()
	fallbackUsers.protocol = domain.ProtocolPHCPassword
	fallbackSvc := &auth.Service{
		Store:    fallbackCred,
		Users:    fallbackUsers,
		Hasher:   phcTestHasher{},
		Clock:    clk.Now,
		Protocol: func() int { return fallbackUsers.protocol },
	}
	if _, err := fallbackSvc.VerifyUserFrom("", "admin", "fallback admin pw!"); err != nil {
		t.Fatalf("legacy admin fallback login failed: %v", err)
	}
	fcAfter, _ := fallbackCred.Load()
	if !strings.HasPrefix(string(fcAfter.Hash), "$argon2id$v=19$m=19456,t=2,p=1$") || len(fcAfter.Salt) != 0 {
		t.Fatalf("legacy admin fallback Credentials not rehashed: %+v", fcAfter)
	}
}

type hookPHCHasher struct {
	phcTestHasher
	onHash func()
}

func (h hookPHCHasher) Hash(pw string, salt []byte) []byte {
	if h.onHash != nil {
		h.onHash()
	}
	return h.phcTestHasher.Hash(pw, salt)
}

func TestPHC_ログイン時リハッシュ中の並行パスワード変更を上書きしない(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)}
	cs := &credMemStore{}
	us := newMemUserStore()
	us.protocol = domain.ProtocolPHCPassword
	salt := []byte("0123456789abcdef")
	legHash := phcTestHasher{}.HashLegacy("old-pw-secret!!", salt)
	_ = us.InsertUser(context.Background(), domain.User{
		ID: "user:race@example.com", Email: "race@example.com", Name: "Race",
		Salt: salt, Hash: legHash, SessionGen: 1, CreatedAt: clk.Now(), UpdatedAt: clk.Now(),
	})

	var svc *auth.Service
	newPW := "concurrent-new-pw!"
	triggered := false
	hh := hookPHCHasher{onHash: func() {
		if !triggered {
			triggered = true
			_, _ = svc.UpdateUser(context.Background(), "user:race@example.com", auth.UpdateUserInput{Password: &newPW})
		}
	}}
	svc = &auth.Service{Store: cs, Users: us, Hasher: hh, Clock: clk.Now, Protocol: func() int { return us.protocol }}

	if _, err := svc.VerifyUserFrom("", "race@example.com", "old-pw-secret!!"); err != nil {
		t.Fatalf("initial login failed: %v", err)
	}
	// The concurrent UpdateUser (SessionGen=2, newPW) must NOT be overwritten by the stale rehash of old-pw-secret!!.
	if _, err := svc.VerifyUserFrom("", "race@example.com", newPW); err != nil {
		t.Fatalf("concurrently updated password was lost: %v", err)
	}
	if _, err := svc.VerifyUserFrom("", "race@example.com", "old-pw-secret!!"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("stale password should be rejected, got %v", err)
	}
}

