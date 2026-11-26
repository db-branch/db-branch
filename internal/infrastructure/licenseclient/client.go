// Package licenseclient asks the licence service for the renewed key of a
// subscription. It sends the current key and the cluster's anonymous id, and
// nothing else.
package licenseclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/db-branch/db-branch/internal/usecase/licensing"
)

// Timeout bounds one request.
const Timeout = 15 * time.Second

// maxBody is the most of an answer that is read (a key is a few KB).
const maxBody = 64 << 10

// Client calls the renew endpoint.
type Client struct {
	// Version goes into the User-Agent: db-branch/<Version>.
	Version string
	// HTTP is the client to use; nil: one with Timeout that follows no redirect.
	HTTP *http.Client
}

var _ licensing.RenewClient = (*Client)(nil)

var defaultHTTPClient = &http.Client{
	Timeout: Timeout,
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return defaultHTTPClient
}

// Renew posts {"key": key, "cluster": clusterID} to renewURL; without a
// clusterID (not drawn yet) the "cluster" field is left out. A new key comes back as is; the
// other answers are the errors of the licensing package, wrapped with the
// service's message.
func (c *Client) Renew(ctx context.Context, renewURL, key, clusterID string) (string, error) {
	if err := licensing.CheckRenewURL(renewURL); err != nil {
		return "", err
	}
	fields := map[string]string{"key": key}
	if clusterID != "" {
		fields["cluster"] = clusterID
	}
	body, _ := json.Marshal(fields)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, renewURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "db-branch/"+c.Version)
	resp, err := c.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("contacting the licence service: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", fmt.Errorf("reading the licence service's answer: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		var out struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(data, &out); err != nil || strings.TrimSpace(out.Key) == "" {
			return "", errors.New("the licence service answered 200 without a key")
		}
		return out.Key, nil
	case http.StatusNoContent:
		return "", licensing.ErrNoNewerKey
	case http.StatusPaymentRequired:
		return "", fmt.Errorf("%w: %s", licensing.ErrSubscriptionInactive, message(data))
	case http.StatusTooManyRequests:
		return "", &licensing.RetryLaterError{After: retryAfter(resp.Header.Get("Retry-After"))}
	case http.StatusBadRequest, http.StatusUnauthorized:
		return "", fmt.Errorf("%w: %s", licensing.ErrKeyNotRecognised, message(data))
	}
	return "", fmt.Errorf("the licence service answered %s: %s", resp.Status, message(data))
}

// retryAfter reads Retry-After seconds; a missing or odd value is an hour,
// and no wait is longer than a week.
func retryAfter(h string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(h))
	if err != nil || n <= 0 {
		return time.Hour
	}
	return min(time.Duration(n)*time.Second, 7*24*time.Hour)
}

// message is error.message of an {"error":{...}} body, or "".
func message(data []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &e) != nil {
		return ""
	}
	return e.Error.Message
}
