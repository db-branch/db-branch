// Package runtimeplugin is how a runtime module (a database engine for
// db-branch, see docs/runtime-modules-standard.md) serves the Runtime gRPC
// service to the core. The module's main is:
//
//	func main() { runtimeplugin.Serve(myServer{}) }
//
// where myServer implements runtimev1.RuntimeServer (embed
// runtimev1.UnimplementedRuntimeServer to leave optional features out).
package runtimeplugin

import (
	"context"

	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	pb "github.com/db-branch/db-branch/pkg/ext/proto/runtime/v1"
)

// Handshake keeps the core from running arbitrary binaries as runtime modules
// and a runtime module from being started by hand. It differs from the
// handshake of the other modules, so neither kind can be started as the other.
var Handshake = goplugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   "DB_BRANCH_RUNTIME",
	MagicCookieValue: "d2b6e1a47c0f4f58b9a3e6c1f07d85a2",
}

// Name is the plugin's name in the go-plugin plugin map.
const Name = "runtime"

// Plugin connects the Runtime service to go-plugin over gRPC.
type Plugin struct {
	goplugin.NetRPCUnsupportedPlugin
	// Impl is the module's server (set on the module side only).
	Impl pb.RuntimeServer
}

// GRPCServer registers the module's server.
func (p *Plugin) GRPCServer(_ *goplugin.GRPCBroker, s *grpc.Server) error {
	pb.RegisterRuntimeServer(s, p.Impl)
	return nil
}

// GRPCClient hands the core the connection to the module.
func (p *Plugin) GRPCClient(_ context.Context, _ *goplugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return c, nil
}

func serveConfig(impl pb.RuntimeServer) *goplugin.ServeConfig {
	return &goplugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         goplugin.PluginSet{Name: &Plugin{Impl: impl}},
		GRPCServer:      goplugin.DefaultGRPCServer,
	}
}

// Serve runs impl as a runtime module until the core stops it. It returns
// at once when the binary was not started by the core.
func Serve(impl pb.RuntimeServer) { serve(serveConfig(impl)) }

// serve is goplugin.Serve; tests replace it (the real one takes over the process).
var serve = goplugin.Serve
