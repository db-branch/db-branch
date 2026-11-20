package runtimegrpc

import (
	"io"
	"os/exec"

	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/db-branch/db-branch/pkg/ext/runtimeplugin"
)

// Launch starts the module process cmd and connects to it over gRPC. The
// caller prepares cmd: path, environment, and (for a module run as its own OS
// user) SysProcAttr.Credential. stop ends the process; it is safe to call twice.
//
// The process must answer go-plugin's handshake with the runtime cookie, so a
// binary that is not a runtime module is refused, and the connection is
// gRPC only (the net/rpc transport of the other modules is not accepted).
func Launch(cmd *exec.Cmd) (*Provider, func(), error) {
	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig:  runtimeplugin.Handshake,
		Plugins:          goplugin.PluginSet{runtimeplugin.Name: &runtimeplugin.Plugin{}},
		Cmd:              cmd,
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
		// go-plugin would add the agent's whole environment (secrets included) to cmd.Env.
		SkipHostEnv: true,
		Logger:      hclog.New(&hclog.LoggerOptions{Output: io.Discard}),
	})
	rpc, err := client.Client()
	if err != nil {
		client.Kill()
		return nil, nil, err
	}
	raw, err := rpc.Dispense(runtimeplugin.Name)
	if err != nil {
		client.Kill()
		return nil, nil, err
	}
	// runtimeplugin.Plugin.GRPCClient always returns the connection.
	return New(raw.(*grpc.ClientConn)), client.Kill, nil
}
