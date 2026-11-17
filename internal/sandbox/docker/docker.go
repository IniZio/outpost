// Package docker is the Docker sandbox provider the machine agent drives. It runs the docker CLI
// with JSON output, so each call is one an operator could type to debug a box.
//
// Everything it creates carries Config.Label and Config.Prefix, and every list and sweep filters on
// that label only, so it never touches another Docker user on the host.
package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

// Config is per machine. Prefix, Label and Network keep this agent's boxes apart from anything
// else on the host.
type Config struct {
	Prefix  string   // containers <Prefix>-t<task>-g<gen>, volumes <Prefix>-t<task>-<kind>
	Label   string   // label namespace: <Label>.task, <Label>.gen, <Label>.repo
	Network string   // the bridge every box joins
	MTU     int      // the network's MTU; 0 probes the host's routes to Probes
	Probes  []string // hosts whose route MTU bounds the network's
	Tools   string   // the image mounted read-only at /opt/outpost in every box
	Memory  string   // docker --memory, or "" for no limit
	Devices []string // host devices passed to every box, such as /dev/kvm
	Run     Runner
	Clock   domain.Clock
	Poll    time.Duration

	// Setup builds. Data is the machine's dir for repo mirrors, inner image tars and build work
	// dirs; empty turns setup builds and mirror mounts off. Builder is the BuildKit builder's
	// name, CacheMax the most its cache keeps after a build (as buildx takes it, such as 20gb).
	Data     string
	Builder  string
	CacheMax string
	GitURL   func(domain.RepoName) string
	GitAuth  []string // git -c flags that authenticate mirror fetches
	Host     Host
}

// Defaults fills the zero parts of c.
func (c Config) Defaults() Config {
	if c.Prefix == "" {
		c.Prefix = "ob"
	}
	if c.Label == "" {
		c.Label = "dev.outpost.box"
	}
	if c.Network == "" {
		c.Network = "outpost-boxes"
	}
	if len(c.Probes) == 0 {
		c.Probes = []string{"140.82.112.6", "1.1.1.1", "8.8.8.8"}
	}
	if c.Run == nil {
		c.Run = CLI{Binary: "docker"}
	}
	if c.Clock == nil {
		c.Clock = domain.SystemClock{}
	}
	if c.Poll == 0 {
		c.Poll = 200 * time.Millisecond
	}
	return c.setupDefaults()
}

func (c Config) setupDefaults() Config {
	if c.Builder == "" {
		c.Builder = c.Prefix + "-setup"
	}
	if c.CacheMax == "" {
		c.CacheMax = "20gb"
	}
	if c.GitURL == nil {
		c.GitURL = GitHubURL
	}
	if c.GitAuth == nil {
		c.GitAuth = GHAuth
	}
	if c.Host == nil {
		c.Host = OSHost{}
	}
	return c
}

// Docker implements ports.Sandbox.
type Docker struct {
	cfg Config
	net sync.Mutex
}

var _ ports.Sandbox = (*Docker)(nil)

// New returns the provider. It touches nothing until a call.
func New(cfg Config) (*Docker, error) {
	cfg = cfg.Defaults()
	if cfg.Tools == "" {
		return nil, errors.New("docker: a tools image is required")
	}
	return &Docker{cfg: cfg}, nil
}

// Runner runs the docker CLI. Output runs it to its end. Start runs it with its stdin and stdout
// as one stream, and Close on the stream ends it. Step runs it to its end with env added to its
// own, holding its stdin open the whole time, and returns its exit code and the tail of its stdout
// and stderr.
type Runner interface {
	Output(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error)
	Start(ctx context.Context, args ...string) (io.ReadWriteCloser, error)
	Step(ctx context.Context, env []string, args ...string) (int, []byte, error)
}

// CmdError is a docker command that failed, with what it wrote to stderr.
type CmdError struct {
	Args   []string
	Code   int
	Stderr string
}

func (e *CmdError) Error() string {
	return fmt.Sprintf("docker %s: exit %d: %s", strings.Join(e.Args, " "), e.Code, strings.TrimSpace(e.Stderr))
}

// notFound reports whether err is docker saying the object does not exist. Containers, images
// and volumes say "no such", networks say "not found".
func notFound(err error) bool {
	var ce *CmdError
	if !errors.As(err, &ce) {
		return false
	}
	msg := strings.ToLower(ce.Stderr)
	return strings.Contains(msg, "no such") || strings.Contains(msg, "not found")
}

// CLI runs a docker binary.
type CLI struct{ Binary string }

// Output runs docker to its end and returns its stdout.
func (c CLI) Output(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.Binary, args...) //nolint:gosec // argv is built by this package, never a shell
	var out, errb bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, &out, &errb
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out.Bytes(), &CmdError{Args: args, Code: ee.ExitCode(), Stderr: errb.String()}
		}
		return nil, fmt.Errorf("docker: %w", err)
	}
	return out.Bytes(), nil
}

// Start runs docker with its stdin and stdout as one stream.
func (c CLI) Start(ctx context.Context, args ...string) (io.ReadWriteCloser, error) {
	cmd := exec.CommandContext(ctx, c.Binary, args...) //nolint:gosec // argv is built by this package, never a shell
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	return &proc{cmd: cmd, in: in, out: out}, nil
}

// Step runs docker to its end. Its stdin stays open until docker exits, because box-step ends the
// command in the box when stdin closes, which is also what happens when this process dies.
func (c CLI) Step(ctx context.Context, env []string, args ...string) (int, []byte, error) {
	cmd := exec.CommandContext(ctx, c.Binary, args...) //nolint:gosec // argv is built by this package, never a shell
	cmd.Env = append(os.Environ(), env...)
	if _, err := cmd.StdinPipe(); err != nil {
		return 0, nil, fmt.Errorf("docker: %w", err)
	}
	out := &tailBuffer{max: stepTail}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return 0, out.bytes(), fmt.Errorf("docker: %w", ctx.Err())
	case errors.As(err, &ee):
		return ee.ExitCode(), out.bytes(), nil
	case err != nil:
		return 0, nil, fmt.Errorf("docker: %w", err)
	}
	return 0, out.bytes(), nil
}

// stepTail is how much of a step's output Step keeps.
const stepTail = 64 << 10

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 2*t.max {
		t.b = append(t.b[:0], t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) bytes() []byte {
	if len(t.b) > t.max {
		return t.b[len(t.b)-t.max:]
	}
	return t.b
}

type proc struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  io.ReadCloser
	once sync.Once
}

func (p *proc) Read(b []byte) (int, error)  { return p.out.Read(b) } //nolint:wrapcheck // a stream passes io.EOF through
func (p *proc) Write(b []byte) (int, error) { return p.in.Write(b) } //nolint:wrapcheck // a stream passes errors through

// Close ends the process: stdin first, so `hc connect` sees its caller leave, then the process.
func (p *proc) Close() error {
	p.once.Do(func() {
		_ = p.in.Close()
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		_ = p.cmd.Wait()
	})
	return nil
}
