package api_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestTheWatchPageIsServedWithAStrictPolicy(t *testing.T) {
	g := newRig(t)
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
