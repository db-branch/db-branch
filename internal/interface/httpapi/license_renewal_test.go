package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/interface/httpapi"
	"github.com/db-branch/db-branch/internal/usecase/licensing"
)

type fakeRenewal struct {
	leading bool
	status  licensing.RenewalStatus
	err     error
	checks  int
}

func (f *fakeRenewal) Status(context.Context) licensing.RenewalStatus {
	if !f.leading {
		return licensing.RenewalStatus{Enabled: true, LastResult: licensing.ResultUnknown}
	}
	return f.status
}
func (f *fakeRenewal) Leading() bool { return f.leading }
func (f *fakeRenewal) CheckNow(context.Context) (licensing.RenewalStatus, error) {
	f.checks++
	return f.status, f.err
}

// leaderCluster answers as the leader node would.
type leaderCluster struct {
	fakeCluster
	reply     string
	replyCode int
	seen      []string
}

func (c *leaderCluster) Forward(node string) (http.Handler, error) {
	if c.forwardErr != nil {
		return nil, c.forwardErr
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.seen = append(c.seen, node+" "+r.Method+" "+r.URL.Path+" via="+r.Header.Get("X-Db-Branch-Forwarded"))
		w.WriteHeader(c.replyCode)
		_, _ = w.Write([]byte(c.reply))
	}), nil
}

func TestRenewal_自動更新が無い構成では状態を出さず操作は501(t *testing.T) {
	f := newFixture(t)
	f.srv.Licensing = &fakeLicensing{}
	if _, body := f.do(t, "GET", "/v1/license", nil); body["renewal"] != nil {
		t.Errorf("%v", body)
	}
	for _, m := range [][2]string{{"GET", "/v1/license/renewal"}, {"POST", "/v1/license/renew"}} {
		if resp, _ := f.do(t, m[0], m[1], nil); resp.StatusCode != 501 {
			t.Errorf("%v: %d", m, resp.StatusCode)
		}
	}
}

func TestRenewal_単一ノードでは状態を返し今すぐ確認できる(t *testing.T) {
	f := newFixture(t)
	f.srv.Licensing = &fakeLicensing{stored: "good"}
	at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	fr := &fakeRenewal{leading: true, status: licensing.RenewalStatus{Enabled: true, LastResult: licensing.ResultRenewed, LastAttempt: &at, Message: "valid until 2026-12-10"}}
	f.srv.Renewal = fr
	_, body := f.do(t, "GET", "/v1/license", nil)
	r, _ := body["renewal"].(map[string]any)
	if r["enabled"] != true || r["last_result"] != "renewed" || r["last_attempt"] != "2026-11-10T12:00:00Z" || r["message"] != "valid until 2026-12-10" {
		t.Errorf("%v", body)
	}
	if _, body := f.do(t, "GET", "/v1/license/renewal", nil); body["last_result"] != "renewed" {
		t.Errorf("%v", body)
	}
	resp, body := f.do(t, "POST", "/v1/license/renew", nil)
	if resp.StatusCode != 200 || body["last_result"] != "renewed" || fr.checks != 1 {
		t.Errorf("%d %v", resp.StatusCode, body)
	}
}

func TestRenewal_確認の失敗を状態コードに写す(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{&licensing.TooSoonError{RetryAfter: 20 * time.Second}, 429, "rate_limited"},
		{licensing.ErrAutoRenewOff, 409, "auto_renew_off"},
		{licensing.ErrNotRenewable, 409, "not_renewable"},
		{licensing.ErrInFlight, 409, "in_progress"},
		{licensing.ErrNotLeader, 503, "no_leader"},
		{licensing.ErrNoRoot, 501, "cannot_verify"},
		{errors.New("boom"), 500, ""},
	}
	for _, c := range cases {
		f := newFixture(t)
		f.srv.Licensing = &fakeLicensing{}
		f.srv.Renewal = &fakeRenewal{leading: true, err: c.err}
		resp, body := f.do(t, "POST", "/v1/license/renew", nil)
		if resp.StatusCode != c.status || c.code != "" && errCode(body) != c.code {
			t.Errorf("%v: %d %v", c.err, resp.StatusCode, body)
		}
		if c.status == 429 && resp.Header.Get("Retry-After") != "21" {
			t.Errorf("Retry-After %q", resp.Header.Get("Retry-After"))
		}
	}
}

func TestRenewal_フォロワーはリーダーに尋ねるか不明と答える(t *testing.T) {
	f := newFixture(t)
	f.srv.Licensing = &fakeLicensing{stored: "good"}
	f.srv.Renewal = &fakeRenewal{leading: false}
	c := &leaderCluster{replyCode: 200, reply: `{"enabled":true,"last_result":"not_yet","next_attempt":"2026-11-11T00:00:00Z"}`}
	c.status = httpapi.ClusterStatus{Mode: "cluster", Leader: "n2"}
	f.srv.Cluster = c

	_, body := f.do(t, "GET", "/v1/license", nil)
	r, _ := body["renewal"].(map[string]any)
	if r["last_result"] != "not_yet" || r["next_attempt"] != "2026-11-11T00:00:00Z" {
		t.Errorf("リーダーの状態: %v", body)
	}
	if len(c.seen) != 1 || c.seen[0] != "n2 GET /v1/license/renewal via=node1" {
		t.Errorf("%v", c.seen)
	}

	c.seen, c.replyCode, c.reply = nil, 429, `{"error":{"code":"rate_limited","message":"checked a moment ago"}}`
	resp, body := f.do(t, "POST", "/v1/license/renew", nil)
	if resp.StatusCode != 429 || len(c.seen) != 1 || c.seen[0] != "n2 POST /v1/license/renew via=node1" {
		t.Errorf("転送: %d %v %v", resp.StatusCode, body, c.seen)
	}
	c.seen = nil
	if resp, _ := f.do(t, "GET", "/v1/license/renewal", nil); resp.StatusCode != 429 || len(c.seen) != 1 {
		t.Errorf("GET renewal も転送する: %d", resp.StatusCode)
	}

	// リーダーに届かなければ不明と答える。
	c.forwardErr = errors.New("unreachable")
	_, body = f.do(t, "GET", "/v1/license", nil)
	if r, _ := body["renewal"].(map[string]any); r["last_result"] != "unknown" {
		t.Errorf("届かない: %v", body)
	}
	c.forwardErr = nil
	c.replyCode, c.reply = 500, `oops`
	_, body = f.do(t, "GET", "/v1/license", nil)
	if r, _ := body["renewal"].(map[string]any); r["last_result"] != "unknown" {
		t.Errorf("リーダーが失敗: %v", body)
	}
	c.replyCode, c.reply = 200, `not json`
	_, body = f.do(t, "GET", "/v1/license", nil)
	if r, _ := body["renewal"].(map[string]any); r["last_result"] != "unknown" {
		t.Errorf("壊れた応答: %v", body)
	}
}

func TestRenewal_リーダーが分からないとき自分がリーダーのとき転送されてきたとき(t *testing.T) {
	f := newFixture(t)
	f.srv.Licensing = &fakeLicensing{}
	fr := &fakeRenewal{leading: false, err: licensing.ErrNotLeader}
	f.srv.Renewal = fr
	c := &leaderCluster{replyCode: 200, reply: `{}`}
	f.srv.Cluster = c

	c.status = httpapi.ClusterStatus{Mode: "cluster", Leader: ""}
	if resp, _ := f.do(t, "POST", "/v1/license/renew", nil); resp.StatusCode != 503 {
		t.Errorf("リーダー不在: %d", resp.StatusCode)
	}
	if _, body := f.do(t, "GET", "/v1/license", nil); body["renewal"].(map[string]any)["last_result"] != "unknown" {
		t.Errorf("リーダー不在でも一覧は返す: %v", body)
	}
	c.err = errors.New("raft down")
	if resp, _ := f.do(t, "POST", "/v1/license/renew", nil); resp.StatusCode != 500 {
		t.Errorf("状態を読めない: %d", resp.StatusCode)
	}
	c.err = nil

	// 自分がリーダーのはずなのにまだ引き継いでいない間は、ここで答える。
	c.status = httpapi.ClusterStatus{Mode: "cluster", Leader: "node1"}
	if resp, _ := f.do(t, "POST", "/v1/license/renew", nil); resp.StatusCode != 503 || len(c.seen) != 0 {
		t.Errorf("自分がリーダー: %d %v", resp.StatusCode, c.seen)
	}

	// 転送されてきた要求はさらに転送しない。
	c.status = httpapi.ClusterStatus{Mode: "cluster", Leader: "n2"}
	resp, _ := send(t, f.srv.Handler(true), "POST", "/v1/license/renew", nil, func(r *http.Request) {
		r.Header.Set("X-Db-Branch-Forwarded", "n3")
		r.Header.Set("X-Db-Branch-Principal", `{"ID":"local"}`)
	})
	if resp.StatusCode != 503 || len(c.seen) != 0 {
		t.Errorf("転送された要求: %d %v", resp.StatusCode, c.seen)
	}
}
