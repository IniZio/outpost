package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUserReadsTheLoginOfAToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer good":
			_, _ = w.Write([]byte(`{"login":"ana","id":42,"name":"Ana Lee"}`))
		case "Bearer odd":
			_, _ = w.Write([]byte(`{}`))
		case "Bearer down":
			w.WriteHeader(http.StatusBadGateway)
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer srv.Close()
	c := Client{API: srv.URL}
	ctx := context.Background()
	if l, err := c.User(ctx, "good"); err != nil || l != "ana" {
		t.Fatalf("%q %v", l, err)
	}
	if _, err := c.User(ctx, "bad"); !errors.Is(err, ErrBadToken) {
		t.Fatalf("a refused token: %v", err)
	}
	for _, tok := range []string{"odd", "down"} {
		if _, err := c.User(ctx, tok); err == nil || errors.Is(err, ErrBadToken) {
			t.Fatalf("%s: %v", tok, err)
		}
	}
	if _, err := (Client{API: "http://127.0.0.1:1"}).User(ctx, "x"); err == nil {
		t.Fatal("an unreachable API fails")
	}
}

func TestProfileReadsTheIDAndDisplayName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"login":"ana","id":42,"name":"Ana Lee"}`))
	}))
	defer srv.Close()
	if p, err := (Client{API: srv.URL}).Profile(context.Background(), "t"); err != nil || p.ID != 42 || p.Name != "Ana Lee" {
		t.Fatalf("%+v %v", p, err)
	}
}
