package main

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/IniZio/outpost/api/client"
)

func TestConfigDefaultsAndRefusals(t *testing.T) {
	cfg, err := loadConfig(func(k string) string { return map[string]string{"OUTPOST_DATABASE_URL": "postgres://x"}[k] })
	if err != nil || cfg.Listen != ":7400" || cfg.MaxRunning != 2 || !cfg.Migrate || cfg.PublicURL != "http://localhost:7400" {
		t.Fatalf("%+v %v", cfg, err)
	}
	if err := cfg.serveReady(); err == nil || !strings.Contains(err.Error(), "OUTPOST_SECRET_KEY") {
		t.Fatalf("serve needs a key and an image: %v", err)
	}
	bad := map[string]string{"OUTPOST_MAX_RUNNING": "0", "OUTPOST_DRAIN": "soon", "OUTPOST_LOG": "loud", "OUTPOST_AGENT_URL": "::"}
	_, err = loadConfig(func(k string) string { return bad[k] })
	for _, want := range []string{"MAX_RUNNING", "DRAIN", "LOG", "AGENT_URL", "DATABASE_URL"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want a complaint about %s: %v", want, err)
		}
	}
}

func TestCommandsOutsideTheDatabase(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"key"}, func(string) string { return "" }, nil, &out); err != nil || len(strings.TrimSpace(out.String())) != 44 {
		t.Fatalf("%q %v", out.String(), err)
	}
	for _, args := range [][]string{nil, {"serve"}} {
		if err := run(context.Background(), args, func(string) string { return "" }, nil, &out); err == nil {
			t.Fatalf("%v needs more", args)
		}
	}
}

func TestServeAnswersAndDrains(t *testing.T) {
	l := startServer(t, nil)
	ctx := testCtx(t, 30*time.Second)
	for _, path := range []string{"/healthz", "/readyz"} {
		res, err := http.Get(l.url + path)
		if err != nil || res.StatusCode != http.StatusOK {
			t.Fatalf("%s: %v %v", path, err, res)
		}
		_ = res.Body.Close()
	}
	adminAndAPI(ctx, t, l)
	l.shutdown()
}

func adminAndAPI(ctx context.Context, t *testing.T, l *live) {
	ana := l.client("ana")
	task, err := ana.CreateTask(ctx, client.NewTask{Repo: "o/r", Prompt: "p"})
	if err != nil || task.Status != "queued" {
		t.Fatalf("%+v %v", task, err)
	}
	if st, err := ana.Status(ctx); err != nil || !st.Leading {
		t.Fatalf("%+v %v", st, err)
	}
	l.admin("", "repo", "set", "o/r", "ready:1", "m1", "warm")
	if rs, err := ana.Repos(ctx); err != nil || len(rs) != 1 || rs[0].ReadyImage != "ready:1" {
		t.Fatalf("%+v %v", rs, err)
	}
	l.admin("gho_x\n", "secret", "set", "ana", "github")
	if err := ana.SetSecret(ctx, "claude", "sk"); err != nil {
		t.Fatal(err)
	}
	if err := ana.SetSecret(ctx, "Bad Name", "sk"); err == nil {
		t.Fatal("a bad secret name is refused")
	}
	res, err := http.Get(l.url + "/machines/link")
	if err != nil || res.StatusCode == http.StatusOK {
		t.Fatalf("a link without a websocket upgrade is refused: %v %v", err, res)
	}
	_ = res.Body.Close()
}
