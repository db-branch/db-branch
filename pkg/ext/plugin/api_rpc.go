package plugin

import (
	"context"
	"errors"
	"net/rpc"

	"github.com/db-branch/db-branch/pkg/ext"
)

// APIServer serves the agent's ModuleAPI to a module (agent side).
type APIServer struct{ A ext.ModuleAPI }

// Call runs one request against the module's own API.
func (s *APIServer) Call(r ext.Request, out *ServeReply) error {
	resp, err := s.A.Call(context.Background(), r)
	out.Response, out.Err = resp, toRPC(err)
	return nil
}

// apiRemote is the agent's ModuleAPI seen from the module.
type apiRemote struct{ c *rpc.Client }

func (a *apiRemote) Call(_ context.Context, r ext.Request) (ext.Response, error) {
	var out ServeReply
	if err := a.c.Call("Plugin.Call", r, &out); err != nil {
		return ext.Response{}, errors.Join(errors.New("the agent did not answer"), err)
	}
	return out.Response, out.Err.err()
}
