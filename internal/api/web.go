package api

import (
	"embed"
	"net/http"
)

//go:embed web
var web embed.FS

// The watch page loads only its own script and style, and the script sets every task text with
// textContent, so the policy has no inline allowance.
const pageCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func (s *Server) webRoutes() {
	page := file("web/watch.html", "text/html; charset=utf-8")
	s.mux.Handle("GET /{$}", page)
	s.mux.Handle("GET /tasks/{id}", page)
	s.mux.Handle("GET /assets/watch.js", file("web/watch.js", "text/javascript; charset=utf-8"))
	s.mux.Handle("GET /assets/watch.css", file("web/watch.css", "text/css; charset=utf-8"))
}

func file(name, kind string) http.Handler {
	b, err := web.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Content-Type", kind)
		h.Set("Content-Security-Policy", pageCSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-cache")
		_, _ = w.Write(b)
	})
}
