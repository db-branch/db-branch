// Package web embeds the built console (web/dist) into the binary.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var dist embed.FS

// placeholder is served when the binary was built without `make web`.
const placeholder = `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>db-branch</title></head>
<body><p>db-branch: the web console was not built into this binary. Run <code>make web</code> before <code>make build</code>.</p></body></html>`

// Handler serves the console. Unknown paths get index.html so client-side
// routes (/branches/x) work on reload; hashed assets are cached for a year.
func Handler() http.Handler {
	root, _ := fs.Sub(dist, "dist")
	files := http.FileServer(http.FS(root))
	index, err := fs.ReadFile(root, "index.html")
	if err != nil {
		index = []byte(placeholder)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if _, err := fs.Stat(root, p); p == "" || p == "index.html" || err != nil {
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(index)
			return
		}
		if strings.HasPrefix(p, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		files.ServeHTTP(w, r)
	})
}
