package app

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/routing"
	"github.com/db-branch/db-branch/pkg/ext"
)

// listenSocket opens the API socket, readable only by root and the db-branch group.
func listenSocket(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		l.Close()
		return nil, err
	}
	if g, err := user.LookupGroup(ServiceUser); err == nil {
		gid, _ := strconv.Atoi(g.Gid)
		_ = os.Chown(path, -1, gid) // best effort: only root may change the group
	}
	return l, nil
}

// lateHandler lets the node API serve this node's /v1 handler, which is
// built after the cluster (and its node API listener) is up.
type lateHandler struct {
	mu sync.RWMutex
	h  http.Handler
}

func (l *lateHandler) set(h http.Handler) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.h = h
}

func (l *lateHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.mu.RLock()
	h := l.h
	l.mu.RUnlock()
	if h == nil {
		http.Error(w, "agent starting", http.StatusServiceUnavailable)
		return
	}
	h.ServeHTTP(w, r)
}

// proxyRouting wires routing when a module provides a Router.
func proxyRouting(a *App, cl *clusterRuntime, node, host string, router ext.Router) *routing.Service {
	if router == nil {
		return nil
	}
	svc, err := a.Branches()
	if err != nil {
		return nil
	}
	users, ok := svc.Store.(domain.RouteUserStore)
	if !ok {
		return nil
	}
	rt := &routing.Service{
		Store: svc.Store, Users: users, Node: node, Host: host,
		Admin: routerAdmin{router},
	}
	if cl != nil {
		rt.Nodes = cl.store
	}
	return rt
}

// routerAdmin hands routing configs to the module's Router.
type routerAdmin struct{ r ext.Router }

func (a routerAdmin) Apply(ctx context.Context, c routing.Config) error { return a.r.Route(ctx, c) }
