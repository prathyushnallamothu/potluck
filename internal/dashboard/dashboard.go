// Package dashboard serves the embedded web UI. It is a single static HTML
// file (no build step) that polls /api/pool and renders the live pool view.
package dashboard

import (
	"embed"
	"net/http"
)

//go:embed index.html
var content embed.FS

// Handler serves the dashboard at /.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		b, err := content.ReadFile("index.html")
		if err != nil {
			http.Error(w, "dashboard missing", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})
}
