package plugin

import (
	"context"
	"errors"
	"testing"

	"github.com/db-branch/db-branch/pkg/ext"
)

type fakeAPI struct{ got []ext.Request }

func (f *fakeAPI) Call(_ context.Context, r ext.Request) (ext.Response, error) {
	f.got = append(f.got, r)
	if r.Path == "/boom" {
		return ext.Response{}, ext.ErrForbidden
	}
	return ext.Response{Status: 202, Body: []byte("hello " + r.Principal.ID)}, nil
}

func Test_モジュールは自分のAPIをエージェント経由で呼べる(t *testing.T) {
	fa := &fakeAPI{}
	st := starter{host: make(chan ext.Host, 1)}
	r := remoteFor(t, ext.Module{Name: "m", Starter: st})
	if err := r.Start(ctx, ext.Host{Node: "n1", Store: memStore{}, API: fa}); err != nil {
		t.Fatal(err)
	}
	api := (<-st.host).API
	if api == nil {
		t.Fatal("no API")
	}
	resp, err := api.Call(ctx, ext.Request{Method: "GET", Path: "/branches/x/info", Principal: ext.Principal{ID: "alice"}})
	if err != nil || resp.Status != 202 || string(resp.Body) != "hello alice" || fa.got[0].Path != "/branches/x/info" {
		t.Errorf("%+v %v", resp, err)
	}
	if _, err := api.Call(ctx, ext.Request{Path: "/boom"}); !errors.Is(err, ext.ErrForbidden) {
		t.Errorf("a refusal keeps its sentinel: %v", err)
	}
	api.(*apiRemote).c.Close()
	if _, err := api.Call(ctx, ext.Request{Path: "/x"}); err == nil {
		t.Error("closed: want error")
	}
	// An agent without the API gives the module none.
	bare := starter{host: make(chan ext.Host, 1)}
	r2 := remoteFor(t, ext.Module{Name: "m2", Starter: bare})
	if err := r2.Start(ctx, ext.Host{Store: memStore{}}); err != nil {
		t.Fatal(err)
	}
	if (<-bare.host).API != nil {
		t.Error("the host should have no API")
	}
}

func Test_存在しないAPIの口を渡されたら起動に失敗する(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the broker to give up")
	}
	r := remoteFor(t, ext.Module{Name: "m", Starter: starter{host: make(chan ext.Host, 1)}})
	id := r.broker.NextId()
	go r.broker.AcceptAndServe(id, &StoreServer{S: memStore{}})
	var out RPCError
	if err := r.c.Call("Plugin.Start", StartArgs{StoreID: id, APIID: 9999}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Msg == "" {
		t.Error("an API stream that never arrives should fail the start")
	}
}
