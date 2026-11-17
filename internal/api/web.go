package api

import (
	"embed"
	"io/fs"
	"net/http"
)

// web holds the plain watch page, which always ships, and web/app, the single-page UI that
// `make web` builds. web/app is not committed, so a build without Node serves the watch page.
//
//go:embed web
var web embed.FS

// The pages load only their own scripts and styles, and set every task text as text, so the
// policy has no inline allowance.
const pageCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// spaPaths are the routes the single-page UI answers with its index page.
var spaPaths = []string{"GET /{$}", "GET /tasks/{id}", "GET /workflows/{name}"}

func (s *Server) webRoutes() {
	root := s.cfg.Web
	if root == nil {
		root, _ = fs.Sub(web, "web")
	}
	if app, err := fs.Sub(root, "app"); err == nil {
		if _, err := fs.Stat(app, "index.html"); err == nil {
			s.appRoutes(app)
			return
		}
	}
	page := file(root, "watch.html", "text/html; charset=utf-8")
	s.mux.Handle("GET /{$}", page)
	s.mux.Handle("GET /tasks/{id}", page)
	s.mux.Handle("GET /assets/watch.js", file(root, "watch.js", "text/javascript; charset=utf-8"))
	s.mux.Handle("GET /assets/watch.css", file(root, "watch.css", "text/css; charset=utf-8"))
}

// appRoutes serves the built UI: its files under /ui/, and its index page on the UI's own paths.
func (s *Server) appRoutes(app fs.FS) {
	index := file(app, "index.html", "text/html; charset=utf-8")
	for _, p := range spaPaths {
		s.mux.Handle(p, index)
	}
	files := http.StripPrefix("/ui/", http.FileServerFS(app))
	s.mux.Handle("GET /ui/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", pageCSP)
		h.Set("X-Content-Type-Options", "nosniff")
		files.ServeHTTP(w, r)
	}))
}

func file(fsys fs.FS, name, kind string) http.Handler {
	b, err := fs.ReadFile(fsys, name)
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
