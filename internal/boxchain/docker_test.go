//go:build docker

package boxchain_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/hc"
	"github.com/IniZio/outpost/internal/machine"
	"github.com/IniZio/outpost/internal/ports"
	"github.com/IniZio/outpost/internal/ports/portstest"
	"github.com/IniZio/outpost/internal/sandbox/docker"
)

// world is one run's Docker namespace: every name starts obit-<id>, every label is under
// dev.outpost.box.it<id>, and every image carries dev.outpost.it=<id>, so cleanup touches nothing
// else on the host.
type world struct {
	id, label, network, tools, box string
	sb                             *docker.Docker
	clones                         atomic.Int32
}

func run(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func newWorld(t *testing.T) *world {
	t.Helper()
	id := os.Getenv("OUTPOST_IT_ID")
	if id == "" {
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		id = hex.EncodeToString(b)
	}
	w := &world{id: id, label: "dev.outpost.box.it" + id, network: "obit-" + id, tools: "obit-tools:" + id, box: "obit-box:" + id}
	t.Cleanup(func() { w.sweep(t) })
	w.build(t)
	sb, err := docker.New(docker.Config{Prefix: "obit-" + id, Label: w.label, Network: w.network, Tools: w.tools, Poll: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	w.sb = sb
	return w
}

func (w *world) build(t *testing.T) {
	if _, err := exec.LookPath("hc"); err != nil {
		t.Skip("hc is not installed")
	}
	ctx := t.TempDir()
	files, _ := filepath.Glob("../../images/tools/*")
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ctx, filepath.Base(f)), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run(t, "build", "-q", "--target", "tools", "--label", "dev.outpost.it="+w.id, "-t", w.tools, ctx)
	run(t, "build", "-q", "--label", "dev.outpost.it="+w.id, "-t", w.box, "testdata/box")
}

// sweep removes what this run made, by its own labels and names only.
func (w *world) sweep(t *testing.T) {
	for _, q := range [][]string{
		{"ps", "-aq", "--filter", "label=" + w.label + ".task"},
		{"volume", "ls", "-q", "--filter", "label=" + w.label + ".task"},
		{"image", "ls", "-q", "--filter", "label=" + w.label + ".task"},
		{"image", "ls", "-q", "--filter", "label=dev.outpost.it=" + w.id},
	} {
		out, _ := exec.Command("docker", q...).Output()
		ids := strings.Fields(string(out))
		if len(ids) == 0 {
			continue
		}
		rm := map[string][]string{"ps": {"rm", "-f"}, "volume": {"volume", "rm", "-f"}, "image": {"image", "rm", "-f"}}[q[0]]
		_ = exec.Command("docker", append(rm, ids...)...).Run()
	}
	_ = exec.Command("docker", "network", "rm", w.network).Run()
}

func (w *world) name(tg ports.Target) string {
	return fmt.Sprintf("obit-%s-t%d-g%d", w.id, tg.Task, tg.Gen)
}

// chain joins the adapters. With needCA the clone refuses to run unless the bundle trusts testCA.
func (w *world) chain(t *testing.T, needCA bool) portstest.BoxesRig {
	clone := "git clone -q /srv/main.git \"$0\""
	if needCA {
		clone = trusted + " && " + clone
	}
	c := newChain(t, w.sb, func(cfg *machine.Config) {
		cfg.Clone = func(_ domain.RepoName, dir, _ string) []string {
			w.clones.Add(1)
			return []string{"sh", "-c", clone, dir}
		}
	})
	rp := c.rp
	rp.Image = w.box
	rp.Report = func(tg ports.Target, s domain.HCStatus) {
		if err := rp.Boxes.Type(context.Background(), tg, words[s]); err != nil {
			t.Errorf("report: %v", err)
		}
	}
	rp.Touch = func(tg ports.Target, p string) { run(t, "exec", w.name(tg), "touch", p) }
	return rp
}

func TestDockerMeetsTheBoxesContract(t *testing.T) {
	w := newWorld(t)
	t.Run("contract", func(t *testing.T) {
		portstest.BoxesContract(t, func(t *testing.T) portstest.BoxesRig { return w.chain(t, false) })
	})
}

func TestDockerBoxLifecycle(t *testing.T) {
	w := newWorld(t)
	rp := w.chain(t, true)
	tg := ports.Target{Task: 1, Machine: rp.Machine, Run: 1, Gen: 1}
	signals := watch(t, rp)
	startFresh(t, w, rp, tg)
	expect(t, signals, domain.AgentReport{Run: 1, Status: domain.HCIdle})
	rp.Report(tg, domain.HCWorking)
	expect(t, signals, domain.AgentReport{Run: 1, Status: domain.HCWorking})
	checkInside(t, w, tg)
	sleepAndWake(t, w, rp, signals, tg)
	tg.Run = 2
	saveAndReuse(t, w, rp, tg)
	for _, task := range []domain.TaskID{1, 2} {
		if err := rp.Boxes.Release(ctx(t), ports.Target{Task: task, Machine: rp.Machine, Gen: 1}); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range [][]string{{"ps", "-aq"}, {"volume", "ls", "-q"}} {
		if left := run(t, append(q, "--filter", "label="+w.label+".task")...); left != "" {
			t.Fatalf("release left %v behind: %s", q, left)
		}
	}
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	return c
}

func watch(t *testing.T, rp portstest.BoxesRig) <-chan domain.Signal {
	c, cancel := context.WithCancel(context.Background())
	out := make(chan domain.Signal, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = rp.Boxes.Watch(c, rp.Machine, func(task domain.TaskID, s domain.Signal) {
			if task == 1 {
				out <- s
			}
		})
	}()
	t.Cleanup(func() { cancel(); <-done })
	return out
}

func expect(t *testing.T, signals <-chan domain.Signal, want domain.Signal) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case s := <-signals:
			t.Logf("signal %#v", s)
			if s == want {
				return
			}
		case <-deadline:
			t.Fatalf("Watch never delivered %#v", want)
		}
	}
}

// testCA stands in for the proxy's CA. A clone that checks for it proves the trust step comes first.
const (
	testCA  = "-----BEGIN CERTIFICATE-----\nOUTPOSTITCA\n-----END CERTIFICATE-----\n"
	trusted = "grep -qxF OUTPOSTITCA /etc/ssl/certs/ca-certificates.crt"
)

func plan(rp portstest.BoxesRig, tg ports.Target, fresh bool) ports.Plan {
	return ports.Plan{
		Target: tg, Image: rp.Image, Fresh: fresh, Repos: []domain.RepoName{"o/main"},
		Env: map[string]string{"GH_TOKEN": "placeholder"}, CA: []byte(testCA), Launch: ports.Launch{
			Argv: []string{"agent"}, Home: []ports.File{{Path: ".config/agent/settings.json", Data: []byte("{}")}},
		},
	}
}

func startFresh(t *testing.T, w *world, rp portstest.BoxesRig, tg ports.Target) {
	var phases []string
	got, err := rp.Boxes.Converge(ctx(t), plan(rp, tg, true), func(p, _ string) { phases = append(phases, p) })
	t.Logf("phases %v", phases)
	if err != nil || !got.Revived {
		t.Fatalf("fresh start: %+v %v", got, err)
	}
	for _, f := range []string{"/workspace/main/README", "/root/.config/agent/settings.json", "/root/.outpost-seeded"} {
		if ok, err := rp.Boxes.Exists(ctx(t), tg, f); err != nil || !ok {
			t.Fatalf("%s: %v %v", f, ok, err)
		}
	}
	if w.clones.Load() != 1 {
		t.Fatalf("one clone, got %d", w.clones.Load())
	}
}

// checkInside reads the box's own facts: the agent's and an exec's oom_score_adj, the network
// MTU inside the box, and that no placeholder is in the box's environment.
func checkInside(t *testing.T, w *world, tg ports.Target) {
	agent := run(t, "exec", w.name(tg), "sh", "-c", `for p in $(pgrep -x agent; pgrep -f '^/bin/sh /usr/local/bin/agent'); do cat /proc/$p/oom_score_adj; done | sort -u`)
	if agent != "-1000" {
		t.Fatalf("the agent runs at -1000, got %q", agent)
	}
	rwc, err := w.sb.Dial(ctx(t), ports.BoxKey{Task: tg.Task, Gen: tg.Gen}, []string{"/opt/outpost/bin/hc", "connect"})
	if err != nil {
		t.Fatal(err)
	}
	c := hc.Dial(rwc)
	defer func() { _ = c.Close() }()
	r, err := c.Exec(ctx(t), hc.ExecReq{Argv: []string{"/opt/outpost/bin/oom0", "cat", "/proc/self/oom_score_adj"}, Timeout: 10})
	if err != nil || strings.TrimSpace(string(r.Stdout)) != "0" {
		t.Fatalf("a command through oom0 runs at 0: %q %v", r.Stdout, err)
	}
	netMTU := run(t, "network", "inspect", "--format", `{{index .Options "com.docker.network.driver.mtu"}}`, w.network)
	boxMTU := run(t, "exec", w.name(tg), "cat", "/sys/class/net/eth0/mtu")
	t.Logf("network MTU %s, box eth0 MTU %s", netMTU, boxMTU)
	if netMTU != boxMTU {
		t.Fatalf("the box gets the network's MTU: %s vs %s", netMTU, boxMTU)
	}
	if env := run(t, "inspect", "--format", "{{json .Config.Env}}", w.name(tg)); strings.Contains(env, "placeholder") {
		t.Fatalf("placeholders stay out of the box env: %s", env)
	}
}

func sleepAndWake(t *testing.T, w *world, rp portstest.BoxesRig, signals <-chan domain.Signal, tg ports.Target) {
	if err := rp.Boxes.Stop(ctx(t), tg); err != nil {
		t.Fatal(err)
	}
	expect(t, signals, domain.BoxEvent{Gen: 1, Kind: domain.BoxDied})
	if s, err := rp.Boxes.Inspect(ctx(t), tg); err != nil || s.Running {
		t.Fatalf("stopped: %+v %v", s, err)
	}
	tg.Run = 2
	got, err := rp.Boxes.Converge(ctx(t), plan(rp, tg, false), nil)
	if err != nil || !got.Revived {
		t.Fatalf("wake: %+v %v", got, err)
	}
	expect(t, signals, domain.AgentReport{Run: 2, Status: domain.HCIdle})
	if w.clones.Load() != 1 {
		t.Fatal("a wake keeps the workspace")
	}
	if n := run(t, "exec", w.name(tg), "grep", "-cxF", "OUTPOSTITCA", "/etc/ssl/certs/ca-certificates.crt"); n != "1" {
		t.Fatalf("a wake trusts the CA once, found %s copies", n)
	}
	if o, err := rp.Boxes.Observe(ctx(t), tg); err != nil || !o.Present || o.Finished {
		t.Fatalf("observe: %+v %v", o, err)
	}
}

// saveAndReuse saves task 1's box as a template and starts task 2 from it: the workspace comes
// from the template, so nothing is cloned.
func saveAndReuse(t *testing.T, w *world, rp portstest.BoxesRig, tg ports.Target) {
	run(t, "exec", w.name(tg), "sh", "-c", "mkdir -p /root/.toolchain && echo v1 >/root/.toolchain/probe")
	clock := domain.SystemClock{}
	start := clock.Now()
	tpl, err := rp.Boxes.SaveTemplate(ctx(t), tg)
	if err != nil || !strings.HasPrefix(tpl.Handle, "obit-"+w.id+"-ready/o--main:t1-g1") {
		t.Fatalf("save: %+v %v", tpl, err)
	}
	t.Logf("saved %s in %v", tpl.Handle, clock.Now().Sub(start).Round(time.Millisecond))
	again, err := rp.Boxes.SaveTemplate(ctx(t), tg)
	if err != nil || again.Handle != tpl.Handle {
		t.Fatalf("a repeated save names the same template: %+v %v", again, err)
	}
	two := ports.Target{Task: 2, Machine: rp.Machine, Run: 1, Gen: 1}
	p := plan(rp, two, true)
	p.Image = tpl.Handle
	if got, err := rp.Boxes.Converge(ctx(t), p, nil); err != nil || !got.Revived {
		t.Fatalf("start from the template: %+v %v", got, err)
	}
	if w.clones.Load() != 1 {
		t.Fatalf("a box from the template clones nothing: %d clones", w.clones.Load())
	}
	mustExist(t, rp, two, map[string]string{
		"/workspace/main/README":            "the template's workspace is restored",
		"/root/.toolchain/probe":            "the template carries what the setup box installed under /root",
		"/root/.config/agent/settings.json": "a box from the template still gets its own home files",
		"/var/lib/outpost-seed/workspace":   "the template carries its seed",
	})
	if ok, _ := rp.Boxes.Exists(ctx(t), tg, "/var/lib/outpost-seed"); ok {
		t.Fatal("the saved box's seed copy is cleaned up")
	}
}

func mustExist(t *testing.T, rp portstest.BoxesRig, tg ports.Target, want map[string]string) {
	t.Helper()
	for path, why := range want {
		if ok, err := rp.Boxes.Exists(ctx(t), tg, path); !ok {
			t.Fatalf("%s: %s missing (%v)", why, path, err)
		}
	}
}

func TestDockerStepEndsWithItsCaller(t *testing.T) {
	w := newWorld(t)
	k := ports.BoxKey{Task: 9, Gen: 1}
	if _, err := w.sb.Up(ctx(t), ports.BoxSpec{Key: k, Image: w.box, Repo: "acme/app", Fresh: true}, func(string, string) {}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.sb.Release(context.Background(), k.Task) })
	res, err := w.sb.Exec(ctx(t), k, ports.ExecReq{Argv: []string{"sh", "-c", `echo "$SECRET"; exit 3`}, Env: map[string]string{"SECRET": "s3"}})
	if err != nil || res.Code != 3 || strings.TrimSpace(string(res.Output)) != "s3" {
		t.Fatalf("a finished step: %+v %v", res, err)
	}
	c, cancel := context.WithCancel(ctx(t))
	time.AfterFunc(2*time.Second, cancel)
	if _, err := w.sb.Exec(c, k, ports.ExecReq{Argv: []string{"sh", "-c", "sleep 301 & sleep 302"}}); err == nil {
		t.Fatal("a cancelled step reported success")
	}
	name := fmt.Sprintf("obit-%s-t%d-g%d", w.id, k.Task, k.Gen)
	ps := ""
	for range 75 {
		if ps = run(t, "exec", name, "sh", "-c", `cat /proc/[0-9]*/cmdline | tr '\0' ' '`); !strings.Contains(ps, "sleep 30") {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the step outlived its caller: %s", ps)
}
