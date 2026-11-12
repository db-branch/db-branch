package httpapi_test

import (
	"context"
	"errors"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/licensing"
	"github.com/db-branch/db-branch/pkg/ext/license"
)

type fakeLicensing struct {
	applied string
	stored  string
	trial   bool
}

func (f *fakeLicensing) Verify(_ context.Context, key string) (licensing.Report, error) {
	switch key {
	case "good":
		return licensing.Report{License: license.License{ID: "L1", Customer: "Acme", Modules: []string{"auth"}}, State: license.Valid, Modules: []string{"auth"}}, nil
	case "old":
		return licensing.Report{License: license.License{ID: "L0"}, State: license.Expired}, nil
	case "nokey":
		return licensing.Report{}, licensing.ErrNoRoot
	}
	return licensing.Report{}, license.ErrInvalid
}

func (f *fakeLicensing) Apply(ctx context.Context, key string) (licensing.Report, error) {
	r, err := f.Verify(ctx, key)
	if err == nil && !r.State.Usable() {
		err = licensing.ErrUnusable
	}
	if err == nil {
		f.applied, f.stored = key, key
		r.Enabled = []string{"auth"}
	}
	return r, err
}

func (f *fakeLicensing) Current(ctx context.Context) (licensing.Report, bool, error) {
	if f.stored == "" {
		if f.trial {
			return licensing.Report{License: license.License{ID: "trial"}, State: license.Valid, Trial: true, All: true}, false, nil
		}
		return licensing.Report{}, false, nil
	}
	r, err := f.Verify(ctx, f.stored)
	return r, true, err
}

func TestLicense_検証は保存せず適用は保存して有効にする(t *testing.T) {
	f := newFixture(t)
	if resp, _ := f.do(t, "POST", "/v1/license/verify", map[string]any{"key": "good"}); resp.StatusCode != 501 {
		t.Errorf("without licensing: %d", resp.StatusCode)
	}
	lic := &fakeLicensing{}
	f.srv.Licensing = lic
	if _, body := f.do(t, "GET", "/v1/license", nil); body["configured"] != false || body["report"] != nil {
		t.Errorf("no key yet: %v", body)
	}
	lic.trial = true
	if _, body := f.do(t, "GET", "/v1/license", nil); body["configured"] != false || body["report"].(map[string]any)["trial"] != true {
		t.Errorf("on trial: %v", body)
	}
	resp, body := f.do(t, "POST", "/v1/license/verify", map[string]any{"key": "good"})
	if resp.StatusCode != 200 || body["state"] != "valid" || body["license"].(map[string]any)["customer"] != "Acme" || lic.applied != "" {
		t.Errorf("verify: %d %v", resp.StatusCode, body)
	}
	resp, body = f.do(t, "PUT", "/v1/license", map[string]any{"key": "good"})
	if resp.StatusCode != 200 || body["enabled"].([]any)[0] != "auth" || lic.applied != "good" {
		t.Errorf("apply: %d %v", resp.StatusCode, body)
	}
	if _, body := f.do(t, "GET", "/v1/license", nil); body["configured"] != true || body["report"].(map[string]any)["state"] != "valid" {
		t.Errorf("current: %v", body)
	}
	for _, c := range []struct {
		method, key string
		want        int
		code        string
	}{
		{"POST", "bad", 422, "invalid_license"},
		{"POST", "nokey", 501, "cannot_verify"},
		{"PUT", "old", 422, "license_unusable"},
		{"PUT", "bad", 422, "invalid_license"},
	} {
		path := "/v1/license"
		if c.method == "POST" {
			path += "/verify"
		}
		if resp, body := f.do(t, c.method, path, map[string]any{"key": c.key}); resp.StatusCode != c.want || errCode(body) != c.code {
			t.Errorf("%s %s: %d %v", c.method, c.key, resp.StatusCode, body)
		}
	}
	if resp, _ := f.do(t, "POST", "/v1/license/verify", map[string]any{"key": " "}); resp.StatusCode != 400 {
		t.Errorf("empty key: %d", resp.StatusCode)
	}
	lic.stored = "bad"
	if _, body := f.do(t, "GET", "/v1/license", nil); body["configured"] != true || body["error"] == nil {
		t.Errorf("stored key no longer valid: %v", body)
	}
}

func TestModules_ライセンスが要るモジュールに印を付ける(t *testing.T) {
	f, c := remoteFixture(t)
	f.srv.ModuleAdmin = &fakeModules{specs: []domain.ModuleSpec{{Name: "auth", Version: "1", Wanted: true}, {Name: "proxysql", Version: "1", Wanted: true}}}
	f.srv.PaidModules = []string{"auth"}
	c.status.Mode = "cluster"
	_, body := f.do(t, "GET", "/v1/modules", nil)
	paid := map[string]any{}
	for _, it := range body["items"].([]any) {
		m := it.(map[string]any)
		paid[m["name"].(string)] = m["requires_license"]
	}
	if paid["auth"] != true || paid["proxysql"] != false {
		t.Errorf("%v", paid)
	}
	_ = errors.New
}
