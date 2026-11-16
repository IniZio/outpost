package ops_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/link"
	"github.com/IniZio/outpost/internal/link/linktest"
	"github.com/IniZio/outpost/internal/ops"
)

func agentRegistry() *link.Registry {
	r := link.NewRegistry(ops.Catalogue(), link.ServedByAgent, 0)
	link.Handle(r, ops.Ping, func(_ context.Context, _ link.Meta, q ops.PingReq) (ops.PingResp, error) {
		return ops.PingResp(q), nil
	})
	link.Handle(r, ops.Status, func(context.Context, link.Meta, struct{}) (ops.StatusResp, error) {
		return ops.StatusResp{Build: "test", Role: domain.RoleSandbox}, nil
	})
	return r
}

func serverRegistry(lines chan<- ops.ReportLine) *link.Registry {
	r := link.NewRegistry(ops.Catalogue(), link.ServedByServer, 0)
	link.HandleResumable(r, ops.Report, func(context.Context, link.Meta, struct{}) (struct{}, link.Bytes, error) {
		return struct{}{}, func(ctx context.Context, s link.Stream) error {
			sc := bufio.NewScanner(s)
			for sc.Scan() {
				var l ops.ReportLine
				if json.Unmarshal(sc.Bytes(), &l) == nil {
					lines <- l
				}
			}
			return nil
		}, nil
	})
	return r
}

func TestCatalogueIsServedEndToEnd(t *testing.T) {
	lines := make(chan ops.ReportLine, 4)
	rig := linktest.New(t, linktest.Options{Server: serverRegistry(lines), Agent: agentRegistry()})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pong, err := link.Call(ctx, rig.Hub.Link(rig.Machine), ops.Ping, ops.PingReq{Token: "abc"})
	if err != nil || pong.Token != "abc" {
		t.Fatalf("ping: %v %v", pong, err)
	}
	st, err := link.Call(ctx, rig.Hub.Link(rig.Machine), ops.Status, struct{}{})
	if err != nil || st.Role != domain.RoleSandbox {
		t.Fatalf("status: %v %v", st, err)
	}

	var opens atomic.Int32
	go func() {
		_ = link.Hold(ctx, rig.Agent.Server(), ops.Report, struct{}{}, func(_ context.Context, _ struct{}, s link.Stream) {
			if opens.Add(1) == 1 {
				_ = json.NewEncoder(s).Encode(ops.ReportLine{MemFree: 42})
			}
			_, _ = io.Copy(io.Discard, s)
		})
	}()
	select {
	case l := <-lines:
		if l.MemFree != 42 {
			t.Fatalf("report: %+v", l)
		}
	case <-ctx.Done():
		t.Fatal("the server never heard the agent's report")
	}
}

func TestEveryOpMustBeServedByItsSide(t *testing.T) {
	for _, c := range []struct {
		side link.Side
		want string
	}{
		{link.ServedByAgent, "machine.ping"},
		{link.ServedByServer, "machine.report"},
	} {
		err := link.NewRegistry(ops.Catalogue(), c.side, 0).Check()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("an empty registry must name %s: %v", c.want, err)
		}
	}
	if err := agentRegistry().Check(); err != nil {
		t.Errorf("the agent registry in this test serves everything: %v", err)
	}
	if err := serverRegistry(nil).Check(); err != nil {
		t.Errorf("the server registry in this test serves everything: %v", err)
	}
}
