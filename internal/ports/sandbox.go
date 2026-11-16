package ports

import (
	"context"
	"errors"
	"io"

	"github.com/IniZio/outpost/internal/domain"
)

// BoxKey names one box generation on a machine.
type BoxKey struct {
	Task domain.TaskID
	Gen  domain.BoxGen
}

// BoxSpec is what Sandbox.Up makes real. The provider names the task's volumes from its id, and
// Fresh says whether it may create them. Env is the box's own environment, never a task credential,
// because a provider may keep it in a saved template.
type BoxSpec struct {
	Key   BoxKey
	Image string
	Repo  domain.RepoName // kept on the box, so a saved template can be named for it
	Fresh bool
	Env   map[string]string
}

// BoxInfo is one box generation as the provider sees it, settled.
type BoxInfo struct {
	Key       BoxKey
	Repo      domain.RepoName
	Running   bool
	OOMKilled bool
}

// BoxChange is one provider event, with the settled facts read once at the event.
type BoxChange struct {
	Info BoxInfo
	Kind domain.BoxEventKind
}

var (
	// ErrSuperseded means a newer generation of the task's box, or a newer run of its agent,
	// exists, so a call for the older one acts on nothing.
	ErrSuperseded = errors.New("sandbox: a newer box generation or agent run exists")
	// ErrNoBox means no box of that generation exists.
	ErrNoBox = errors.New("sandbox: no such box")
	// ErrNoVolumes means a resume found the task's volumes gone, so its work is lost.
	ErrNoVolumes = errors.New("sandbox: the task's volumes are gone")
	// ErrNoImage means the image is a local template this machine does not hold.
	ErrNoImage = errors.New("sandbox: the image is not on this machine")
)

// ExecReq is one command run to its end in a box, with env added to the box's own.
type ExecReq struct {
	Argv []string
	Env  map[string]string
}

// ExecResult is how a command ended: its exit code and the tail of its stdout and stderr together.
type ExecResult struct {
	Code   int
	Output []byte
}

// Sandbox is one box provider: privileged Docker today, Incus VMs later. It makes "a box of this
// generation exists and runs, with the task's volumes" true and reports box-level facts. Inside the
// box, the agent terminal and quick checks go through hc over Dial, and the converge and save
// steps go through Exec, which has no time limit of its own. Only internal/machine consumes it,
// and it serializes the shape calls (Up, Stop, Release, Commit) for one task, so a provider need
// not lock.
//
// Every method is idempotent.
type Sandbox interface {
	// Up makes the box of spec.Key.Gen run: images present, network present, volumes present
	// (created only when Fresh), older generations removed, this one created or started. A newer
	// generation is ErrSuperseded. Up returns once the box's init has started hc.
	Up(ctx context.Context, spec BoxSpec, report Progress) (BoxInfo, error)

	// Boxes lists every box this provider owns on the machine.
	Boxes(ctx context.Context) ([]BoxInfo, error)

	// Inspect waits until the box has settled and reports it. A missing box is ErrNoBox.
	Inspect(ctx context.Context, k BoxKey) (BoxInfo, error)

	// Stop stops the box, keeping it and its volumes. A generation older than the task's box
	// does nothing.
	Stop(ctx context.Context, k BoxKey) error

	// Release removes every generation of the task's box and its volumes. Missing parts are
	// success.
	Release(ctx context.Context, task domain.TaskID) error

	// Dial runs argv in the running box and returns its stdin and stdout as one stream. Closing
	// the stream ends the process.
	Dial(ctx context.Context, k BoxKey, argv []string) (io.ReadWriteCloser, error)

	// Exec runs r in the running box to its end. It has no time limit: the end of ctx ends the
	// command and everything it started, and so does the death of the caller's process, so a step
	// cut short never races its own retry. A non-zero exit is a result, not an error.
	Exec(ctx context.Context, k BoxKey, r ExecReq) (ExecResult, error)

	// CopyIn unpacks a tar stream into dir inside the box.
	CopyIn(ctx context.Context, k BoxKey, dir string, tar io.Reader) error

	// Commit saves the box as a template and returns its handle, which is the same for the same
	// box generation, so a retried save replaces its own template. Volumes and mounts are not
	// part of it.
	Commit(ctx context.Context, k BoxKey) (string, error)

	// Events delivers box changes until ctx ends, following the provider across its restarts.
	// It first replays removals it can still see from before the call, as BoxDied changes with
	// Running false, so a box removed while nobody watched is not missed.
	Events(ctx context.Context, sink func(BoxChange)) error
}
