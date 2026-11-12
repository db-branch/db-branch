package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/db-branch/db-branch/pkg/ext"
)

// ModuleAPI is how the module named module calls its own API through the
// agent (see ext.ModuleAPI): the request takes the path a client's takes,
// so it is authorized for the principal the module names, audited when it
// writes, and forwarded to the node of the branch it names.
func (s *Server) ModuleAPI(module string) ext.ModuleAPI { return moduleAPI{s: s, module: module} }

type moduleAPI struct {
	s      *Server
	module string
}

func (m moduleAPI) Call(ctx context.Context, r ext.Request) (ext.Response, error) {
	if _, ok := m.s.ExtHandlers[m.module]; !ok {
		return ext.Response{}, fmt.Errorf("module %s has no API on this node", m.module)
	}
	path := strings.TrimPrefix(r.Path, "/")
	target := "/v1/ext/" + m.module + "/" + path
	if r.RawQuery != "" {
		target += "?" + r.RawQuery
	}
	method := r.Method
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(withPrincipal(ctx, r.Principal), method, target, bytes.NewReader(r.Body))
	if err != nil {
		return ext.Response{}, err
	}
	req.SetPathValue("name", m.module)
	req.SetPathValue("path", path)
	rec := &recorder{header: http.Header{}, status: http.StatusOK}
	m.s.extAPI(rec, req)
	return ext.Response{Status: rec.status, ContentType: rec.header.Get("Content-Type"), Body: rec.body.Bytes()}, nil
}
