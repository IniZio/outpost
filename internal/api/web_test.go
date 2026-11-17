package api_test

import (
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/IniZio/outpost/internal/api"
)

// watchFS is the watch page's files alone, whether or not `make web` has built the UI beside them.
func watchFS(t *testing.T) fstest.MapFS {
	t.Helper()
	m := fstest.MapFS{}
	for _, n := range []string{"watch.html", "watch.js", "watch.css"} {
		b, err := os.ReadFile("web/" + n)
		if err != nil {
			t.Fatal(err)
		}
		m[n] = &fstest.MapFile{Data: b}
	}
	return m
}

func TestTheBuiltUIIsServedOnItsPathsAndFilesOnly(t *testing.T) {
	m := watchFS(t)
	m["app/index.html"] = &fstest.MapFile{Data: []byte("<title>ui</title>")}
	m["app/assets/x.js"] = &fstest.MapFile{Data: []byte("console.log(1)")}
	g := newRig(t, func(c *api.Config) { c.Web = m })
	for path, want := range map[string]int{"/": 200, "/tasks/3": 200, "/workflows/dep-check": 200, "/ui/assets/x.js": 200, "/ui/nope.js": 404, "/assets/watch.js": 404} {
		res, err := http.Get(g.srv.URL + path) //nolint:noctx // a test against httptest
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != want || want == 200 && !strings.Contains(res.Header.Get("Content-Security-Policy"), "script-src 'self'") {
			t.Errorf("%s: %d", path, res.StatusCode)
		}
	}
}

func TestTheWatchPageIsServedWithAStrictPolicy(t *testing.T) {
	g := newRig(t, func(c *api.Config) { c.Web = watchFS(t) })
	for path, want := range map[string]string{
		"/":                 "/assets/watch.js",
		"/tasks/7":          "/assets/watch.js",
		"/assets/watch.js":  "textContent",
		"/assets/watch.css": "pre.screen",
	} {
		res, err := http.Get(g.srv.URL + path) //nolint:noctx // a test against httptest
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		csp := res.Header.Get("Content-Security-Policy")
		if res.StatusCode != http.StatusOK || !strings.Contains(string(body), want) || !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe") {
			t.Errorf("%s: %d %q", path, res.StatusCode, csp)
		}
	}
	if strings.Contains(string(mustRead(t, g.srv.URL+"/assets/watch.js")), "innerHTML") {
		t.Error("the page script never writes HTML")
	}
}

func mustRead(t *testing.T, url string) []byte {
	t.Helper()
	res, err := http.Get(url) //nolint:noctx // a test against httptest
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return b
}
