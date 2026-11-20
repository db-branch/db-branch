package runtimeplugin

import (
	"context"
	"testing"

	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	pb "github.com/db-branch/db-branch/pkg/ext/proto/runtime/v1"
)

func TestHandshake_既存モジュールのCookieと区別される(t *testing.T) {
	// A runtime module must not start as a plain module or the other way round.
	if Handshake.MagicCookieKey == "DB_BRANCH_MODULE" {
		t.Fatal("the runtime handshake must differ from the module one")
	}
}

func TestPlugin_サービスを登録しコネクションをそのまま返す(t *testing.T) {
	p := &Plugin{Impl: pb.UnimplementedRuntimeServer{}}
	s := grpc.NewServer()
	if err := p.GRPCServer(nil, s); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.GetServiceInfo()["dbbranch.runtime.v1.Runtime"]; !ok {
		t.Fatal("Runtime service not registered")
	}
	conn := &grpc.ClientConn{}
	got, err := p.GRPCClient(context.Background(), nil, conn)
	if err != nil || got != conn {
		t.Fatalf("%v %v", got, err)
	}
}

func TestServeConfig_gRPCだけで指定のハンドシェイクを使う(t *testing.T) {
	c := serveConfig(pb.UnimplementedRuntimeServer{})
	if c.HandshakeConfig != Handshake || c.GRPCServer == nil || c.Plugins[Name] == nil {
		t.Fatalf("%+v", c)
	}
}

func TestServe_設定を作ってgo_pluginに渡す(t *testing.T) {
	var got *goplugin.ServeConfig
	old := serve
	serve = func(c *goplugin.ServeConfig) { got = c }
	defer func() { serve = old }()
	Serve(pb.UnimplementedRuntimeServer{})
	if got == nil || got.HandshakeConfig != Handshake {
		t.Fatalf("%+v", got)
	}
}
