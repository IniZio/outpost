// Package claude is the agent adapter for Claude Code. It runs the claude binary from the tools
// image, derives the session id from the task id so a resume needs nothing stored, and reads the
// last assistant message back from the session transcript on the home volume.
//
// Status reporting needs nothing here: Claude Code speaks OSC 7501 to its terminal itself, and hc
// turns that into working, idle, blocked and error. The done marker is the prompt's job
// (domain.LaunchPrompt); this adapter only reads files for it through ports.BoxFS.
package claude

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

const (
	// Bin is where the tools image puts claude in every box.
	Bin = "/opt/outpost/bin/claude"
	// Home is the agent's home in the box, the home volume's mount.
	Home = "/root"
	// WorkspaceDir is the parent of every checkout. Newer Claude Code does not let it cover a git
	// root below it, so each repo dir is trusted too.
	WorkspaceDir = "/workspace"
	// OAuthPlaceholder is what the box holds in CLAUDE_CODE_OAUTH_TOKEN. The proxy swaps it for the
	// owner's real token on the way to the API.
	OAuthPlaceholder = "outpost-placeholder-claude-oauth-token"

	sessionPointer = ".outpost/session.json"
	defaultPoll    = 500 * time.Millisecond
)

// Config is how the adapter is set up. The zero value works.
type Config struct {
	Model string        // --model; empty keeps Claude Code's default
	Bin   string        // the command in the box; empty is Bin. A test points it at a fake claude
	Poll  time.Duration // how often LastMessage looks again; default 500ms
	// Marketplaces are plugin marketplace sources (owner/repo or a git URL) added on a fresh
	// home, and Plugins are the plugins (name@marketplace) installed from them, with Claude Code's
	// own plugin commands.
	Marketplaces []string
	Plugins      []string
}

// Agent implements ports.Agent for Claude Code.
type Agent struct{ cfg Config }

var _ ports.Agent = (*Agent)(nil)

// New returns a Claude Code adapter.
func New(cfg Config) *Agent {
	if cfg.Poll <= 0 {
		cfg.Poll = defaultPoll
	}
	if cfg.Bin == "" {
		cfg.Bin = Bin
	}
	return &Agent{cfg: cfg}
}

// SessionID is the task's Claude Code session id: a UUID derived from the task id, the same on
// every launch of the task.
func SessionID(t domain.TaskID) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("outpost/claude-session/%d", int64(t))))
	b := h[:16]
	b[6] = b[6]&0x0f | 0x50
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Launch builds the claude command for a fresh start or a resume, the env it needs in a root
// sandbox, and the home files that keep it from stopping at a dialog.
func (a *Agent) Launch(_ context.Context, f ports.LaunchFor) (ports.Launch, error) {
	session := SessionID(f.Target.Task)
	argv := []string{a.cfg.Bin, "--dangerously-skip-permissions"}
	if f.Resume {
		argv = append(argv, "--resume", session)
	} else {
		argv = append(argv, "--session-id", session)
	}
	if a.cfg.Model != "" {
		argv = append(argv, "--model", a.cfg.Model)
	}
	if !f.Resume {
		argv = append(argv, f.Prompt)
	}
	home, err := homeFiles(f.Repos, a.cfg.Plugins)
	if err != nil {
		return ports.Launch{}, err
	}
	return ports.Launch{
		Argv: argv,
		Env: map[string]string{
			"IS_SANDBOX":              "1",
			"DISABLE_AUTOUPDATER":     "1",
			"CLAUDE_CODE_OAUTH_TOKEN": OAuthPlaceholder,
		},
		Home:  home,
		Setup: a.setup(),
	}, nil
}

// Profile is the commands that install the owner's plugins into a home, for a setup build to bake
// into its image.
func (a *Agent) Profile() [][]string { return a.setup() }

// setup adds the marketplaces and installs the plugins. Both commands succeed when the thing is
// there already.
func (a *Agent) setup() [][]string {
	var out [][]string
	for _, m := range a.cfg.Marketplaces {
		out = append(out, []string{a.cfg.Bin, "plugin", "marketplace", "add", m})
	}
	for _, p := range a.cfg.Plugins {
		out = append(out, []string{a.cfg.Bin, "plugin", "install", p})
	}
	return out
}

// homeFiles are the files written into the home at every start. .claude.json skips onboarding and
// the bypass-permissions and trust dialogs. The settings file records the transcript path at every
// session start, because the transcript's directory is named after the working directory, which
// LastMessage does not know. It also enables the plugins: writing it drops what `claude plugin
// install` recorded, and on a home from a template the install finds the plugin there and enables
// nothing.
func homeFiles(repos []domain.RepoName, plugins []string) ([]ports.File, error) {
	trusted := func() map[string]any {
		return map[string]any{"hasTrustDialogAccepted": true, "hasCompletedProjectOnboarding": true}
	}
	projects := map[string]any{WorkspaceDir: trusted()}
	for _, r := range repos {
		projects[domain.RepoDir(r)] = trusted()
	}
	state := map[string]any{
		"hasCompletedOnboarding":        true,
		"bypassPermissionsModeAccepted": true,
		"projects":                      projects,
	}
	enabled := make(map[string]bool, len(plugins))
	for _, p := range plugins {
		enabled[p] = true
	}
	settings := map[string]any{
		"enabledPlugins":                    enabled,
		"skipDangerousModePermissionPrompt": true,
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{"hooks": []any{map[string]any{
				"type":    "command",
				"command": `mkdir -p "$HOME/.outpost" && cat > "$HOME/` + sessionPointer + `"`,
			}}}},
		},
	}
	files := make([]ports.File, 0, 2)
	for _, f := range []struct {
		path string
		v    any
	}{{".claude.json", state}, {".claude/settings.json", settings}} {
		data, err := json.MarshalIndent(f.v, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("claude: encode %s: %w", f.path, err)
		}
		files = append(files, ports.File{Path: f.path, Mode: 0o600, Data: data})
	}
	return files, nil
}
