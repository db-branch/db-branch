// Package notifier delivers notices to Slack and to webhooks.
package notifier

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/db-branch/db-branch/internal/usecase/notify"
)

var client = &http.Client{Timeout: 10 * time.Second}

func post(ctx context.Context, url string, body []byte, header map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s answered %s", url, resp.Status)
	}
	return nil
}

var icons = map[string]string{"info": ":information_source:", "warning": ":warning:", "error": ":rotating_light:"}

// Slack posts to an incoming webhook (URL "" turns it off).
type Slack struct{ URL func() string }

func (s Slack) Send(ctx context.Context, n notify.Notice) error {
	u := s.URL()
	if u == "" {
		return nil
	}
	b, _ := json.Marshal(map[string]string{"text": icons[n.Level] + " *db-branch* " + n.Text})
	return post(ctx, u, b, nil)
}

// Webhook posts each notice as JSON, signed with Secret when set:
// X-Db-Branch-Signature: sha256=<hex HMAC-SHA256 of the body>.
type Webhook struct {
	URL    func() string
	Secret func() string
}

func (w Webhook) Send(ctx context.Context, n notify.Notice) error {
	u := w.URL()
	if u == "" {
		return nil
	}
	b, _ := json.Marshal(n)
	h := map[string]string{"X-Db-Branch-Event": n.Event}
	if s := w.Secret(); s != "" {
		mac := hmac.New(sha256.New, []byte(s))
		mac.Write(b)
		h["X-Db-Branch-Signature"] = "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	return post(ctx, u, b, h)
}
