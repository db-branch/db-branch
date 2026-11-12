package web_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/web"
)

func TestHandler_知らないパスでもindexを返す(t *testing.T) {
	for _, p := range []string{"/", "/branches/feature-x", "/index.html"} {
		rec := httptest.NewRecorder()
		web.Handler().ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "db-branch") || rec.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: %d %q %q", p, rec.Code, rec.Header().Get("Cache-Control"), rec.Body.String())
		}
	}
}
