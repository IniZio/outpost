package docker

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

// host is a scripted docker CLI: answer maps a command to its stdout or error, and every call is
// logged.
type host struct {
	mu     sync.Mutex
	calls  []string
	answer func(cmd string) (string, error)
	stream string
}

func missing(what string) error {
	return &CmdError{Code: 1, Stderr: "Error: No such " + what}
}

func (h *host) Output(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
	cmd := strings.Join(args, " ")
	h.mu.Lock()
	h.calls = append(h.calls, cmd)
	h.mu.Unlock()
	out, err := h.answer(cmd)
	return []byte(out), err
}

func (h *host) Start(_ context.Context, args ...string) (io.ReadWriteCloser, error) {
	h.mu.Lock()
	h.calls = append(h.calls, strings.Join(args, " "))
	s := h.stream
	h.stream = ""
	h.mu.Unlock()
	return rwc{strings.NewReader(s)}, nil
}

type rwc struct{ io.Reader }

func (rwc) Write(b []byte) (int, error) { return len(b), nil }
func (rwc) Close() error                { return nil }

func (h *host) ran(prefix string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, c := range h.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

const running = `[{"Name":"/ob-t5-g2","Config":{"Labels":{"dev.outpost.box.task":"5","dev.outpost.box.gen":"2","dev.outpost.box.repo":"o/r"}},"State":{"Status":"running","Running":true}}]`

func boxJSON(gen, status string) string {
	r := strings.ReplaceAll(running, `box.gen":"2"`, `box.gen":"`+gen+`"`)
	r = strings.ReplaceAll(r, "g2", "g"+gen)
	return strings.ReplaceAll(r, `"Status":"running","Running":true`, `"Status":"`+status+`","Running":`+boolStr(status == "running"))
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// world answers like a host with the given containers of task 5 (by gen) and no volumes.
func world(boxes map[string]string, volumes bool) func(string) (string, error) {
	return func(cmd string) (string, error) {
		switch {
		case strings.HasPrefix(cmd, "ps -aq"):
			return strings.Join(slices.Sorted(keys(boxes)), "\n"), nil
		case strings.HasPrefix(cmd, "container inspect ob-t5-g"):
			gen := strings.TrimPrefix(cmd, "container inspect ob-t5-g")
			if s, ok := boxes[gen]; ok {
				return "[" + strings.Trim(boxJSON(gen, s), "[]") + "]", nil
			}
			return "", missing("container")
		case strings.HasPrefix(cmd, "container inspect"):
			var all []string
			for _, g := range slices.Sorted(keys(boxes)) {
				all = append(all, strings.Trim(boxJSON(g, boxes[g]), "[]"))
			}
			return "[" + strings.Join(all, ",") + "]", nil
		case strings.HasPrefix(cmd, "volume inspect") && !volumes:
			return "", missing("volume")
		case strings.HasPrefix(cmd, "network inspect"):
			return "1280\n", nil
		}
		return "", nil
	}
}

func keys(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func provider(t *testing.T, h *host) *Docker {
	t.Helper()
	d, err := New(Config{Tools: "ob-tools:1", MTU: 1280, Run: h, Poll: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func spec(gen domain.BoxGen, fresh bool) ports.BoxSpec {
	return ports.BoxSpec{
		Key: ports.BoxKey{Task: 5, Gen: gen}, Image: "ob-base:1", Repo: "o/r", Fresh: fresh,
		Env: map[string]string{"Z": "1", "A": "2"},
	}
}

func TestUpCreatesAFreshBoxOnLabelledVolumes(t *testing.T) {
	h := &host{}
	created := false
	h.answer = func(cmd string) (string, error) {
		if strings.HasPrefix(cmd, "create ") {
			created = true
		}
		if strings.HasPrefix(cmd, "ps -aq") && !created {
			return "", nil
		}
		return world(map[string]string{"2": "running"}, false)(cmd)
	}
	info, err := provider(t, h).Up(context.Background(), spec(2, true), nil)
	if err != nil || !info.Running || info.Key.Gen != 2 || info.Repo != "o/r" {
		t.Fatalf("up: %+v %v", info, err)
	}
	if v := h.ran("volume create --label dev.outpost.box.task=5 ob-t5-"); len(v) != 4 {
		t.Fatalf("four labelled volumes: %v", v)
	}
	checkCreate(t, h.ran("create "))
	if len(h.ran("start ob-t5-g2")) != 1 || len(h.ran("exec ob-t5-g2 test -e /run/box/ready")) != 1 {
		t.Fatal("the new box is started and waited for")
	}
}

func checkCreate(t *testing.T, c []string) {
	t.Helper()
	if len(c) != 1 || !strings.Contains(c[0], "type=image,src=ob-tools:1,dst=/opt/outpost") ||
		!strings.Contains(c[0], "--env OUTPOST_MTU=1280 --entrypoint /opt/outpost/bin/box-init --env A=2 --env Z=1 ob-base:1") ||
		strings.Contains(c[0], "outpost.task") {
		t.Fatalf("create: %v", c)
	}
}

func TestUpOnAResumeNeedsTheVolumes(t *testing.T) {
	h := &host{answer: world(nil, false)}
	if _, err := provider(t, h).Up(context.Background(), spec(2, false), nil); !errors.Is(err, ports.ErrNoVolumes) {
		t.Fatalf("got %v", err)
	}
	if len(h.ran("create ")) != 0 || len(h.ran("volume create")) != 0 {
		t.Fatal("a resume never creates volumes")
	}
}

func TestUpReplacesOlderAndYieldsToNewerGenerations(t *testing.T) {
	h := &host{answer: world(map[string]string{"3": "exited"}, true)}
	if _, err := provider(t, h).Up(context.Background(), spec(2, false), nil); !errors.Is(err, ports.ErrSuperseded) {
		t.Fatalf("a newer gen wins: %v", err)
	}
	h = &host{answer: world(map[string]string{"1": "exited", "2": "exited"}, true)}
	if _, err := provider(t, h).Up(context.Background(), spec(2, false), nil); err != nil {
		t.Fatal(err)
	}
	if len(h.ran("rm -f ob-t5-g1")) != 1 || len(h.ran("create ")) != 0 || len(h.ran("start ob-t5-g2")) != 1 {
		t.Fatalf("the old gen goes, the stopped one restarts: %v", h.calls)
	}
}

func TestUpPullsRegistryImagesButNeverTemplates(t *testing.T) {
	h := &host{answer: func(cmd string) (string, error) {
		if strings.HasPrefix(cmd, "image inspect") && !strings.Contains(cmd, "ob-tools") {
			return "", missing("image")
		}
		return world(nil, true)(cmd)
	}}
	d := provider(t, h)
	s := spec(1, true)
	s.Image = "ob-ready/o--r:t5-g1"
	if _, err := d.Up(context.Background(), s, nil); !errors.Is(err, ports.ErrNoImage) {
		t.Fatalf("a missing template is refused: %v", err)
	}
	var phases []string
	s.Image = "debian:13"
	_, _ = d.Up(context.Background(), s, func(p, _ string) { phases = append(phases, p) })
	if len(h.ran("pull --quiet debian:13")) != 1 || len(phases) != 1 || len(h.ran("pull --quiet ob-ready")) != 0 {
		t.Fatalf("pulls %v, phases %v", h.ran("pull"), phases)
	}
}

func TestUpFailsFastWhenTheBoxStops(t *testing.T) {
	h := &host{answer: func(cmd string) (string, error) {
		if strings.HasPrefix(cmd, "exec ") {
			return "", &CmdError{Code: 1}
		}
		if strings.HasPrefix(cmd, "logs") {
			return "box-init: boom", nil
		}
		return world(map[string]string{"1": "exited"}, true)(cmd)
	}}
	_, err := provider(t, h).Up(context.Background(), spec(1, false), nil)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("got %v", err)
	}
}

func TestStopLeavesANewerGenerationAlone(t *testing.T) {
	h := &host{answer: world(map[string]string{"3": "running"}, true)}
	d := provider(t, h)
	if err := d.Stop(context.Background(), ports.BoxKey{Task: 5, Gen: 2}); err != nil || len(h.ran("stop")) != 0 {
		t.Fatalf("%v %v", err, h.ran("stop"))
	}
	if err := d.Stop(context.Background(), ports.BoxKey{Task: 5, Gen: 3}); err != nil || len(h.ran("stop --time 10 ob-t5-g3")) != 1 {
		t.Fatalf("%v %v", err, h.calls)
	}
}

func TestReleaseRemovesEveryGenerationAndVolume(t *testing.T) {
	h := &host{answer: world(map[string]string{"1": "exited", "2": "running"}, true)}
	if err := provider(t, h).Release(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	if len(h.ran("rm -f --volumes ob-t5-g")) != 2 || len(h.ran("volume rm -f ob-t5-")) != 4 {
		t.Fatalf("%v", h.calls)
	}
}

func TestInspectWaitsForASettledBox(t *testing.T) {
	n := 0
	h := &host{answer: func(cmd string) (string, error) {
		n++
		if n < 3 {
			return boxJSON("2", "restarting"), nil
		}
		return boxJSON("2", "exited"), nil
	}}
	info, err := provider(t, h).Inspect(context.Background(), ports.BoxKey{Task: 5, Gen: 2})
	if err != nil || info.Running || n != 3 {
		t.Fatalf("%+v %v after %d looks", info, err, n)
	}
	h = &host{answer: world(nil, true)}
	if _, err := provider(t, h).Inspect(context.Background(), ports.BoxKey{Task: 5, Gen: 9}); !errors.Is(err, ports.ErrNoBox) {
		t.Fatalf("got %v", err)
	}
}

func TestEventsReplayUnseenRemovalsThenFollow(t *testing.T) {
	h := &host{}
	destroy := `{"Action":"destroy","Actor":{"Attributes":{"dev.outpost.box.task":"4","dev.outpost.box.gen":"1"}},"time":10}`
	h.answer = func(cmd string) (string, error) {
		if strings.HasPrefix(cmd, "events") {
			return destroy + "\n" + strings.ReplaceAll(destroy, `"task":"4"`, `"task":"5"`) + "\n", nil
		}
		return world(map[string]string{"1": "running"}, true)(cmd)
	}
	h.stream = `{"Action":"die","Actor":{"Attributes":{"dev.outpost.box.task":"5","dev.outpost.box.gen":"1"}},"time":11}` + "\n" +
		`{"Action":"exec_start: sh","Actor":{"Attributes":{"dev.outpost.box.task":"5","dev.outpost.box.gen":"1"}},"time":12}` + "\n"
	ctx, cancel := context.WithCancel(context.Background())
	var got []ports.BoxChange
	err := provider(t, h).Events(ctx, func(c ports.BoxChange) {
		got = append(got, c)
		if len(got) == 2 {
			cancel()
		}
	})
	if err != nil || len(got) != 2 {
		t.Fatalf("%v %+v", err, got)
	}
	if got[0].Info.Key.Task != 4 || got[0].Kind != domain.BoxDied || got[0].Info.Running {
		t.Fatalf("only the removal with no box left replays: %+v", got[0])
	}
	if got[1].Kind != domain.BoxDied || !got[1].Info.Running || got[1].Info.Key.Gen != 1 {
		t.Fatalf("a live event carries the settled facts: %+v", got[1])
	}
}

func TestNetworkKeepsAnMTUThatMatches(t *testing.T) {
	h := &host{answer: world(nil, true)}
	d := provider(t, h)
	_, _ = d.Up(context.Background(), spec(1, true), nil)
	if len(h.ran("network rm")) != 0 || len(h.ran("network create")) != 0 {
		t.Fatalf("a matching network stays: %v", h.calls)
	}
}

func TestNetworkIsRecreatedForANewMTU(t *testing.T) {
	h := &host{answer: func(cmd string) (string, error) {
		if strings.HasPrefix(cmd, "network inspect") {
			return "1500\n", nil
		}
		return world(nil, true)(cmd)
	}}
	_, _ = provider(t, h).Up(context.Background(), spec(1, true), nil)
	if c := h.ran("network create"); len(h.ran("network rm outpost-boxes")) != 1 || len(c) != 1 ||
		!strings.Contains(c[0], "com.docker.network.driver.mtu=1280 outpost-boxes") {
		t.Fatalf("%v", h.calls)
	}
}

func TestAMissingNetworkIsCreated(t *testing.T) {
	h := &host{answer: func(cmd string) (string, error) {
		if strings.HasPrefix(cmd, "network inspect") {
			return "", &CmdError{Code: 1, Stderr: "Error response from daemon: network outpost-boxes not found"}
		}
		return world(nil, true)(cmd)
	}}
	_, _ = provider(t, h).Up(context.Background(), spec(1, true), nil)
	if len(h.ran("network create")) != 1 || len(h.ran("network rm")) != 0 {
		t.Fatalf("%v", h.calls)
	}
}
