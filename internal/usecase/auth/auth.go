// Package auth implements the OSS console login: one "admin" user, password
// change, and a reset path for root on the node.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrNoSession          = errors.New("no valid session")
	ErrNoCredentials      = errors.New("admin password not set")
	ErrWeakPassword       = errors.New("password must be at least 12 characters")
)

// RateLimitedError is returned while logins are locked out after repeated failures.
type RateLimitedError struct{ RetryAfter time.Duration }

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("too many failed logins; retry in %s", e.RetryAfter.Round(time.Second))
}

// AdminUser is the only user in the OSS edition.
const AdminUser = "admin"

// DefaultSessionTTL is how long a console session lasts without use.
const DefaultSessionTTL = 7 * 24 * time.Hour

const (
	maxFailures    = 5
	lockout        = time.Minute
	minPasswordLen = 12
)

// Credentials is the stored admin password. Generation increases on every
// change so sessions created before it stop working on every node.
type Credentials struct {
	Salt       []byte    `json:"salt"`
	Hash       []byte    `json:"hash"`
	Generation int       `json:"generation"`
	UpdatedAt  time.Time `json:"updated_at"`
	// SessionKey signs session tokens. Stored with the password (in a
	// cluster: replicated), so sessions survive agent restarts and work on
	// every node.
	SessionKey []byte `json:"session_key,omitempty"`
	// MustChange is set for a known initial password (`init --evaluation`): the
	// console asks for a new one before anything else. An older node ignores it.
	MustChange bool `json:"must_change,omitempty"`
}

// Store persists Credentials.
type Store interface {
	Load() (Credentials, error)
	Save(Credentials) error
}

// Hasher derives a password hash (argon2id in production).
type Hasher interface {
	Hash(password string, salt []byte) []byte
}

// Session is a logged-in console session.
type Session struct {
	Token     string
	CSRF      string
	User      string
	Name      string
	Roles     []string
	ExpiresAt time.Time
	// Renewed is set when Session issued a fresh Token (same session, later
	// expiry) that the caller should hand back to the client.
	Renewed bool
	// MustChange: the admin signed in with an initial password and has to set a
	// new one first (only the session and password calls work until then).
	MustChange bool
	id         string
}

// Service checks passwords and issues signed session tokens. Nothing about
// sessions is kept in memory but logouts on this node.
type Service struct {
	Store  Store
	Hasher Hasher
	Clock  func() time.Time
	// TTL is how long a session lasts without use (default DefaultSessionTTL).
	TTL func() time.Duration

	mu          sync.Mutex
	revoked     map[string]time.Time // session id -> expiry, after logout
	failures    int
	lockedUntil time.Time
}

func (s *Service) ttl() time.Duration {
	if s.TTL != nil {
		if d := s.TTL(); d > 0 {
			return d
		}
	}
	return DefaultSessionTTL
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// GeneratePassword returns a random initial password.
func GeneratePassword() string { return randomToken()[:24] }

// SetPassword replaces the admin password and ends every session.
func (s *Service) SetPassword(password string) error { return s.setPassword(password, false) }

// SetInitialPassword is SetPassword for a password everyone knows (such as
// "admin" on an evaluation VM): the next sign-in must change it.
func (s *Service) SetInitialPassword(password string) error { return s.setPassword(password, true) }

func (s *Service) setPassword(password string, mustChange bool) error {
	c, err := s.Store.Load()
	if err != nil && !errors.Is(err, ErrNoCredentials) {
		return err
	}
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	key := c.SessionKey
	if len(key) == 0 {
		key = newKey()
	}
	return s.Store.Save(Credentials{
		Salt: salt, Hash: s.Hasher.Hash(password, salt), Generation: c.Generation + 1, UpdatedAt: s.Clock(), SessionKey: key, MustChange: mustChange,
	})
}

func (s *Service) verify(c Credentials, password string) bool {
	return subtle.ConstantTimeCompare(s.Hasher.Hash(password, c.Salt), c.Hash) == 1
}

// Login checks the admin password and starts a session.
func (s *Service) Login(user, password string) (Session, error) {
	if err := s.Verify(user, password); err != nil {
		return Session{}, err
	}
	return s.Begin(user, user, nil)
}

// Verify checks the admin password, locking logins out for a while after
// repeated failures.
func (s *Service) Verify(user, password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Clock()
	if now.Before(s.lockedUntil) {
		return &RateLimitedError{RetryAfter: s.lockedUntil.Sub(now)}
	}
	c, err := s.Store.Load()
	if errors.Is(err, ErrNoCredentials) {
		return ErrInvalidCredentials
	}
	if err != nil {
		return err
	}
	if user != AdminUser || !s.verify(c, password) {
		s.failures++
		if s.failures >= maxFailures {
			s.failures, s.lockedUntil = 0, now.Add(lockout)
		}
		return ErrInvalidCredentials
	}
	s.failures = 0
	return nil
}

// Begin starts a session for someone already authenticated (by Verify or a
// module). Changing the admin password ends it like any other session.
func (s *Service) Begin(user, name string, roles []string) (Session, error) {
	c, err := s.credentialsWithKey()
	if err != nil {
		return Session{}, err
	}
	return s.issue(c, payload{ID: randomToken()[:22], User: user, Name: name, Roles: roles}), nil
}

// credentialsWithKey loads the credentials, adding a session key to ones
// stored before sessions were signed.
func (s *Service) credentialsWithKey() (Credentials, error) {
	c, err := s.Store.Load()
	if errors.Is(err, ErrNoCredentials) {
		return c, ErrNoSession
	}
	if err != nil {
		return c, err
	}
	if len(c.SessionKey) == 0 {
		c.SessionKey = newKey()
		if err := s.Store.Save(c); err != nil {
			return c, err
		}
	}
	return c, nil
}

func newKey() []byte {
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return k
}

// payload is what a session token carries.
type payload struct {
	ID      string   `json:"i"`
	User    string   `json:"u"`
	Name    string   `json:"n,omitempty"`
	Roles   []string `json:"r,omitempty"`
	Gen     int      `json:"g"`
	Expires int64    `json:"e"`
}

func mac(key []byte, parts ...string) []byte {
	h := hmac.New(sha256.New, key)
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return h.Sum(nil)
}

func (s *Service) issue(c Credentials, p payload) Session {
	exp := s.Clock().Add(s.ttl())
	p.Gen, p.Expires = c.Generation, exp.Unix()
	b, _ := json.Marshal(p)
	body := base64.RawURLEncoding.EncodeToString(b)
	token := body + "." + base64.RawURLEncoding.EncodeToString(mac(c.SessionKey, "session", body))
	csrf := base64.RawURLEncoding.EncodeToString(mac(c.SessionKey, "csrf", p.ID))[:32]
	return Session{Token: token, CSRF: csrf, User: p.User, Name: p.Name, Roles: p.Roles, ExpiresAt: time.Unix(p.Expires, 0).UTC(), MustChange: c.MustChange && p.User == AdminUser, id: p.ID}
}

func (s *Service) parse(c Credentials, token string) (payload, error) {
	var p payload
	body, sig, ok := strings.Cut(token, ".")
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if !ok || err != nil || !hmac.Equal(got, mac(c.SessionKey, "session", body)) {
		return p, ErrNoSession
	}
	b, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || json.Unmarshal(b, &p) != nil {
		return p, ErrNoSession
	}
	return p, nil
}

// Session validates a token. Past half its lifetime it is renewed (same
// session and CSRF token, later expiry; Renewed is set).
func (s *Service) Session(token string) (Session, error) {
	c, err := s.Store.Load()
	if errors.Is(err, ErrNoCredentials) {
		return Session{}, ErrNoSession
	}
	if err != nil {
		return Session{}, err
	}
	p, err := s.parse(c, token)
	if err != nil {
		return Session{}, err
	}
	now := s.Clock()
	exp := time.Unix(p.Expires, 0)
	s.mu.Lock()
	_, revoked := s.revoked[p.ID]
	s.mu.Unlock()
	if p.Gen != c.Generation || !now.Before(exp) || revoked {
		return Session{}, ErrNoSession
	}
	if exp.Sub(now) < s.ttl()/2 {
		sess := s.issue(c, p)
		sess.Renewed = true
		return sess, nil
	}
	return Session{Token: token, CSRF: s.issue(c, p).CSRF, User: p.User, Name: p.Name, Roles: p.Roles, ExpiresAt: exp.UTC(), MustChange: c.MustChange && p.User == AdminUser, id: p.ID}, nil
}

// Logout ends a session on this node. (Tokens are not stored, so another
// node would still accept it until it expires; the console drops the cookie.)
func (s *Service) Logout(token string) {
	sess, err := s.Session(token)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revoked == nil {
		s.revoked = map[string]time.Time{}
	}
	now := s.Clock()
	for id, exp := range s.revoked {
		if !now.Before(exp) {
			delete(s.revoked, id)
		}
	}
	s.revoked[sess.id] = sess.ExpiresAt
}

// ChangePassword sets a new password, which ends every session; the caller
// gets a fresh token for the same session.
func (s *Service) ChangePassword(token, current, next string) (Session, error) {
	sess, err := s.Session(token)
	if err != nil {
		return Session{}, err
	}
	c, err := s.Store.Load()
	if err != nil {
		return Session{}, err
	}
	if !s.verify(c, current) {
		return Session{}, ErrInvalidCredentials
	}
	if len(next) < minPasswordLen {
		return Session{}, ErrWeakPassword
	}
	if err := s.SetPassword(next); err != nil {
		return Session{}, err
	}
	if c, err = s.Store.Load(); err != nil {
		return Session{}, err
	}
	fresh := s.issue(c, payload{ID: sess.id, User: sess.User, Name: sess.Name, Roles: sess.Roles})
	fresh.Renewed = true
	return fresh, nil
}
