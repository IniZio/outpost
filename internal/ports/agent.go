package ports

import (
	"context"

	"github.com/IniZio/outpost/internal/domain"
)

// File is one file to write into the agent's home volume on a fresh start.
type File struct {
	Path string // relative to the home volume
	Mode uint32
	Data []byte
}

// Launch is how to run the agent in its terminal. The agent adapter derives the session id from
// the task id, so a resume needs nothing stored.
type Launch struct {
	Argv []string
	Env  map[string]string // the agent's own placeholders, merged over Secrets' by lifecycle
	Home []File
}

// LaunchFor says which launch Agent.Launch builds. Prompt is set for a fresh start only; a resume
// continues the agent's own session.
type LaunchFor struct {
	Target Target
	Owner  domain.Login
	Prompt string
	Resume bool
}

// Agent is one coding agent CLI: Claude Code first, Cursor's agent next. internal/agent/claude
// implements it. It owns the agent's argv, its profile and where it keeps its last message, and
// knows nothing of boxes beyond BoxFS.
type Agent interface {
	Launch(ctx context.Context, f LaunchFor) (Launch, error)

	// LastMessage reads the agent's last message from its session files in the box. The agent
	// may still be writing it, so the adapter retries until it has one or ctx is done, and then
	// returns what it has, possibly "".
	LastMessage(ctx context.Context, fs BoxFS, t Target) (string, error)
}

// Creds is what a box needs to reach the credential proxy: env with placeholders and the proxy
// login, and the proxy's public CA, which the box trusts before its first clone.
type Creds struct {
	Env map[string]string
	CA  []byte
}

// Secrets hands out the credentials a box sees. The proxy swaps each placeholder for the real
// secret on the way out, so no secret enters a box. The proxy login is bound to t, so it stops
// working once that box generation is gone.
type Secrets interface {
	Credentials(ctx context.Context, t Target, owner domain.Login, repos []domain.RepoName) (Creds, error)
}
