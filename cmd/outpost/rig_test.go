package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/IniZio/outpost/api/client"
	"github.com/IniZio/outpost/internal/secrets"
)

// newDB makes a database of the test's own on OUTPOST_TEST_DATABASE_URL, or skips.
func newDB(t *testing.T) string {
	t.Helper()
	admin := os.Getenv("OUTPOST_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("OUTPOST_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "outpost_c_" + hex.EncodeToString(b[:])
	if _, err := conn.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `DROP DATABASE `+name+` WITH (FORCE)`)
		_ = conn.Close(context.WithoutCancel(ctx))
	})
	u, _ := url.Parse(admin)
	u.Path = "/" + name
	return u.String()
}

// live is one serve process in the test, on a free port.
type live struct {
	t    *testing.T
	env  map[string]string
	url  string
	stop context.CancelFunc
	done chan error
}

func startServer(t *testing.T, env map[string]string) *live {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + ln.Addr().String()
	l := &live{t: t, env: map[string]string{
		"OUTPOST_DATABASE_URL": newDB(t), "OUTPOST_SECRET_KEY": secrets.NewKey(), "OUTPOST_BASE_IMAGE": "base:1",
		"OUTPOST_PUBLIC_URL": base, "OUTPOST_AGENT_URL": "ws://" + ln.Addr().String() + "/machines/link",
		"OUTPOST_ALLOWED_LOGINS": "ana", "OUTPOST_DRAIN": "5s",
	}, url: base, done: make(chan error, 1)}
	for k, v := range env {
		l.env[k] = v
	}
	cfg, err := loadConfig(l.getenv)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s, err := newServer(ctx, cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	l.stop = cancel
	go func() { l.done <- s.serve(ctx, ln) }()
	t.Cleanup(l.shutdown)
	return l
}

func (l *live) getenv(k string) string { return l.env[k] }

func (l *live) shutdown() {
	if l.stop == nil {
		return
	}
	l.stop()
	l.stop = nil
	select {
	case err := <-l.done:
		if err != nil {
			l.t.Errorf("serve: %v", err)
		}
	case <-time.After(20 * time.Second):
		l.t.Error("serve did not drain")
	}
}

// admin runs an admin command against the server's database and returns what it printed.
func (l *live) admin(stdin string, args ...string) string {
	l.t.Helper()
	var out bytes.Buffer
	if err := run(context.Background(), args, l.getenv, strings.NewReader(stdin), &out); err != nil {
		l.t.Fatalf("outpost %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(out.String())
}

func (l *live) client(login string) *client.Client {
	c := client.New(l.url, l.admin("", "token", "create", login))
	c.Retry = 50 * time.Millisecond
	return c
}

func testCtx(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}
