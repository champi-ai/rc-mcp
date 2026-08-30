package admin

import (
	"embed"
	"io/fs"
	"net/http"
)

// uiFS embeds the admin web UI's static assets (Section 19: "served as
// static assets... on the same loopback-only admin port as the existing
// admin API -- never exposed through nginx"). Embedding keeps the UI part
// of the single rc-mcp-server binary; no separate deploy step.
//
//go:embed ui/index.html
var uiFS embed.FS

// uiHandler serves the admin web UI at "/". It is mounted on the same mux
// as the JSON admin API and wrapped by the same loopbackOnly guard in
// API.Handler, so it carries no separate authorization boundary.
func uiHandler() http.Handler {
	sub, err := fs.Sub(uiFS, "ui")
	if err != nil {
		// uiFS is compiled in via go:embed; a failure here means the
		// embed itself is broken, which build-time embedding should
		// already have caught. Fall back to a 500 rather than panicking
		// a running server.
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "admin UI assets unavailable", http.StatusInternalServerError)
		})
	}
	return http.FileServer(http.FS(sub))
}
