package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

func TestBoxesOfASetupBuildMountMirrorsAndInnerImagesReadOnly(t *testing.T) {
	data := t.TempDir()
	d, err := New(Config{Tools: "ob-tools:1", Data: data, Run: &host{answer: func(string) (string, error) { return "", nil }}})
	if err != nil {
		t.Fatal(err)
	}
	s := ports.BoxSpec{Repo: "o/r", Repos: []domain.RepoName{"o/r", "o/lib"}, Inner: "0123456789abcdef"}
	if _, err := d.setupMounts(s); !errors.Is(err, ports.ErrNoImage) {
		t.Fatalf("a missing inner set is a missing image: %v", err)
	}
	for _, p := range []string{d.mirrorPath("o/r"), d.innerPath("o/r", s.Inner)} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got, err := d.setupMounts(s)
	if err != nil {
		t.Fatal(err)
	}
	want := "--mount type=bind,src=" + d.mirrorPath("o/r") + ",dst=/opt/outpost-mirrors/o--r.git,readonly " +
		"--mount type=bind,src=" + d.innerPath("o/r", s.Inner) + ",dst=" + domain.InnerImagesDir + ",readonly " +
		"--label dev.outpost.box.inner=" + s.Inner
	if strings.Join(got, " ") != want {
		t.Fatalf("mounts\n got %v\nwant %v", strings.Join(got, " "), want)
	}
	if !d.IsTemplate("ob-setup/o--r:r1") || d.IsTemplate("ghcr.io/x/ob-setup:1") {
		t.Fatal("setup builds are local only")
	}
}

func TestTheFinalStageInstallsTheProfileAndTheAfterCloneScript(t *testing.T) {
	d, err := New(Config{Tools: "ghcr.io/x/tools:1", Run: &host{}})
	if err != nil {
		t.Fatal(err)
	}
	b := &build{d: d, s: ports.SetupSpec{
		Files:   map[string]string{domain.AfterCloneFile: "true"},
		Profile: [][]string{{"/opt/outpost/bin/claude", "plugin", "install", `a"b`}},
	}, work: "/w", image: "ob-setup/o--r:r1"}
	final := b.final()
	for _, s := range []string{
		"FROM box\n", "COPY --chmod=0755 after-clone.sh " + domain.AfterClonePath,
		`RUN --mount=type=bind,from=tools,source=/opt/outpost,target=/opt/outpost ["/opt/outpost/bin/claude","plugin","install","a\"b"]`,
		"RUN touch /root/.outpost-profiled",
	} {
		if !strings.Contains(final, s) {
			t.Errorf("final stage lacks %q:\n%s", s, final)
		}
	}
	overlay, err := b.overlay()
	if err != nil || !strings.Contains(overlay, `"tools": "docker-image://ghcr.io/x/tools:1"`) || !strings.Contains(overlay, `"ob-setup/o--r:r1"`) {
		t.Fatalf("overlay: %v\n%s", err, overlay)
	}
	b.s = ports.SetupSpec{}
	if got := b.final(); got != "FROM box\n" {
		t.Fatalf("no profile and no script: %q", got)
	}
}

func TestStepWriterReportsEachStepOnce(t *testing.T) {
	var said []string
	var log tailBuffer
	log.max = 1 << 10
	w := &stepWriter{to: &log, say: func(s string) { said = append(said, s) }, seen: map[string]bool{}}
	_, _ = w.Write([]byte("#1 [internal] load\n#5 [box 1/2] RUN apt-get\n#5 0.1 Reading\n#5 DONE 1.0s\n#6 [box 2/2] RU"))
	_, _ = w.Write([]byte("N pip\n"))
	if strings.Join(said, "|") != "[internal] load|[box 1/2] RUN apt-get|[box 2/2] RUN pip" {
		t.Fatalf("said %q", said)
	}
	if !strings.Contains(string(log.bytes()), "Reading") {
		t.Fatal("every line reaches the log")
	}
	if got := string(jsonPart([]byte("#1 [internal] x\n[\n {}\n]"))); got != "[\n {}\n]" {
		t.Fatalf("json part %q", got)
	}
}

func TestTidyRemovesOldRevisionsButNotWhatBoxesUse(t *testing.T) {
	data := t.TempDir()
	h := &host{}
	h.answer = func(cmd string) (string, error) {
		switch {
		case strings.HasPrefix(cmd, "image ls"):
			return "r1\nr2\nr3\nr4\nlatest\n", nil
		case cmd == "image rm ob-setup/o--r:r2":
			return "", &CmdError{Code: 1, Stderr: "conflict: image is being used by stopped container"}
		case strings.Contains(cmd, "label=dev.outpost.box.inner=k1"):
			return "abc\n", nil
		}
		return "", nil
	}
	d, err := New(Config{Tools: "t", Data: data, Run: h, Poll: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	for rev := 1; rev <= 5; rev++ {
		writeRef(t, d, rev, "k"+strconv.Itoa(rev))
	}
	if err := d.tidy(context.Background(), ports.SetupSpec{Repo: "o/r", Rev: 5, Keep: []int{4}}); err != nil {
		t.Fatal(err)
	}
	if len(h.ran("image rm ob-setup/o--r:r1")) != 1 || len(h.ran("image rm ob-setup/o--r:r4")) != 0 || len(h.ran("image rm ob-setup/o--r:latest")) != 0 {
		t.Fatalf("calls %v", h.calls)
	}
	// r1: image gone but a box mounts k1. r2: image in use. r3: gone. r4, r5: kept.
	checkSets(t, d, map[string]bool{"k1": true, "k2": true, "k3": false, "k4": true, "k5": true})
	if len(h.ran("buildx prune --builder ob-setup --force --max-used-space 20gb")) != 1 {
		t.Fatalf("the cache is capped: %v", h.calls)
	}
}

func writeRef(t *testing.T, d *Docker, rev int, key string) {
	t.Helper()
	for _, p := range []string{d.innerPath("o/r", key), d.refsPath("o/r")} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(d.refPath("o/r", rev), []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
}

func checkSets(t *testing.T, d *Docker, want map[string]bool) {
	t.Helper()
	for key, stays := range want {
		if _, err := os.Stat(filepath.Join(d.innerRoot("o/r"), key)); (err == nil) != stays {
			t.Errorf("%s stays = %v, want %v", key, err == nil, stays)
		}
	}
}
