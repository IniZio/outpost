package ports

import (
	"context"

	"github.com/IniZio/outpost/internal/domain"
)

// Target names one task's box and the agent terminal in it, as of one Apply. Every Boxes call
// takes one. Run and Gen are the labels the box and the terminal carry, so a call aimed at an older
// run or generation acts on nothing newer: Kill under Run 3 never kills the terminal of Run 4.
type Target struct {
	Task    domain.TaskID
	Machine domain.MachineID
	Run     domain.AgentRun
	Gen     domain.BoxGen
}

// Plan is the whole state a box should be in, as data. Converge makes the box match it. Lifecycle
// builds a Plan from the task, the repo, the agent's Launch and the credential placeholders, and
// holds no step of its own: the order of image pull, volume creation, clones, runtime installs and
// the terminal launch is the adapter's.
//
// Fresh is set for StartTask: the workspace and home volumes are created, Repos are cloned into
// the workspace and Launch.Home seeds the home volume. Unset, for EnsureRunning, both volumes
// already exist and are reused untouched, so a replaced box loses no work.
type Plan struct {
	Target Target
	Image  string            // the base image, or the repo's ready template handle
	Fresh  bool              // create volumes, clone and seed home; else reuse them
	Repos  []domain.RepoName // Spec.Repo first, then Spec.Also, each pushable
	Env    map[string]string // credential placeholders; the proxy swaps in the real secrets
	Launch Launch            // the agent terminal, labelled with Target.Run
}

// Progress is how Converge reports a phase ("Cloning x/y") while it runs. Lifecycle applies each
// call as a Progressed event before it returns, so the timeline shows phases in order. It may be
// nil, as for EnsureRunning, and Converge then reports nothing.
type Progress func(phase, note string)

// Converged is what Converge did. Revived is false only when a terminal under Plan.Target.Run was
// already alive, which is the retried-ask case domain.Ensured.Revived describes.
type Converged struct {
	Revived bool
}

// Template is a saved ready template: an opaque handle and the machine that holds it.
type Template struct {
	Handle  string
	Machine domain.MachineID
}

// BoxState is a settled observation of one box generation. A box half way through dying is not
// Running: the adapter waits until the container has settled before it answers.
type BoxState struct {
	Running   bool
	OOMKilled bool
}

// BoxFS is the file access an agent adapter needs inside the box, and the done marker uses.
// Paths are inside the box. It is split out so internal/agent/* can take it without the rest.
type BoxFS interface {
	ReadFile(ctx context.Context, t Target, path string) ([]byte, error)
	Exists(ctx context.Context, t Target, path string) (bool, error)
	Remove(ctx context.Context, t Target, path string) error
}

// Sink receives what a feed observes. Lifecycle applies each signal before Sink returns, so a feed
// that calls Sink in order delivers in order.
type Sink func(task domain.TaskID, s domain.Signal)

// Boxes is everything Outpost does to a task's box and to the agent's terminal inside it. The
// agent runs in the box and hc runs in the box, so they are one port: a terminal exists only while
// its box is up, and one fake models both. internal/remote implements it over the machine link;
// the machine agent does the work next to the box.
//
// Every method is idempotent and safe to call for a box that is half built, already stopped or
// gone. On the machine, calls for one box are serialized, and Stop and Release first cancel and
// wait for a Converge in flight for that box, including one whose server-side caller died.
type Boxes interface {
	BoxFS

	// Converge makes the box of Plan.Target.Gen run Plan.Image with the task's volumes, and one
	// agent terminal run under Plan.Target.Run: a lost box of an older generation is replaced and a
	// terminal of an older run is ended first. It answers StartTask (Fresh) and EnsureRunning.
	// It returns promptly once ctx is done, leaving whatever it built for Release or a later
	// Converge to adopt.
	Converge(ctx context.Context, p Plan, report Progress) (Converged, error)

	// Type, Interrupt and Kill act on the agent terminal of t.Run, and do nothing when it is gone.
	Type(ctx context.Context, t Target, text string) error
	Interrupt(ctx context.Context, t Target) error
	Kill(ctx context.Context, t Target) error

	// Observe reports the terminal of t.Run, for Recovered after a restart. A missing box or
	// terminal is Present: false with no error. An error means the machine could not be asked,
	// and lifecycle waits for it to link again.
	Observe(ctx context.Context, t Target) (domain.AgentObservation, error)

	// Inspect reports the settled state of the box of t.Gen.
	Inspect(ctx context.Context, t Target) (BoxState, error)

	// Stop stops the box, keeping its disk and volumes. Release removes the box, its volumes and
	// its hc session for good.
	Stop(ctx context.Context, t Target) error
	Release(ctx context.Context, t Target) error

	// SaveTemplate commits the box as a ready template for the task's repo.
	SaveTemplate(ctx context.Context, t Target) (Template, error)

	// Watch delivers signals about every box on machine m until ctx is done. It owns reconnects:
	// it survives a relink of the machine and a restart of hc in a box. On every (re)connect it
	// first sends the current level of each box it can see, then edges:
	//   - an AgentReport with the terminal's current hc status;
	//   - an AgentExited for a terminal that ended while nobody watched;
	//   - a BoxEvent{BoxDied, StillRunning: false} for a box found stopped.
	// It never replays BoxStarted, because a late start reads as a restart. Delivery is at least
	// once: the domain drops duplicates and stale keys. It returns early only when m is revoked.
	Watch(ctx context.Context, m domain.MachineID, sink Sink) error
}
