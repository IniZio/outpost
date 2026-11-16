package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetriesAStandbyThenReportsTheErrorShape(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"not_leading","message":"later"}`))
		case 2:
			_, _ = w.Write([]byte(`{"login":"ana","act":true}`))
		default:
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("upstream down"))
		}
	}))
	defer srv.Close()
	c := New(srv.URL+"/", "tok")
	c.Retry = time.Millisecond
	ctx := context.Background()
	if me, err := c.Me(ctx); err != nil || me.Login != "ana" {
		t.Fatalf("%+v %v", me, err)
	}
	_, err := c.Me(ctx)
	var e *Error
	if !errors.As(err, &e) || e.Status != http.StatusBadGateway || e.Code != "http" || e.Message != "upstream down" {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(e.Error(), "502") {
		t.Fatal(e.Error())
	}
}

func TestReadEventsSkipsWhatItHasSeen(t *testing.T) {
	stream := ": keep-alive\n\nid: 1\nevent: entry\ndata: {\"seq\":1,\"kind\":\"queued\"}\n\n" +
		"id: 2\nevent: entry\ndata: {\"seq\":2,\"kind\":\"phase\",\"text\":\"Cloning\"}\n\ndata: junk\n\n"
	var got []string
	last, err := readEvents(strings.NewReader(stream), 1, func(e Entry) error {
		got = append(got, e.Text)
		return nil
	})
	if err != nil || last != 2 || len(got) != 1 || got[0] != "Cloning" {
		t.Fatalf("%d %v %v", last, got, err)
	}
	if (Task{Status: "stopped"}).Ended() != true || (Task{Status: "waiting"}).Ended() {
		t.Fatal("Ended")
	}
}
