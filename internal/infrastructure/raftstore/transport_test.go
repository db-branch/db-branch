package raftstore_test

import (
	"errors"
	"net"
	"testing"

	"github.com/hashicorp/raft"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/clusterpki"
	"github.com/db-branch/db-branch/internal/infrastructure/raftstore"
)

func addr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestTLSTransport_相互TLSでRaftが複製する(t *testing.T) {
	ca, _ := clusterpki.InitCA(t.TempDir())
	var stores []*raftstore.Store
	var servers []raft.Server
	for _, id := range []string{"a", "b"} {
		cert, _ := ca.IssueNode(t.TempDir(), id, []string{"127.0.0.1"})
		a := addr(t)
		tr, err := raftstore.NewTLSTransport(a, a, cert.ServerTLS(true), cert.ClientTLS(), nil)
		if err != nil {
			t.Fatal(err)
		}
		// Real mTLS handshakes under -race can exceed Fast's 50 ms lease timeout.
		s, err := raftstore.Open(raftstore.Options{NodeID: id, Transport: tr, InMemory: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		stores = append(stores, s)
		servers = append(servers, raft.Server{ID: raft.ServerID(id), Address: raft.ServerAddress(a)})
	}
	_ = stores[0].Bootstrap(servers)
	var follower *raftstore.Store
	eventually(t, func() bool {
		for i, s := range stores {
			if !s.IsLeader() {
				continue
			}
			if err := s.PutBase(ctx, domain.Base{Name: name("prod")}); err != nil {
				if errors.Is(err, domain.ErrNoLeader) {
					return false
				}
				t.Fatal(err)
			}
			follower = stores[1-i]
			return true
		}
		return false
	})
	eventually(t, func() bool { _, err := follower.GetBase(ctx, name("prod")); return err == nil })
}

func TestTLSTransport_アドレスが不正なら失敗する(t *testing.T) {
	ca, _ := clusterpki.InitCA(t.TempDir())
	cert, _ := ca.IssueNode(t.TempDir(), "x", nil)
	if _, err := raftstore.NewTLSTransport("127.0.0.1:0", "not an address", cert.ServerTLS(true), cert.ClientTLS(), nil); err == nil {
		t.Error("bad advertise: want error")
	}
	if _, err := raftstore.NewTLSTransport("256.0.0.1:1", "127.0.0.1:1", cert.ServerTLS(true), cert.ClientTLS(), nil); err == nil {
		t.Error("bad bind: want error")
	}
}
