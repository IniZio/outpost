package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

var _ ports.SetupBuilder = (*Docker)(nil)

// Host runs the host commands of a setup build, git and docker buildx, in dir with env added to
// the agent's own, writing what they print to out.
type Host interface {
	Run(ctx context.Context, dir string, env []string, out io.Writer, name string, args ...string) error
}

// OSHost runs commands on this machine.
type OSHost struct{}

// Run runs name to its end. A non-zero exit is an error.
func (OSHost) Run(ctx context.Context, dir string, env []string, out io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // argv is built by this package, never a shell
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, append(os.Environ(), env...), out, out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// GitHubURL is where the machine fetches a repo's mirror from.
func GitHubURL(r domain.RepoName) string { return "https://github.com/" + string(r) + ".git" }

// GHAuth makes git take its GitHub credential from the machine's own gh login. Setup builds have
// no task, so no proxy grant: the machine fetches as its owner, who is the only one it builds for.
var GHAuth = []string{"-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential"}

// Files the build writes next to the revision's files. domain.ValidSetupFiles reserves the names.
const (
	overlayFile = "outpost.json"
	finalFile   = "outpost.Dockerfile"
	logTail     = 4000
)

// build is one run of BuildSetup.
type build struct {
	d      *Docker
	s      ports.SetupSpec
	report ports.Progress
	work   string
	image  string
	log    *tailBuffer
	commit string
}

// BuildSetup builds a setup revision: refresh the repo's mirror, check the commit out, bake the
// box image with Outpost's final stage on top, export the inner images, and tidy the machine.
func (d *Docker) BuildSetup(ctx context.Context, s ports.SetupSpec, report ports.Progress) (ports.SetupBuilt, error) {
	if d.cfg.Data == "" {
		return ports.SetupBuilt{}, errors.New("docker: setup builds need a data dir")
	}
	b := &build{
		d: d, s: s, report: report, log: &tailBuffer{max: stepTail},
		work:  filepath.Join(d.cfg.Data, "builds", fmt.Sprintf("%s-r%d", domain.RepoSlug(s.Repo), s.Rev)),
		image: fmt.Sprintf("%s-setup/%s:r%d", d.cfg.Prefix, domain.RepoSlug(s.Repo), s.Rev),
	}
	defer func() { _ = os.RemoveAll(b.work) }()
	got, err := b.run(ctx)
	if err != nil {
		return ports.SetupBuilt{}, fmt.Errorf("%w\n%s", err, tail(string(b.log.bytes()), 2000))
	}
	got.Log = tail(string(b.log.bytes()), logTail)
	return got, nil
}

func (b *build) run(ctx context.Context) (ports.SetupBuilt, error) {
	for _, step := range []func(context.Context) error{b.mirror, b.checkout, b.write, b.builder, b.bake} {
		if err := step(ctx); err != nil {
			return ports.SetupBuilt{}, err
		}
	}
	out, err := b.d.docker(ctx, "image", "inspect", "--format", "{{.Id}}", b.image)
	if err != nil {
		return ports.SetupBuilt{}, err
	}
	inner, err := b.d.storeInner(b.s.Repo, b.s.Rev, filepath.Join(b.work, "out"))
	if err != nil {
		return ports.SetupBuilt{}, err
	}
	b.say("Tidying the machine", "")
	if err := b.d.tidy(ctx, b.s); err != nil {
		return ports.SetupBuilt{}, err
	}
	return ports.SetupBuilt{Image: b.image, Digest: strings.TrimSpace(string(out)), Inner: inner, Commit: b.commit}, nil
}

func (b *build) say(phase, note string) {
	if b.report != nil {
		b.report(phase, note)
	}
}

func (b *build) host(ctx context.Context, dir string, env []string, name string, args ...string) error {
	return b.d.cfg.Host.Run(ctx, dir, env, b.log, name, args...) //nolint:wrapcheck // the host names the command
}

func (b *build) git(ctx context.Context, args ...string) error {
	return b.host(ctx, "", nil, "git", append(slices.Clone(b.d.cfg.GitAuth), args...)...)
}

// mirror clones the repo's bare mirror once, then fetches into it. Branches map onto the mirror's
// own, and gc is off, so objects that boxes borrow through --reference are never pruned.
func (b *build) mirror(ctx context.Context) error {
	dir := b.d.mirrorPath(b.s.Repo)
	if !isDir(dir) {
		b.say("Mirroring "+string(b.s.Repo), "")
		tmp := dir + ".partial"
		if err := os.RemoveAll(tmp); err != nil {
			return fmt.Errorf("docker: %w", err)
		}
		if err := b.git(ctx, "clone", "--bare", "-q", b.d.cfg.GitURL(b.s.Repo), tmp); err != nil {
			return err
		}
		if err := os.Rename(tmp, dir); err != nil {
			return fmt.Errorf("docker: %w", err)
		}
	}
	b.say("Fetching "+string(b.s.Repo), "")
	for _, kv := range [][2]string{{"gc.auto", "0"}, {"remote.origin.fetch", "+refs/heads/*:refs/heads/*"}} {
		if err := b.git(ctx, "-C", dir, "config", kv[0], kv[1]); err != nil {
			return err
		}
	}
	return b.git(ctx, "-C", dir, "fetch", "-q", "--prune", "--tags", "origin")
}

// checkout makes work/src the commit's tree, sharing the mirror's objects.
func (b *build) checkout(ctx context.Context) error {
	mirror := b.d.mirrorPath(b.s.Repo)
	b.commit = b.s.Commit
	if b.commit == "" {
		var out bytes.Buffer
		if err := b.d.cfg.Host.Run(ctx, "", nil, &out, "git", "-C", mirror, "rev-parse", "HEAD"); err != nil {
			return err //nolint:wrapcheck // the host names the command
		}
		b.commit = strings.TrimSpace(out.String())
	}
	if err := os.RemoveAll(b.work); err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	src := filepath.Join(b.work, "src")
	if err := b.git(ctx, "clone", "-q", "--shared", "--no-checkout", mirror, src); err != nil {
		return err
	}
	b.say("Checking out "+b.commit, "")
	return b.git(ctx, "-C", src, "checkout", "-q", "--detach", b.commit)
}

// write puts the revision's files and Outpost's overlay into the work dir.
func (b *build) write(context.Context) error {
	files := map[string]string{}
	for name, body := range b.s.Files {
		files[name] = body
	}
	overlay, err := b.overlay()
	if err != nil {
		return err
	}
	files[overlayFile], files[finalFile] = overlay, b.final()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(b.work, name), []byte(body), 0o600); err != nil {
			return fmt.Errorf("docker: %w", err)
		}
	}
	return nil
}

// overlay is the bake file Outpost adds: it feeds the revision's box target the base image and the
// source, and builds the final image from it under Outpost's own tag.
func (b *build) overlay() (string, error) {
	contexts := map[string]string{"box": "target:box"}
	if len(b.s.Profile) > 0 {
		contexts["tools"] = "docker-image://" + b.d.cfg.Tools
	}
	doc := map[string]any{"target": map[string]any{
		"box": map[string]any{
			"args":     map[string]string{"BASE": b.s.Base},
			"contexts": map[string]string{"src": filepath.Join(b.work, "src")},
		},
		"outpost": map[string]any{
			"context": ".", "dockerfile": finalFile, "contexts": contexts,
			"tags": []string{b.image}, "output": []string{"type=docker"},
			"labels": map[string]string{b.d.cfg.Label + ".setup": fmt.Sprintf("%s r%d", b.s.Repo, b.s.Rev)},
		},
	}}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("docker: overlay: %w", err)
	}
	return string(out), nil
}

// final is Outpost's stage on top of the revision's box: the after-clone script, and the agent
// profile installed into /root, which Docker copies into a new task's empty home volume. The
// marker tells the machine's converge that the profile is in.
func (b *build) final() string {
	lines := []string{"FROM box"}
	if _, ok := b.s.Files[domain.AfterCloneFile]; ok {
		lines = append(lines, "COPY --chmod=0755 "+domain.AfterCloneFile+" "+domain.AfterClonePath)
	}
	if len(b.s.Profile) == 0 {
		return strings.Join(lines, "\n") + "\n"
	}
	for _, argv := range b.s.Profile {
		j, _ := json.Marshal(argv)
		lines = append(lines, "RUN --mount=type=bind,from=tools,source=/opt/outpost,target=/opt/outpost "+string(j))
	}
	return strings.Join(append(lines, "RUN touch /root/.outpost-profiled"), "\n") + "\n"
}

// builder makes the machine's BuildKit builder once: a container builder on the box network, so
// builds reach the network the way boxes do, and its cache is its own to prune.
func (b *build) builder(ctx context.Context) error {
	if _, err := b.d.docker(ctx, "buildx", "inspect", b.d.cfg.Builder); err == nil {
		return nil
	}
	if _, err := b.d.ensureNetwork(ctx); err != nil {
		return err
	}
	_, err := b.d.docker(ctx, "buildx", "create", "--name", b.d.cfg.Builder, "--driver", "docker-container",
		"--driver-opt", "network="+b.d.cfg.Network)
	return err
}

// bake builds the groups in order: prepare, the final image, then inner. Only the box target is
// required.
func (b *build) bake(ctx context.Context) error {
	targets, err := b.targets(ctx)
	if err != nil {
		return err
	}
	if !targets["box"] {
		return errors.New("docker: the bake file has no target \"box\"")
	}
	steps := []struct{ name, phase string }{
		{"prepare", "Preparing build contexts"}, {"outpost", "Building the box image"}, {"inner", "Building the repo's Docker images"},
	}
	for _, s := range steps {
		if s.name != "outpost" && !targets[s.name] {
			continue
		}
		b.say(s.phase, "")
		if err := b.bakeOne(ctx, s.name); err != nil {
			return err
		}
	}
	return nil
}

func (b *build) bakeArgs(extra ...string) []string {
	return append([]string{"buildx", "bake", "-f", domain.SetupBake, "-f", overlayFile}, extra...)
}

func (b *build) env() []string {
	return []string{"SRC=" + filepath.Join(b.work, "src"), "BASE=" + b.s.Base}
}

func (b *build) targets(ctx context.Context) (map[string]bool, error) {
	var out bytes.Buffer
	err := b.d.cfg.Host.Run(ctx, b.work, b.env(), &out, "docker", b.bakeArgs("--list=type=targets,format=json")...)
	if err != nil {
		return nil, fmt.Errorf("docker: reading the bake file: %w", err)
	}
	var list []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(jsonPart(out.Bytes()), &list); err != nil {
		return nil, fmt.Errorf("docker: bake targets: %w", err)
	}
	got := map[string]bool{}
	for _, t := range list {
		got[t.Name] = true
	}
	return got, nil
}

// jsonPart drops the progress lines bake prints before its JSON, which starts a line with "[".
func jsonPart(b []byte) []byte {
	if bytes.HasPrefix(b, []byte("[")) {
		return b
	}
	if i := bytes.Index(b, []byte("\n[")); i >= 0 {
		return b[i+1:]
	}
	return b
}

func (b *build) bakeOne(ctx context.Context, target string) error {
	w := &stepWriter{to: b.log, say: func(h string) { b.say("", h) }, seen: map[string]bool{}}
	args := b.bakeArgs("--builder", b.d.cfg.Builder, "--progress=plain", target)
	return b.d.cfg.Host.Run(ctx, b.work, b.env(), w, "docker", args...) //nolint:wrapcheck // the host names the command
}

// stepHeader is a BuildKit plain-progress line that names a build step, as "#12 [box 2/4] RUN …".
var stepHeader = regexp.MustCompile(`^#(\d+) (\[[^\]]+\] .*)$`)

// stepWriter passes output to the log and reports each step header once, so a follower sees what
// the build is doing without every line of it.
type stepWriter struct {
	mu   sync.Mutex
	to   io.Writer
	say  func(string)
	seen map[string]bool
	part []byte
}

func (w *stepWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.part = append(w.part, p...)
	for {
		i := bytes.IndexByte(w.part, '\n')
		if i < 0 {
			break
		}
		line := string(w.part[:i])
		w.part = w.part[i+1:]
		if m := stepHeader.FindStringSubmatch(line); m != nil && !w.seen[m[1]] {
			w.seen[m[1]] = true
			w.say(tail(m[2], 200))
		}
	}
	return w.to.Write(p) //nolint:wrapcheck // a writer passes errors through
}
