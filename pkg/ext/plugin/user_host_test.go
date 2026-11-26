package plugin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/db-branch/db-branch/pkg/ext"
)

type fakeUserHost struct {
	calls []string
	fail  error
}

func (f *fakeUserHost) ListUsers(context.Context) ([]ext.User, error) {
	f.calls = append(f.calls, "list")
	if f.fail != nil {
		return nil, f.fail
	}
	return []ext.User{{ID: "admin", Name: "admin"}, {ID: "user:a@example.com", Email: "a@example.com", Name: "Alice"}}, nil
}

func (f *fakeUserHost) GetUser(_ context.Context, id string) (ext.User, error) {
	f.calls = append(f.calls, "get "+id)
	if f.fail != nil {
		return ext.User{}, f.fail
	}
	return ext.User{ID: id, Name: "Alice", MustChange: true}, nil
}

func (f *fakeUserHost) CreateUser(_ context.Context, in ext.UserCreate) (ext.User, error) {
	f.calls = append(f.calls, "create "+in.Email)
	if f.fail != nil {
		return ext.User{}, f.fail
	}
	return ext.User{ID: "user:" + in.Email, Email: in.Email, Name: in.Name, MustChange: in.MustChange, HasPassword: !in.EmptyPassword}, nil
}

func (f *fakeUserHost) UpdateUser(_ context.Context, id string, in ext.UserUpdate) (ext.User, error) {
	f.calls = append(f.calls, "update "+id)
	if f.fail != nil {
		return ext.User{}, f.fail
	}
	disabled := in.Disabled != nil && *in.Disabled
	return ext.User{ID: id, Disabled: disabled}, nil
}

func (f *fakeUserHost) DeleteUser(_ context.Context, id string) error {
	f.calls = append(f.calls, "delete "+id)
	return f.fail
}

func (f *fakeUserHost) VerifyPassword(_ context.Context, client, username, _ string) (ext.User, error) {
	f.calls = append(f.calls, "verify "+client+" "+username)
	if f.fail != nil {
		return ext.User{}, f.fail
	}
	return ext.User{ID: "user:" + username, Email: username, Name: username}, nil
}

func TestRPCError_全センチネルと複数合成とRateLimitedErrorを運ぶ(t *testing.T) {
	sentinels := []error{
		ext.ErrUnauthenticated,
		ext.ErrForbidden,
		ext.ErrStopAuth,
		ext.ErrInvalid,
		ext.ErrAlreadyExists,
		ext.ErrNotFound,
		ext.ErrConflict,
		ext.ErrClusterUpgrading,
		ext.ErrWeakPassword,
	}
	for _, s := range sentinels {
		back := toRPC(s).err()
		if !errors.Is(back, s) {
			t.Errorf("sentinel %v lost across RPC: %v", s, back)
		}
	}
	joined := errors.Join(ext.ErrUnauthenticated, ext.ErrStopAuth)
	backJoined := toRPC(joined).err()
	if !errors.Is(backJoined, ext.ErrUnauthenticated) || !errors.Is(backJoined, ext.ErrStopAuth) {
		t.Errorf("joined error lost sentinels: %v", backJoined)
	}
	rl := &ext.RateLimitedError{RetryAfter: 42 * time.Second}
	backRL := toRPC(rl).err()
	var gotRL *ext.RateLimitedError
	if !errors.As(backRL, &gotRL) || gotRL.RetryAfter != 42*time.Second {
		t.Errorf("RateLimitedError = %v (%+v)", backRL, gotRL)
	}
}

func Test_モジュールはエージェントのユーザー操作を呼べる(t *testing.T) {
	fu := &fakeUserHost{}
	st := starter{host: make(chan ext.Host, 1)}
	r := remoteFor(t, ext.Module{Name: "m", Starter: st})
	if err := r.Start(ctx, ext.Host{Node: "n1", Store: memStore{}, Users: fu}); err != nil {
		t.Fatal(err)
	}
	u := (<-st.host).Users
	if u == nil {
		t.Fatal("no user host")
	}
	if list, err := u.ListUsers(ctx); err != nil || len(list) != 2 {
		t.Fatalf("ListUsers = %+v %v", list, err)
	}
	if got, err := u.GetUser(ctx, "user:a@example.com"); err != nil || !got.MustChange {
		t.Fatalf("GetUser = %+v %v", got, err)
	}
	if created, err := u.CreateUser(ctx, ext.UserCreate{Email: "b@example.com", Name: "Bob", Password: "pw", MustChange: true}); err != nil || created.ID != "user:b@example.com" || !created.HasPassword {
		t.Fatalf("CreateUser = %+v %v", created, err)
	}
	dis := true
	if upd, err := u.UpdateUser(ctx, "user:b@example.com", ext.UserUpdate{Disabled: &dis}); err != nil || !upd.Disabled {
		t.Fatalf("UpdateUser = %+v %v", upd, err)
	}
	if ver, err := u.VerifyPassword(ctx, "1.2.3.4", "b@example.com", "pw"); err != nil || ver.ID != "user:b@example.com" {
		t.Fatalf("VerifyPassword = %+v %v", ver, err)
	}
	if err := u.DeleteUser(ctx, "user:b@example.com"); err != nil {
		t.Fatalf("DeleteUser = %v", err)
	}
	fu.fail = ext.ErrConflict
	if _, err := u.UpdateUser(ctx, "user:a@example.com", ext.UserUpdate{}); !errors.Is(err, ext.ErrConflict) {
		t.Fatalf("error sentinel = %v, want ErrConflict", err)
	}
}
