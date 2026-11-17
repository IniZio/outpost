package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IniZio/outpost/api/client"
)

func setupAPI(t *testing.T, got *client.SetupFiles) cli {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/repos/acme/app/setups", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(got)
		_ = json.NewEncoder(w).Encode(client.SetupRev{Repo: "acme/app", Rev: 3, Files: []string{"a", "b"}})
	})
	mux.HandleFunc("GET /api/repos/acme/app/setups", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]client.SetupRev{{Repo: "acme/app", Rev: 3, Status: "ready", Image: "obd-setup/acme--app:r3", Log: "#9 DONE"}})
	})
	mux.HandleFunc("POST /api/repos/acme/app/setups/3/build", func(w http.ResponseWriter, r *http.Request) {
		var in client.SetupBuild
		_ = json.NewDecoder(r.Body).Decode(&in)
		_ = json.NewEncoder(w).Encode(client.SetupRev{Repo: "acme/app", Rev: 3, Status: "building", Machine: in.Machine})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return cli{t: t, env: map[string]string{"OUTPOST_SERVER": srv.URL, "OUTPOST_TOKEN": "otp_x", "OUTPOST_CLI_CONFIG": filepath.Join(t.TempDir(), "none.json")}}
}

func TestRepoSetupCommands(t *testing.T) {
	var got client.SetupFiles
	c := setupAPI(t, &got)
	dir := setupDir(t)
	if out := c.must("", "repo", "setup", "put", "acme/app", dir); !strings.Contains(out, "revision 3") {
		t.Fatalf("put: %q", out)
	}
	if len(got.Files) != 2 || got.Files["docker-bake.hcl"] != "b" || got.Files["after-clone.sh"] != "a" {
		t.Fatalf("sent %v: regular files only, no dot files or dirs", got.Files)
	}
	for args, want := range map[string]string{
		"repo setup ls acme/app":                  "obd-setup/acme--app:r3",
		"repo setup log acme/app 3":               "#9 DONE",
		"repo setup build -machine m9 acme/app 3": "on m9",
	} {
		if out := c.must("", strings.Fields(args)...); !strings.Contains(out, want) {
			t.Errorf("%s: %q", args, out)
		}
	}
	for _, args := range []string{"repo", "repo setup nope x", "repo setup build acme/app x", "repo setup log acme/app 4", "repo setup put acme/app /no/such"} {
		if _, err := c.run("", strings.Fields(args)...); err == nil {
			t.Errorf("%s should fail", args)
		}
	}
}

// setupDir is a revision's files, plus a dot file and a dir that put leaves out.
func setupDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"docker-bake.hcl": "b", "after-clone.sh": "a", ".hidden": "h"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
