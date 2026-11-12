package notifier_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/infrastructure/notifier"
	"github.com/db-branch/db-branch/internal/usecase/notify"
)

var n = notify.Notice{Event: "branch.expiring", Level: "warning", Subject: "x", Text: "Branch x expires in 30m.", Time: time.Unix(0, 0).UTC(), Owner: "a@x.io"}

type got struct {
	header http.Header
	body   []byte
}

func server(t *testing.T, status int) (*httptest.Server, *[]got) {
	var all []got
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		all = append(all, got{r.Header.Clone(), b})
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &all
}

func TestSlack_文面を送る(t *testing.T) {
	srv, all := server(t, 200)
	s := notifier.Slack{URL: func() string { return srv.URL }}
	if err := s.Send(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	_ = json.Unmarshal((*all)[0].body, &body)
	if !strings.Contains(body["text"], "Branch x expires") || !strings.Contains(body["text"], ":warning:") {
		t.Errorf("%v", body)
	}
	if err := (notifier.Slack{URL: func() string { return "" }}).Send(context.Background(), n); err != nil {
		t.Errorf("off: %v", err)
	}
}

func TestWebhook_署名付きのJSONを送る(t *testing.T) {
	srv, all := server(t, 204)
	w := notifier.Webhook{URL: func() string { return srv.URL }, Secret: func() string { return "s3" }}
	if err := w.Send(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	g := (*all)[0]
	mac := hmac.New(sha256.New, []byte("s3"))
	mac.Write(g.body)
	if g.header.Get("X-Db-Branch-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) || g.header.Get("X-Db-Branch-Event") != "branch.expiring" {
		t.Errorf("headers %v", g.header)
	}
	var back notify.Notice
	if json.Unmarshal(g.body, &back) != nil || back.Owner != "a@x.io" {
		t.Errorf("body %s", g.body)
	}
	unsigned := notifier.Webhook{URL: func() string { return srv.URL }, Secret: func() string { return "" }}
	_ = unsigned.Send(context.Background(), n)
	if (*all)[1].header.Get("X-Db-Branch-Signature") != "" {
		t.Error("signed without a secret")
	}
}

func TestSend_送り先の失敗を伝える(t *testing.T) {
	srv, _ := server(t, 500)
	for _, s := range []notify.Sender{
		notifier.Slack{URL: func() string { return srv.URL }},
		notifier.Webhook{URL: func() string { return srv.URL }, Secret: func() string { return "" }},
		notifier.Webhook{URL: func() string { return "http://127.0.0.1:1" }, Secret: func() string { return "" }},
		notifier.Webhook{URL: func() string { return "://bad" }, Secret: func() string { return "" }},
	} {
		if err := s.Send(context.Background(), n); err == nil {
			t.Errorf("%T: no error", s)
		}
	}
	if err := (notifier.Webhook{URL: func() string { return "" }}).Send(context.Background(), n); err != nil {
		t.Errorf("off: %v", err)
	}
}
