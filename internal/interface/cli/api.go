package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Client calls the agent's /v1 API.
type Client interface {
	// Do sends in as JSON (when non-nil) and decodes the reply into out (when non-nil).
	// A non-2xx reply is returned as *APIError.
	Do(ctx context.Context, method, path string, in, out any) error
}

// Raw is a request body sent as is (e.g. a backup file), not as JSON.
type Raw []byte

// APIError is an error reply from the agent.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("%s (%s)", e.Message, e.Code) }

// SocketClient talks HTTP over the agent's UNIX socket.
type SocketClient struct{ http *http.Client }

// NewSocketClient returns a client for the socket at path.
func NewSocketClient(path string) *SocketClient {
	return &SocketClient{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}}}
}

// Do implements Client.
func (c *SocketClient) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	ctype := "application/json"
	if raw, ok := in.(Raw); ok {
		body, ctype = bytes.NewReader(raw), "application/octet-stream"
	} else if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://agent"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", ctype)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the db-branch agent (is it running? try: sudo systemctl status db-branch): %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&env)
		return &APIError{Status: resp.StatusCode, Code: env.Error.Code, Message: env.Error.Message}
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type operationJSON struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	State string `json:"state"`
	Phase string `json:"phase"`
	Error string `json:"error"`
}

// start sends a request answered with 202 + operation and, unless noWait,
// waits for the operation, printing each new phase to stderr.
func (a *App) start(ctx context.Context, c Client, method, path string, in any, noWait bool) error {
	var op operationJSON
	if err := c.Do(ctx, method, path, in, &op); err != nil {
		return err
	}
	if noWait {
		fmt.Fprintln(a.Stdout, op.ID)
		return nil
	}
	poll := a.Poll
	if poll == 0 {
		poll = 200 * time.Millisecond
	}
	last := ""
	for {
		if err := c.Do(ctx, "GET", "/v1/operations/"+op.ID, nil, &op); err != nil {
			return err
		}
		if op.Phase != "" && op.Phase != last {
			fmt.Fprintf(a.Stderr, "  %s...\n", op.Phase)
			last = op.Phase
		}
		switch op.State {
		case "succeeded":
			return nil
		case "failed":
			return fmt.Errorf("%s failed: %s", op.Kind, op.Error)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}
