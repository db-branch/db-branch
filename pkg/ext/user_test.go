package ext_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/db-branch/db-branch/pkg/ext"
)

type stubAuth struct {
	fn func(context.Context, ext.Credentials) (ext.Principal, error)
}

func (s stubAuth) Authenticate(ctx context.Context, c ext.Credentials) (ext.Principal, error) {
	return s.fn(ctx, c)
}

func TestChain_ErrStopAuthで後続の認証器を止める(t *testing.T) {
	fallbackCalled := false
	chain := ext.Chain{
		stubAuth{fn: func(_ context.Context, c ext.Credentials) (ext.Principal, error) {
			if c.Username == "sso@example.com" {
				return ext.Principal{}, errors.Join(ext.ErrUnauthenticated, ext.ErrStopAuth)
			}
			return ext.Principal{}, ext.ErrUnauthenticated
		}},
		stubAuth{fn: func(_ context.Context, _ ext.Credentials) (ext.Principal, error) {
			fallbackCalled = true
			return ext.Principal{ID: "fallback"}, nil
		}},
	}

	_, err := chain.Authenticate(context.Background(), ext.Credentials{Username: "sso@example.com"})
	if !errors.Is(err, ext.ErrUnauthenticated) || !errors.Is(err, ext.ErrStopAuth) {
		t.Fatalf("got err = %v, want ErrUnauthenticated + ErrStopAuth", err)
	}
	if fallbackCalled {
		t.Fatal("fallback authenticator must not be called when ErrStopAuth is returned")
	}

	p, err := chain.Authenticate(context.Background(), ext.Credentials{Username: "admin"})
	if err != nil || p.ID != "fallback" || !fallbackCalled {
		t.Fatalf("fallback failed: p=%+v err=%v", p, err)
	}
}

func TestRateLimitedError_フォーマット(t *testing.T) {
	e := &ext.RateLimitedError{RetryAfter: 15 * time.Second}
	if e.Error() == "" {
		t.Fatal("empty error string")
	}
}
