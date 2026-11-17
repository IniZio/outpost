package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

var uuidShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func TestSessionID(t *testing.T) {
	a, b := SessionID(7), SessionID(8)
	if !uuidShape.MatchString(a) {
		t.Fatalf("not a uuid: %s", a)
	}
	if a != SessionID(7) || a == b {
		t.Fatalf("want a stable id per task, got %s %s", a, b)
	}
}

func TestLaunchArgv(t *testing.T) {
	sid := SessionID(7)
	tg := ports.Target{Task: 7, Run: 2}
	tests := []struct {
		name string
		cfg  Config
		f    ports.LaunchFor
		want []string
	}{
		{
			"fresh",
			Config{Model: "opus"},
			ports.LaunchFor{Target: tg, Prompt: "do it"},
			[]string{Bin, "--dangerously-skip-permissions", "--session-id", sid, "--model", "opus", "do it"},
		},
		{
			"fresh without a model",
			Config{},
			ports.LaunchFor{Target: tg, Prompt: "/loop\n\nx"},
			[]string{Bin, "--dangerously-skip-permissions", "--session-id", sid, "/loop\n\nx"},
		},
		{
			"resume",
			Config{Model: "opus"},
			ports.LaunchFor{Target: tg, Resume: true, Prompt: "ignored"},
			[]string{Bin, "--dangerously-skip-permissions", "--resume", sid, "--model", "opus"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, err := New(tc.cfg).Launch(context.Background(), tc.f)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(l.Argv, tc.want) {
				t.Fatalf("argv\n got %q\nwant %q", l.Argv, tc.want)
			}
		})
	}
}

func TestLaunchEnv(t *testing.T) {
	l, err := New(Config{}).Launch(context.Background(), ports.LaunchFor{Target: ports.Target{Task: 1}, Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"IS_SANDBOX": "1", "DISABLE_AUTOUPDATER": "1", "CLAUDE_CODE_OAUTH_TOKEN": OAuthPlaceholder,
	}
	for k, v := range want {
		if l.Env[k] != v {
			t.Errorf("env %s = %q, want %q", k, l.Env[k], v)
		}
	}
	if len(l.Env) != len(want) {
		t.Errorf("unexpected env: %v", l.Env)
	}
}

func TestLaunchTrustsRepoDirs(t *testing.T) {
	repos := []domain.RepoName{"o/moor-pilot-example", "o/other"}
	l, err := New(Config{}).Launch(context.Background(), ports.LaunchFor{Target: ports.Target{Task: 1}, Prompt: "x", Repos: repos})
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Projects map[string]struct {
			Trusted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	mustJSON(t, l.Home[0].Data, &state)
	for _, r := range repos {
		if !state.Projects[domain.RepoDir(r)].Trusted {
			t.Errorf("repo %s not trusted: %+v", r, state.Projects)
		}
	}
}

func TestLaunchHomeFiles(t *testing.T) {
	l, err := New(Config{}).Launch(context.Background(), ports.LaunchFor{Target: ports.Target{Task: 1}, Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Home) != 2 || l.Home[0].Path != ".claude.json" || l.Home[1].Path != ".claude/settings.json" {
		t.Fatalf("home files: %+v", l.Home)
	}
	var state struct {
		Onboarded bool `json:"hasCompletedOnboarding"`
		Bypass    bool `json:"bypassPermissionsModeAccepted"`
		Projects  map[string]struct {
			Trusted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	mustJSON(t, l.Home[0].Data, &state)
	if !state.Onboarded || !state.Bypass || !state.Projects[WorkspaceDir].Trusted {
		t.Errorf(".json: %+v", state)
	}
	var settings struct {
		Skip  bool `json:"skipDangerousModePermissionPrompt"`
		Hooks struct {
			SessionStart []struct {
				Hooks []struct{ Type, Command string } `json:"hooks"`
			}
		} `json:"hooks"`
	}
	mustJSON(t, l.Home[1].Data, &settings)
	hs := settings.Hooks.SessionStart
	if !settings.Skip || len(hs) != 1 || len(hs[0].Hooks) != 1 || hs[0].Hooks[0].Type != "command" {
		t.Fatalf("settings: %+v", settings)
	}
	for _, f := range l.Home {
		if f.Mode != 0o600 {
			t.Errorf("%s mode %o", f.Path, f.Mode)
		}
	}
}

func mustJSON(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

// boxFS is a map of box paths to contents. With max set it reads as the machine does: a whole
// file over max is refused, and a tail read keeps the last max bytes.
type boxFS struct {
	mu    sync.Mutex
	files map[string][]byte
	err   error
	max   int
}

func (b *boxFS) put(p string, data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.files == nil {
		b.files = map[string][]byte{}
	}
	b.files[p] = data
}

func (b *boxFS) ReadFile(_ context.Context, _ ports.Target, p string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return nil, b.err
	}
	d, ok := b.files[p]
	if !ok {
		return nil, fs.ErrNotExist
	}
	if b.max > 0 && len(d) > b.max {
		return nil, errors.New("over the read limit")
	}
	return d, nil
}

func (b *boxFS) ReadTail(ctx context.Context, t ports.Target, p string) ([]byte, error) {
	b.mu.Lock()
	limit, d := b.max, b.files[p]
	b.mu.Unlock()
	if limit > 0 && len(d) > limit {
		return d[len(d)-limit:], nil
	}
	return b.ReadFile(ctx, t, p)
}

func (b *boxFS) Exists(context.Context, ports.Target, string) (bool, error) { return false, nil }
func (b *boxFS) Remove(context.Context, ports.Target, string) error         { return nil }

const (
	pointerPath    = Home + "/.outpost/session.json"
	transcriptPath = Home + "/.claude/projects/-workspace-repo/x.jsonl"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func rig(t *testing.T) (*Agent, *boxFS) {
	t.Helper()
	b := &boxFS{}
	b.put(pointerPath, []byte(`{"session_id":"x","transcript_path":"`+transcriptPath+`","hook_event_name":"SessionStart"}`))
	return New(Config{Poll: time.Millisecond}), b
}

func TestLastMessage(t *testing.T) {
	a, b := rig(t)
	b.put(transcriptPath, fixture(t))
	got, err := a.LastMessage(context.Background(), b, ports.Target{Task: 1})
	if err != nil {
		t.Fatal(err)
	}
	if want := "Done. Opened PR 12 with the bumps."; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLastMessageSkipsAHalfWrittenTail(t *testing.T) {
	a, b := rig(t)
	b.put(transcriptPath, append(fixture(t), []byte(`{"type":"assistant","message":{"content":[{"type":"te`)...))
	got, _ := a.LastMessage(context.Background(), b, ports.Target{Task: 1})
	if got != "Done. Opened PR 12 with the bumps." {
		t.Fatalf("got %q", got)
	}
}

func TestLastMessageReadsTheEndOfALongTranscript(t *testing.T) {
	a, b := rig(t)
	data := fixture(t)
	long := append(bytes.Repeat([]byte(`{"type":"user","message":{"content":"padding"}}`+"\n"), 2000), data...)
	b.put(transcriptPath, long)
	b.max = len(data) + 10
	got, err := a.LastMessage(context.Background(), b, ports.Target{Task: 1})
	if err != nil || got != "Done. Opened PR 12 with the bumps." {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestLastMessageWaitsForTheTranscript(t *testing.T) {
	a, b := rig(t)
	data := fixture(t)
	go func() {
		time.Sleep(20 * time.Millisecond)
		b.put(transcriptPath, data)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := a.LastMessage(ctx, b, ports.Target{Task: 1})
	if err != nil || got == "" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestLastMessageGivesUpEmptyWhenCtxEnds(t *testing.T) {
	tests := map[string]func(*boxFS){
		"no pointer":        func(b *boxFS) { delete(b.files, pointerPath) },
		"no transcript":     func(*boxFS) {},
		"half pointer":      func(b *boxFS) { b.put(pointerPath, []byte(`{"transcript_pa`)) },
		"no assistant text": func(b *boxFS) { b.put(transcriptPath, []byte(`{"type":"user","message":{"content":"hi"}}`+"\n")) },
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			a, b := rig(t)
			setup(b)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			got, err := a.LastMessage(ctx, b, ports.Target{Task: 1})
			if got != "" || err != nil {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
}

func TestLastMessageReturnsAReadError(t *testing.T) {
	a, b := rig(t)
	b.err = errors.New("link down")
	_, err := a.LastMessage(context.Background(), b, ports.Target{Task: 1})
	if err == nil {
		t.Fatal("want the read error")
	}
}

func TestLaunchSetupInstallsThePlugins(t *testing.T) {
	a := New(Config{Marketplaces: []string{"acme/plugins"}, Plugins: []string{"kit@acme", "lint@acme"}})
	l, err := a.Launch(context.Background(), ports.LaunchFor{Target: ports.Target{Task: 1}, Prompt: "/kit go"})
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{Bin, "plugin", "marketplace", "add", "acme/plugins"},
		{Bin, "plugin", "install", "kit@acme"},
		{Bin, "plugin", "install", "lint@acme"},
	}
	if !slices.EqualFunc(l.Setup, want, slices.Equal[[]string]) {
		t.Fatalf("setup: %v", l.Setup)
	}
	if l, _ := New(Config{}).Launch(context.Background(), ports.LaunchFor{Target: ports.Target{Task: 1}}); l.Setup != nil {
		t.Fatalf("no plugins, no setup: %v", l.Setup)
	}
}
