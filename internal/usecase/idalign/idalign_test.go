package idalign_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/idalign"
)

type fakeHost struct {
	local    domain.ServiceIDs
	localErr error
	users    map[int]string
	groups   map[int]string
	stopErr  error
	reownErr error
	setErr   error
	calls    []string
}

func (f *fakeHost) Local(context.Context) (domain.ServiceIDs, error) { return f.local, f.localErr }
func (f *fakeHost) UserWithUID(uid int) string                       { return f.users[uid] }
func (f *fakeHost) GroupWithGID(gid int) string                      { return f.groups[gid] }
func (f *fakeHost) StopServices(context.Context) error {
	f.calls = append(f.calls, "stop")
	return f.stopErr
}
func (f *fakeHost) Reown(_ context.Context, from, to domain.ServiceIDs) error {
	f.calls = append(f.calls, "reown "+from.String()+" -> "+to.String())
	return f.reownErr
}
func (f *fakeHost) SetIDs(_ context.Context, from, to domain.ServiceIDs) error {
	f.calls = append(f.calls, "set "+to.String())
	return f.setErr
}

var cluster = domain.ServiceIDs{UID: 109, GID: 112}

func TestAlign_違うidなら停止_ファイル所有者変更_アカウント変更の順で行う(t *testing.T) {
	h := &fakeHost{local: domain.ServiceIDs{UID: 103, GID: 109}}
	res, err := idalign.Align(context.Background(), h, cluster)
	if err != nil || !res.Changed || res.From != h.local || res.To != cluster {
		t.Fatalf("%+v %v", res, err)
	}
	want := []string{"stop", "reown uid 103 / gid 109 -> uid 109 / gid 112", "set uid 109 / gid 112"}
	if strings.Join(h.calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls = %v", h.calls)
	}
}

func TestAlign_すでに同じidなら何もしない(t *testing.T) {
	h := &fakeHost{local: cluster}
	res, err := idalign.Align(context.Background(), h, cluster)
	if err != nil || res.Changed || len(h.calls) != 0 {
		t.Errorf("%+v %v %v", res, err, h.calls)
	}
}

func TestAlign_クラスタがidを記録していなければ何もしない(t *testing.T) {
	h := &fakeHost{localErr: errors.New("must not be asked")}
	res, err := idalign.Align(context.Background(), h, domain.ServiceIDs{})
	if err != nil || res.Changed {
		t.Errorf("%+v %v", res, err)
	}
}

func TestAlign_ローカルにdb_branchユーザーがなければ失敗する(t *testing.T) {
	h := &fakeHost{localErr: errors.New("unknown user")}
	_, err := idalign.Align(context.Background(), h, cluster)
	if err == nil || !strings.Contains(err.Error(), "install the db-branch package") {
		t.Errorf("err = %v", err)
	}
}

func TestAlign_idが別アカウントで使われていれば何も変えず対処法を示す(t *testing.T) {
	h := &fakeHost{local: domain.ServiceIDs{UID: 103, GID: 109}, users: map[int]string{109: "postgres"}, groups: map[int]string{112: "lxd"}}
	_, err := idalign.Align(context.Background(), h, cluster)
	if !errors.Is(err, idalign.ErrIDTaken) {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{`user "postgres"`, `group "lxd"`, "usermod -u", "Nothing was changed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q lacks %q", err, want)
		}
	}
	if len(h.calls) != 0 {
		t.Errorf("changed things: %v", h.calls)
	}
}

func TestAlign_自分のidと同じ値は衝突とみなさない(t *testing.T) {
	// The uid already matches (and is of course "used" by db-branch itself).
	h := &fakeHost{local: domain.ServiceIDs{UID: 109, GID: 109}, users: map[int]string{109: "db-branch"}}
	if _, err := idalign.Align(context.Background(), h, cluster); err != nil {
		t.Errorf("err = %v", err)
	}
}

func TestAlign_各段階の失敗は段階を示して返す(t *testing.T) {
	local := domain.ServiceIDs{UID: 103, GID: 109}
	for name, tc := range map[string]struct {
		h    *fakeHost
		want string
	}{
		"stop":  {&fakeHost{local: local, stopErr: errors.New("boom")}, "stopping db-branch services"},
		"reown": {&fakeHost{local: local, reownErr: errors.New("boom")}, "re-owning files"},
		"set":   {&fakeHost{local: local, setErr: errors.New("boom")}, "changing the db-branch user"},
	} {
		res, err := idalign.Align(context.Background(), tc.h, cluster)
		if err == nil || !strings.Contains(err.Error(), tc.want) || res.Changed {
			t.Errorf("%s: %+v %v", name, res, err)
		}
	}
}
