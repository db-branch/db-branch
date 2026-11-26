package httpapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/filestore"
	"github.com/db-branch/db-branch/internal/interface/httpapi"
	"github.com/db-branch/db-branch/internal/usecase/auth"
	"github.com/db-branch/db-branch/internal/usecase/operation"
	"github.com/db-branch/db-branch/internal/usecase/token"
	"github.com/db-branch/db-branch/pkg/ext"
)

type testHash struct{}

func (testHash) Hash(pw string, salt []byte) []byte {
	h := sha256.Sum256(append(append([]byte{}, salt...), pw...))
	return h[:]
}

type credStore struct {
	c    auth.Credentials
	have bool
}

func (s *credStore) Load() (auth.Credentials, error) {
	if !s.have {
		return auth.Credentials{}, auth.ErrNoCredentials
	}
	return s.c, nil
}

func (s *credStore) Save(c auth.Credentials) error {
	s.c = c
	s.have = true
	return nil
}

type stubExtAuth struct {
	authenticate     func(context.Context, ext.Credentials) (ext.Principal, error)
	resolvePrincipal func(context.Context, ext.Principal) (ext.Principal, error)
	changePassword   func(context.Context, ext.Principal, string, string) error
}

func (s *stubExtAuth) Authenticate(ctx context.Context, c ext.Credentials) (ext.Principal, error) {
	if s.authenticate != nil {
		return s.authenticate(ctx, c)
	}
	return ext.Principal{}, ext.ErrUnauthenticated
}

func (s *stubExtAuth) ResolvePrincipal(ctx context.Context, p ext.Principal) (ext.Principal, error) {
	if s.resolvePrincipal != nil {
		return s.resolvePrincipal(ctx, p)
	}
	return p, nil
}

func (s *stubExtAuth) ChangePassword(ctx context.Context, p ext.Principal, cur, next string) error {
	if s.changePassword != nil {
		return s.changePassword(ctx, p, cur, next)
	}
	return ext.ErrForbidden
}

func newUserTestHarness(t *testing.T, mod ...ext.Module) (*httpapi.Server, http.Handler, http.Handler, *auth.Service, *token.Service, *filestore.Store) {
	t.Helper()
	dir := t.TempDir()
	fs := &filestore.Store{Path: filepath.Join(dir, "state.json")}
	authSvc := &auth.Service{
		Store:  &credStore{},
		Users:  fs,
		Hasher: testHash{},
		Clock:  time.Now,
	}
	tokSvc := &token.Service{Store: fs, Clock: time.Now}
	authSvc.RevokeTokens = tokSvc.RevokeByOwner
	if err := authSvc.SetPassword("admin initial secret"); err != nil {
		t.Fatal(err)
	}
	srv := &httpapi.Server{
		Branches:  func() (httpapi.Branches, error) { return nil, errors.New("unused") },
		Bus:       operation.NewBus(8),
		Auth:      authSvc,
		Tokens:    tokSvc,
		TokenAuth: tokSvc,
	}
	if len(mod) > 0 {
		srv.Ext = mod[0]
	}
	return srv, srv.Handler(false), srv.Handler(true), authSvc, tokSvc, fs
}

func reqJSON(t *testing.T, h http.Handler, method, path string, body any, cookie *http.Cookie, csrf, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func loginReq(t *testing.T, h http.Handler, user, password string) (*http.Cookie, map[string]any, int) {
	t.Helper()
	rec := reqJSON(t, h, http.MethodPost, "/v1/auth/login", map[string]string{
		"username": user,
		"password": password,
	}, nil, "", "")
	var payload map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	var sessCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "dbb_session" {
			sessCookie = c
		}
	}
	return sessCookie, payload, rec.Code
}

func TestHTTPAPI_UsersCRUD_Tokens_Session_SelfAndLastUser(t *testing.T) {
	_, remote, local, _, tokSvc, fs := newUserTestHarness(t)

	// Unauthenticated request rejected.
	if rec := reqJSON(t, remote, http.MethodGet, "/v1/users", nil, nil, "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon GET /v1/users: want 401, got %d", rec.Code)
	}

	// Scoped (branches) token is rejected with 403 on user endpoints.
	branchesTok, _, err := tokSvc.Create(context.Background(), "b-only", domain.ScopeBranches, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rec := reqJSON(t, remote, http.MethodGet, "/v1/users", nil, nil, "", branchesTok); rec.Code != http.StatusForbidden {
		t.Fatalf("scope:branches GET /v1/users: want 403, got %d (%s)", rec.Code, rec.Body.String())
	}

	// Admin login works and lists built-in admin.
	admCookie, admSess, code := loginReq(t, remote, "admin", "admin initial secret")
	if code != http.StatusOK || admCookie == nil {
		t.Fatalf("admin login: code=%d body=%v", code, admSess)
	}
	admCSRF := admSess["csrf_token"].(string)

	// Create user without password -> random initial_password + must_change_password=true.
	rec := reqJSON(t, remote, http.MethodPost, "/v1/users", map[string]any{
		"email": "Alice@Example.com",
		"name":  "Alice",
	}, admCookie, admCSRF, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/users: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		User            map[string]any `json:"user"`
		InitialPassword string         `json:"initial_password"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if len(created.InitialPassword) != 24 || created.User["id"] != "user:alice@example.com" || created.User["must_change_password"] != true {
		t.Fatalf("unexpected create response: %+v", created)
	}
	if _, hasHash := created.User["hash"]; hasHash {
		t.Fatal("user response must never include hash")
	}

	// Explicitly empty password -> 400 weak_password (must NOT auto-generate).
	rec = reqJSON(t, remote, http.MethodPost, "/v1/users", map[string]any{
		"email":    "empty@example.com",
		"password": "",
	}, admCookie, admCSRF, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /v1/users with empty password: want 400, got %d", rec.Code)
	}

	// Duplicate email -> 409 already_exists.
	rec = reqJSON(t, remote, http.MethodPost, "/v1/users", map[string]any{
		"email":    "alice@example.com",
		"password": "some long passphrase",
	}, admCookie, admCSRF, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate user: want 409, got %d", rec.Code)
	}

	// Alice logs in with generated initial password -> must_change_password=true, blocked from /v1/users until changed.
	aliceCookie, aliceSess, code := loginReq(t, remote, "alice@example.com", created.InitialPassword)
	if code != http.StatusOK || aliceSess["must_change_password"] != true || aliceSess["can_change_password"] != true {
		t.Fatalf("alice initial login: code=%d sess=%v", code, aliceSess)
	}
	aliceCSRF := aliceSess["csrf_token"].(string)
	if rec := reqJSON(t, remote, http.MethodGet, "/v1/users", nil, aliceCookie, aliceCSRF, ""); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "password_change_required") {
		t.Fatalf("must_change gate: want 403 password_change_required, got %d %s", rec.Code, rec.Body.String())
	}

	// Alice changes her own password -> receives renewed cookie, must_change clears.
	rec = reqJSON(t, remote, http.MethodPut, "/v1/auth/password", map[string]string{
		"current_password": created.InitialPassword,
		"new_password":     "alice chosen secret pw",
	}, aliceCookie, aliceCSRF, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("alice PUT /v1/auth/password: code=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "dbb_session" {
			aliceCookie = c
		}
	}
	if rec := reqJSON(t, remote, http.MethodGet, "/v1/users", nil, aliceCookie, aliceCSRF, ""); rec.Code != http.StatusOK {
		t.Fatalf("alice GET /v1/users after password change: want 200, got %d %s", rec.Code, rec.Body.String())
	}

	// Alice creates a personal API token (branches-scoped) and we also seed a full-scoped owned token.
	rec = reqJSON(t, remote, http.MethodPost, "/v1/account/tokens", map[string]string{"name": "cli"}, aliceCookie, aliceCSRF, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create personal token: %d %s", rec.Code, rec.Body.String())
	}
	var ptok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &ptok)
	secret := "0123456789abcdef"
	fullSecret := "dbb_fullalice_" + secret
	fh := sha256.Sum256([]byte(secret))
	if err := fs.PutToken(context.Background(), domain.APIToken{
		ID: "fullalice", Name: "full-alice", Hash: hex.EncodeToString(fh[:]), Scope: domain.ScopeFull,
		Owner: "user:alice@example.com", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	// Self-protection: Alice cannot disable or delete herself (via session OR full personal token).
	if rec := reqJSON(t, remote, http.MethodPatch, "/v1/users/user:alice@example.com", map[string]any{"disabled": true}, aliceCookie, aliceCSRF, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("self disable via session: want 400, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := reqJSON(t, remote, http.MethodDelete, "/v1/users/user:alice@example.com", nil, nil, "", ptok.Token); rec.Code != http.StatusForbidden {
		t.Fatalf("branches-scoped personal token delete: want 403, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := reqJSON(t, remote, http.MethodDelete, "/v1/users/user:alice@example.com", nil, nil, "", fullSecret); rec.Code != http.StatusBadRequest {
		t.Fatalf("self delete via full personal token: want 400, got %d %s", rec.Code, rec.Body.String())
	}

	// GET /v1/users & GET /v1/users/{id} & error status checks.
	if rec := reqJSON(t, remote, http.MethodGet, "/v1/users", nil, admCookie, admCSRF, ""); rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/users: %d %s", rec.Code, rec.Body.String())
	}
	if rec := reqJSON(t, remote, http.MethodGet, "/v1/users/user:alice@example.com", nil, admCookie, admCSRF, ""); rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/users/{id}: %d %s", rec.Code, rec.Body.String())
	}
	if rec := reqJSON(t, remote, http.MethodGet, "/v1/users/bad-id", nil, admCookie, admCSRF, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("GET bad id: want 400, got %d", rec.Code)
	}
	if rec := reqJSON(t, remote, http.MethodGet, "/v1/users/missing@example.com", nil, admCookie, admCSRF, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("GET missing id: want 404, got %d", rec.Code)
	}

	// Reset Alice's password.
	rec = reqJSON(t, remote, http.MethodPost, "/v1/users/user:alice@example.com/reset-password", map[string]any{}, admCookie, admCSRF, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("reset-password: %d %s", rec.Code, rec.Body.String())
	}
	var resetOut struct {
		Password string `json:"password"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resetOut)
	if len(resetOut.Password) != 24 {
		t.Fatalf("expected 24-char reset password, got %q", resetOut.Password)
	}

	// Admin disables Alice -> Alice's session and personal tokens immediately fail with 401 & tokens are revoked.
	rec = reqJSON(t, remote, http.MethodPatch, "/v1/users/user:alice@example.com", map[string]any{"disabled": true}, admCookie, admCSRF, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("disable alice: %d %s", rec.Code, rec.Body.String())
	}
	if rec := reqJSON(t, remote, http.MethodGet, "/v1/users", nil, aliceCookie, aliceCSRF, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled alice session must get 401, got %d", rec.Code)
	}
	if rec := reqJSON(t, remote, http.MethodGet, "/v1/users", nil, nil, "", ptok.Token); rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled alice personal token must get 401, got %d", rec.Code)
	}
	if owned, _ := tokSvc.ListOwned(context.Background(), "user:alice@example.com"); len(owned) != 0 {
		t.Fatalf("expected all alice personal tokens revoked on disable, remaining=%d", len(owned))
	}

	// Local socket can re-enable and delete Alice, and then cannot delete the last user (admin).
	if rec := reqJSON(t, local, http.MethodPatch, "/v1/users/user:alice@example.com", map[string]any{"disabled": false}, nil, "", ""); rec.Code != http.StatusOK {
		t.Fatalf("local enable alice: %d %s", rec.Code, rec.Body.String())
	}
	if rec := reqJSON(t, local, http.MethodDelete, "/v1/users/user:alice@example.com", nil, nil, "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("local delete alice: %d %s", rec.Code, rec.Body.String())
	}
	if rec := reqJSON(t, local, http.MethodDelete, "/v1/users/admin", nil, nil, "", ""); rec.Code != http.StatusConflict {
		t.Fatalf("delete last active user: want 409, got %d %s", rec.Code, rec.Body.String())
	}

	// Create Bob, delete admin, then use POST /v1/auth/reset on local socket to recover admin.
	if rec := reqJSON(t, local, http.MethodPost, "/v1/users", map[string]any{"email": "bob@example.com", "password": "bob secret passphrase"}, nil, "", ""); rec.Code != http.StatusCreated {
		t.Fatalf("create bob: %d %s", rec.Code, rec.Body.String())
	}
	if rec := reqJSON(t, local, http.MethodDelete, "/v1/users/admin", nil, nil, "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete admin with bob active: %d %s", rec.Code, rec.Body.String())
	}
	rec = reqJSON(t, local, http.MethodPost, "/v1/auth/reset", nil, nil, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("local auth reset after admin deleted: %d %s", rec.Code, rec.Body.String())
	}
	var authResetOut struct {
		Password string `json:"password"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &authResetOut)
	if _, _, code := loginReq(t, remote, "admin", authResetOut.Password); code != http.StatusOK {
		t.Fatalf("login with recovered admin password: code=%d", code)
	}
}

func TestHTTPAPI_ExtErrors_And_PasswordChangerReissue(t *testing.T) {
	stub := &stubExtAuth{}
	_, remote, local, authSvc, _, _ := newUserTestHarness(t, ext.Module{
		Authenticator:     stub,
		PrincipalResolver: stub,
	})
	ctx := context.Background()
	u, err := authSvc.CreateUser(ctx, auth.CreateUserInput{
		Email:      "mod@example.com",
		Name:       "ModUser",
		Password:   "initial module pw",
		MustChange: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	stub.authenticate = func(_ context.Context, c ext.Credentials) (ext.Principal, error) {
		switch c.Username {
		case "rate@example.com":
			return ext.Principal{}, &ext.RateLimitedError{RetryAfter: 12 * time.Second}
		case "conflict@example.com":
			return ext.Principal{}, ext.ErrConflict
		case "exists@example.com":
			return ext.Principal{}, ext.ErrAlreadyExists
		case "notfound@example.com":
			return ext.Principal{}, ext.ErrNotFound
		case "upgrading@example.com":
			return ext.Principal{}, ext.ErrClusterUpgrading
		case "weak@example.com":
			return ext.Principal{}, ext.ErrWeakPassword
		case "invalid@example.com":
			return ext.Principal{}, ext.ErrInvalid
		}
		if c.Client == "" {
			return ext.Principal{}, errors.New("missing Credentials.Client")
		}
		return ext.Principal{ID: u.ID, Name: u.Name, Roles: []string{"maintainer"}}, nil
	}
	stub.changePassword = func(ctx context.Context, p ext.Principal, cur, next string) error {
		mustChange := false
		_, err := authSvc.UpdateUser(ctx, p.ID, auth.UpdateUserInput{Password: &next, MustChange: &mustChange})
		return err
	}

	// ext error mappings through fail().
	cases := []struct {
		user string
		want int
	}{
		{"rate@example.com", http.StatusTooManyRequests},
		{"conflict@example.com", http.StatusConflict},
		{"exists@example.com", http.StatusConflict},
		{"notfound@example.com", http.StatusNotFound},
		{"upgrading@example.com", http.StatusServiceUnavailable},
		{"weak@example.com", http.StatusBadRequest},
		{"invalid@example.com", http.StatusBadRequest},
	}
	for _, tc := range cases {
		if _, _, code := loginReq(t, remote, tc.user, "pw"); code != tc.want {
			t.Fatalf("fail mapping for %s: want %d, got %d", tc.user, tc.want, code)
		}
	}

	// Module PasswordChanger changePassword reissues cookie with new SessionGen & MustChange=false.
	cookie, sess, code := loginReq(t, remote, "mod@example.com", "anything")
	if code != http.StatusOK || sess["must_change_password"] != true {
		t.Fatalf("module login: code=%d sess=%v", code, sess)
	}
	csrf := sess["csrf_token"].(string)
	rec := reqJSON(t, remote, http.MethodPut, "/v1/auth/password", map[string]string{
		"current_password": "initial module pw",
		"new_password":     "updated module pw!",
	}, cookie, csrf, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("module changePassword: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var newCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "dbb_session" {
			newCookie = c
		}
	}
	if newCookie == nil {
		t.Fatal("expected renewed session cookie after PasswordChanger.ChangePassword")
	}
	if rec := reqJSON(t, remote, http.MethodGet, "/v1/users", nil, newCookie, csrf, ""); rec.Code != http.StatusOK {
		t.Fatalf("request with reissued cookie: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	// Old cookie had pre-change SessionGen -> must fail 401.
	if rec := reqJSON(t, remote, http.MethodGet, "/v1/users", nil, cookie, csrf, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("old cookie after PasswordChanger should fail 401, got %d", rec.Code)
	}

	// Unmigrated legacy external user missing from UserStore still resolves via PrincipalResolver.
	legacySess, err := authSvc.Begin("user:legacy@example.com", "Legacy", []string{"viewer"})
	if err != nil {
		t.Fatal(err)
	}
	if rec := reqJSON(t, remote, http.MethodGet, "/v1/auth/session", nil, &http.Cookie{Name: "dbb_session", Value: legacySess.Token}, legacySess.CSRF, ""); rec.Code != http.StatusOK {
		t.Fatalf("unmigrated legacy module user should defer to PrincipalResolver: %d %s", rec.Code, rec.Body.String())
	}

	// Server without Auth/Users answers 503 on /v1/users endpoints.
	uninit := (&httpapi.Server{}).Handler(false)
	if rec := reqJSON(t, uninit, http.MethodGet, "/v1/users", nil, nil, "", ""); rec.Code != http.StatusUnauthorized {
		// No session on remote requires auth first; on local socket it hits requireUsers -> 503.
		t.Fatalf("uninit remote GET /v1/users: want 401, got %d", rec.Code)
	}
	uninitLocal := (&httpapi.Server{}).Handler(true)
	if rec := reqJSON(t, uninitLocal, http.MethodGet, "/v1/users", nil, nil, "", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("uninit local GET /v1/users: want 503, got %d", rec.Code)
	}
	if rec := reqJSON(t, uninitLocal, http.MethodPost, "/v1/users", map[string]any{}, nil, "", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("uninit local POST /v1/users: want 503, got %d", rec.Code)
	}
	if rec := reqJSON(t, uninitLocal, http.MethodGet, "/v1/users/admin", nil, nil, "", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("uninit local GET /v1/users/admin: want 503, got %d", rec.Code)
	}
	if rec := reqJSON(t, uninitLocal, http.MethodPatch, "/v1/users/admin", map[string]any{}, nil, "", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("uninit local PATCH /v1/users/admin: want 503, got %d", rec.Code)
	}
	if rec := reqJSON(t, uninitLocal, http.MethodDelete, "/v1/users/admin", nil, nil, "", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("uninit local DELETE /v1/users/admin: want 503, got %d", rec.Code)
	}
	if rec := reqJSON(t, uninitLocal, http.MethodPost, "/v1/users/admin/reset-password", nil, nil, "", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("uninit local reset-password: want 503, got %d", rec.Code)
	}

	// Chunked request body (ContentLength == -1) on reset-password honors must_change_password:false.
	chunkedReq := httptest.NewRequest(http.MethodPost, "/v1/users/admin/reset-password", strings.NewReader(`{"must_change_password":false}`))
	chunkedReq.ContentLength = -1
	chunkedRec := httptest.NewRecorder()
	local.ServeHTTP(chunkedRec, chunkedReq)
	if chunkedRec.Code != http.StatusOK || !strings.Contains(chunkedRec.Body.String(), `"must_change_password":false`) {
		t.Fatalf("chunked reset-password = %d %s, want 200 with must_change_password:false", chunkedRec.Code, chunkedRec.Body.String())
	}
	badChunked := httptest.NewRequest(http.MethodPost, "/v1/users/admin/reset-password", strings.NewReader(`{invalid`))
	badChunked.ContentLength = -1
	badRec := httptest.NewRecorder()
	local.ServeHTTP(badRec, badChunked)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("invalid chunked reset-password = %d, want 400", badRec.Code)
	}
}
