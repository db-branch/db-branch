package plugin

import (
	"context"
	"errors"
	"net"
	"net/rpc"
	"testing"

	"github.com/db-branch/db-branch/pkg/ext"
)

type plainStore struct {
	data    map[string][]byte
	listErr error
	putErr  error
	delErr  error
}

func (s *plainStore) List(context.Context) (map[string][]byte, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := make(map[string][]byte, len(s.data))
	for k, v := range s.data {
		out[k] = append([]byte(nil), v...)
	}
	return out, nil
}

func (s *plainStore) Put(_ context.Context, k string, v []byte) error {
	if s.putErr != nil {
		return s.putErr
	}
	s.data[k] = append([]byte(nil), v...)
	return nil
}

func (s *plainStore) Delete(_ context.Context, k string) error {
	if s.delErr != nil {
		return s.delErr
	}
	delete(s.data, k)
	return nil
}

type atomicTestStore struct {
	plainStore
	calls []string
}

func (s *atomicTestStore) Get(_ context.Context, k string) ([]byte, bool, error) {
	s.calls = append(s.calls, "get:"+k)
	v, ok := s.data[k]
	return v, ok, s.listErr
}

func (s *atomicTestStore) PutIfAbsent(_ context.Context, k string, v []byte) (bool, error) {
	s.calls = append(s.calls, "putIfAbsent:"+k)
	if s.putErr != nil {
		return false, s.putErr
	}
	if _, exists := s.data[k]; exists {
		return false, nil
	}
	s.data[k] = append([]byte(nil), v...)
	return true, nil
}

func (s *atomicTestStore) Take(_ context.Context, k string) ([]byte, bool, error) {
	s.calls = append(s.calls, "take:"+k)
	if s.delErr != nil {
		return nil, false, s.delErr
	}
	v, ok := s.data[k]
	if !ok {
		return nil, false, nil
	}
	delete(s.data, k)
	return v, true, nil
}

// legacyStoreServer mimics an older host exposing only List, Put, and Delete.
type legacyStoreServer struct {
	S ext.Store
}

func (s *legacyStoreServer) List(_ struct{}, out *StoreListReply) error {
	recs, err := s.S.List(context.Background())
	out.Records, out.Err = recs, toRPC(err)
	return nil
}

func (s *legacyStoreServer) Put(a StorePutArgs, out *RPCError) error {
	*out = toRPC(s.S.Put(context.Background(), a.Key, a.Value))
	return nil
}

func (s *legacyStoreServer) Delete(a StorePutArgs, out *RPCError) error {
	*out = toRPC(s.S.Delete(context.Background(), a.Key))
	return nil
}

func dialRPCForTest(t *testing.T, receiver any) *rpc.Client {
	t.Helper()
	srv := rpc.NewServer()
	if err := srv.RegisterName("Plugin", receiver); err != nil {
		t.Fatal(err)
	}
	cConn, sConn := net.Pipe()
	go srv.ServeConn(sConn)
	client := rpc.NewClient(cConn)
	t.Cleanup(func() {
		_ = client.Close()
		_ = sConn.Close()
	})
	return client
}

func TestStoreServer_Atomic委譲と非Atomicフォールバック(t *testing.T) {
	t.Run("plain Store fallback and errors", func(t *testing.T) {
		st := &plainStore{data: map[string][]byte{}}
		srv := &StoreServer{S: st}

		var gr StoreGetReply
		_ = srv.Get(StorePutArgs{Key: "missing"}, &gr)
		if gr.OK || gr.Err.Msg != "" {
			t.Fatalf("missing Get = %+v", gr)
		}

		var br StoreBoolReply
		_ = srv.PutIfAbsent(StorePutArgs{Key: "k1", Value: []byte("v1")}, &br)
		if !br.OK || br.Err.Msg != "" {
			t.Fatalf("first PutIfAbsent = %+v", br)
		}
		br = StoreBoolReply{}
		_ = srv.PutIfAbsent(StorePutArgs{Key: "k1", Value: []byte("v2")}, &br)
		if br.OK || br.Err.Msg != "" {
			t.Fatalf("duplicate PutIfAbsent = %+v", br)
		}

		gr = StoreGetReply{}
		_ = srv.Get(StorePutArgs{Key: "k1"}, &gr)
		if !gr.OK || string(gr.Value) != "v1" || gr.Err.Msg != "" {
			t.Fatalf("Get k1 = %+v", gr)
		}

		gr = StoreGetReply{}
		_ = srv.Take(StorePutArgs{Key: "k1"}, &gr)
		if !gr.OK || string(gr.Value) != "v1" || gr.Err.Msg != "" {
			t.Fatalf("Take k1 = %+v", gr)
		}
		gr = StoreGetReply{}
		_ = srv.Take(StorePutArgs{Key: "k1"}, &gr)
		if gr.OK || gr.Err.Msg != "" {
			t.Fatalf("second Take = %+v", gr)
		}

		boom := errors.New("boom")
		st.listErr = boom
		_ = srv.Get(StorePutArgs{Key: "k"}, &gr)
		if gr.Err.Msg == "" {
			t.Error("expected List error on Get")
		}
		_ = srv.PutIfAbsent(StorePutArgs{Key: "k", Value: []byte("x")}, &br)
		if br.Err.Msg == "" {
			t.Error("expected List error on PutIfAbsent")
		}
		_ = srv.Take(StorePutArgs{Key: "k"}, &gr)
		if gr.Err.Msg == "" {
			t.Error("expected List error on Take")
		}

		st.listErr = nil
		st.putErr = boom
		br = StoreBoolReply{}
		_ = srv.PutIfAbsent(StorePutArgs{Key: "k2", Value: []byte("v2")}, &br)
		if br.OK || br.Err.Msg == "" {
			t.Error("expected Put error on PutIfAbsent")
		}
		st.putErr = nil
		st.data["k2"] = []byte("v2")
		st.delErr = boom
		gr = StoreGetReply{}
		_ = srv.Take(StorePutArgs{Key: "k2"}, &gr)
		if gr.OK || gr.Err.Msg == "" {
			t.Error("expected Delete error on Take")
		}
	})

	t.Run("AtomicStore delegates directly", func(t *testing.T) {
		st := &atomicTestStore{plainStore: plainStore{data: map[string][]byte{}}}
		srv := &StoreServer{S: st}

		var br StoreBoolReply
		_ = srv.PutIfAbsent(StorePutArgs{Key: "k1", Value: []byte("v1")}, &br)
		if !br.OK || br.Err.Msg != "" {
			t.Fatalf("PutIfAbsent = %+v", br)
		}
		var gr StoreGetReply
		_ = srv.Get(StorePutArgs{Key: "k1"}, &gr)
		if !gr.OK || string(gr.Value) != "v1" {
			t.Fatalf("Get = %+v", gr)
		}
		gr = StoreGetReply{}
		_ = srv.Take(StorePutArgs{Key: "k1"}, &gr)
		if !gr.OK || string(gr.Value) != "v1" {
			t.Fatalf("Take = %+v", gr)
		}
		if len(st.calls) != 3 {
			t.Fatalf("calls = %v, want 3 atomic calls", st.calls)
		}
	})
}

func TestRemoteStore_古いホストでのフォールバックと通信エラー(t *testing.T) {
	ctx := context.Background()
	st := &plainStore{data: map[string][]byte{}}
	rs := &remoteStore{c: dialRPCForTest(t, &legacyStoreServer{S: st})}

	if ok, err := rs.PutIfAbsent(ctx, "k1", []byte("v1")); err != nil || !ok {
		t.Fatalf("legacy PutIfAbsent 1st: ok=%v err=%v", ok, err)
	}
	if ok, err := rs.PutIfAbsent(ctx, "k1", []byte("v2")); err != nil || ok {
		t.Fatalf("legacy PutIfAbsent 2nd: ok=%v err=%v", ok, err)
	}
	if v, ok, err := rs.Get(ctx, "k1"); err != nil || !ok || string(v) != "v1" {
		t.Fatalf("legacy Get: %q ok=%v err=%v", v, ok, err)
	}
	if v, ok, err := rs.Take(ctx, "k1"); err != nil || !ok || string(v) != "v1" {
		t.Fatalf("legacy Take 1st: %q ok=%v err=%v", v, ok, err)
	}
	if _, ok, err := rs.Take(ctx, "k1"); err != nil || ok {
		t.Fatalf("legacy Take 2nd: ok=%v err=%v", ok, err)
	}

	boom := errors.New("boom")
	st.listErr = boom
	if _, _, err := rs.Get(ctx, "k1"); err == nil {
		t.Error("expected List error on legacy Get")
	}
	if _, err := rs.PutIfAbsent(ctx, "k1", []byte("x")); err == nil {
		t.Error("expected List error on legacy PutIfAbsent")
	}
	if _, _, err := rs.Take(ctx, "k1"); err == nil {
		t.Error("expected List error on legacy Take")
	}

	st.listErr = nil
	st.data["k2"] = []byte("v2")
	st.delErr = boom
	if _, _, err := rs.Take(ctx, "k2"); err == nil {
		t.Error("expected Delete error on legacy Take")
	}

	// Closed client surfaces transport error rather than falling back.
	_ = rs.c.Close()
	if _, _, err := rs.Get(ctx, "k1"); err == nil {
		t.Error("expected RPC error after Close on Get")
	}
	if _, err := rs.PutIfAbsent(ctx, "k1", []byte("x")); err == nil {
		t.Error("expected RPC error after Close on PutIfAbsent")
	}
	if _, _, err := rs.Take(ctx, "k1"); err == nil {
		t.Error("expected RPC error after Close on Take")
	}
}
