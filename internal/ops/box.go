package ops

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/link"
)

// BoxRef is ports.Target on the wire, less the machine the call is routed to. ops may not import
// ports, so internal/remote converts.
type BoxRef struct {
	Task domain.TaskID   `json:"task"`
	Run  domain.AgentRun `json:"run"`
	Gen  domain.BoxGen   `json:"gen"`
}

// Validate refuses a missing task. Gen 0 is legal: Release for a task that never got a box.
func (r BoxRef) Validate() error {
	if r.Task <= 0 {
		return errors.New("box ref needs a task")
	}
	return nil
}

// HomeFile is ports.File on the wire. Data is base64 in JSON.
type HomeFile struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
	Data []byte `json:"data"`
}

// ConvergeReq is ports.Plan on the wire, plus the caller's budget. The machine runs the job under
// that budget from its first submission, so a job whose callers all died still ends.
type ConvergeReq struct {
	Box    BoxRef            `json:"box"`
	Image  string            `json:"image"`
	Fresh  bool              `json:"fresh"`
	Repos  []domain.RepoName `json:"repos"`
	Env    map[string]string `json:"env"`
	CA     []byte            `json:"ca,omitempty"`
	Argv   []string          `json:"argv"`
	Home   []HomeFile        `json:"home"`
	Setup  [][]string        `json:"setup,omitempty"`
	Budget time.Duration     `json:"budget"`
}

var (
	envName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	repoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	imageRef = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]{0,254}$`)
)

// HomeBudget keeps a ConvergeReq under the link's 1 MiB frame, with room for base64.
const HomeBudget = 640 << 10

// Validate is the one place Plan rules live. Nothing here reaches a shell: the machine passes argv
// and env as lists to hc and the provider.
func (r ConvergeReq) Validate() error {
	if err := r.Box.Validate(); err != nil {
		return err
	}
	if !imageRef.MatchString(r.Image) || len(r.Repos) == 0 || len(r.Argv) == 0 || r.Budget <= 0 {
		return errors.New("converge needs an image, a repo, an argv and a budget")
	}
	for _, n := range r.Repos {
		if !repoName.MatchString(string(n)) || strings.Contains(string(n), "..") {
			return fmt.Errorf("bad repo %q", n)
		}
	}
	if err := validEnv(r.Env); err != nil {
		return err
	}
	if slices.ContainsFunc(r.Setup, func(argv []string) bool { return len(argv) == 0 }) {
		return errors.New("a setup command is empty")
	}
	return validHome(r.Home)
}

func validEnv(env map[string]string) error {
	for k := range env {
		if !envName.MatchString(k) {
			return fmt.Errorf("bad env name %q", k)
		}
	}
	return nil
}

func validHome(files []HomeFile) error {
	size := 0
	for _, f := range files {
		size += len(f.Data)
		clean := path.Clean(f.Path)
		if f.Path == "" || path.IsAbs(f.Path) || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("home file %q must stay in the home volume", f.Path)
		}
	}
	if size > HomeBudget {
		return fmt.Errorf("home files are %d bytes, over %d", size, HomeBudget)
	}
	return nil
}

// ConvergeLine is one JSON line on the converge stream. Exactly one field is set. The stream ends
// with Done or Err. An EOF before either means the link was cut, not that the job ended.
type ConvergeLine struct {
	Phase *Phase      `json:"phase,omitempty"`
	Done  *Converged  `json:"done,omitempty"`
	Err   *link.Error `json:"err,omitempty"`
}

// Phase is one progress report of a converge job.
type Phase struct {
	Phase string `json:"phase"`
	Note  string `json:"note,omitempty"`
}

// Converged is what a converge job did. Revived is false only when this run's terminal was
// already live.
type Converged struct {
	Revived bool `json:"revived"`
}

type (
	// TypeReq is text for the agent terminal of Box.Run.
	TypeReq struct {
		Box  BoxRef `json:"box"`
		Text string `json:"text"`
	}
	// BoxPath is a file inside the box.
	BoxPath struct {
		Box  BoxRef `json:"box"`
		Path string `json:"path"`
		// Tail reads only the file's last bytes, as many as one reply holds.
		Tail bool `json:"tail,omitempty"`
	}
	// FileResp is a file's bytes, or whether it exists.
	FileResp struct {
		Data   []byte `json:"data,omitempty"`
		Exists bool   `json:"exists"`
	}
	// ObserveResp is domain.AgentObservation on the wire.
	ObserveResp struct {
		Present  bool `json:"present"`
		Finished bool `json:"finished"`
		ExitCode int  `json:"exit_code"`
	}
	// InspectResp is ports.BoxState on the wire.
	InspectResp struct {
		Running   bool `json:"running"`
		OOMKilled bool `json:"oom_killed"`
	}
	// SaveReq asks for the box to be saved as a template within Budget.
	SaveReq struct {
		Box    BoxRef        `json:"box"`
		Budget time.Duration `json:"budget"`
	}
	// SaveResp is the saved template's handle on the machine.
	SaveResp struct {
		Handle string `json:"handle"`
	}
	// ScreenReq asks for the agent terminal's screen plus Lines of history above it.
	ScreenReq struct {
		Box   BoxRef `json:"box"`
		Lines int    `json:"lines"`
	}
	// ScreenResp is the terminal's text, empty when the box or the terminal is gone.
	ScreenResp struct {
		Text string `json:"text"`
	}
)

// MaxScreenLines bounds the history one box.screen reads, so the reply fits a link frame.
const MaxScreenLines = 2000

// Validate refuses a screen read without a box or with too much history.
func (r ScreenReq) Validate() error {
	if r.Lines < 0 || r.Lines > MaxScreenLines {
		return fmt.Errorf("screen lines must be 0 to %d", MaxScreenLines)
	}
	return r.Box.Validate()
}

// Validate refuses a request without a box.
func (r TypeReq) Validate() error { return r.Box.Validate() }

// Validate refuses a save without a box or a budget.
func (r SaveReq) Validate() error {
	if r.Budget <= 0 {
		return errors.New("save needs a budget")
	}
	return r.Box.Validate()
}

// Validate keeps box paths absolute and clean. ReadFile is capped by the handler, not here.
func (p BoxPath) Validate() error {
	if err := p.Box.Validate(); err != nil {
		return err
	}
	if !path.IsAbs(p.Path) || path.Clean(p.Path) != p.Path {
		return fmt.Errorf("box path %q must be absolute and clean", p.Path)
	}
	return nil
}

// WatchKind picks which signal a WatchLine carries.
type WatchKind string

// The kinds of WatchLine.
const (
	WatchReport WatchKind = "report"
	WatchExit   WatchKind = "exit"
	WatchBox    WatchKind = "box"
)

// WatchLine is one JSON line on the watch stream: one signal about one task's box. It is the wire
// form of domain.AgentReport, AgentExited and BoxEvent.
type WatchLine struct {
	Task   domain.TaskID       `json:"task"`
	Kind   WatchKind           `json:"kind"`
	Run    domain.AgentRun     `json:"run,omitempty"`
	Status domain.HCStatus     `json:"status,omitempty"`
	Phase  string              `json:"phase,omitempty"`
	Code   int                 `json:"code,omitempty"`
	Gen    domain.BoxGen       `json:"gen,omitempty"`
	Box    domain.BoxEventKind `json:"box,omitempty"`
	Up     bool                `json:"up,omitempty"`
	OOM    bool                `json:"oom,omitempty"`
}

// Signal turns a line back into the domain value. An unknown kind is a version skew, and the
// caller drops it.
func (l WatchLine) Signal() (domain.Signal, bool) {
	switch l.Kind {
	case WatchReport:
		return domain.AgentReport{Run: l.Run, Status: l.Status, Phase: l.Phase}, true
	case WatchExit:
		return domain.AgentExited{Run: l.Run, Code: l.Code}, true
	case WatchBox:
		return domain.BoxEvent{Gen: l.Gen, Kind: l.Box, StillRunning: l.Up, OOMKilled: l.OOM}, true
	}
	return nil, false
}

// Box ops, served by the machine agent. Each is idempotent and safe for a box that is half built,
// stopped or gone.
var (
	// BoxConverge submits or joins the machine's converge job for the box, then streams
	// ConvergeLine values until the job ends. A cut stream is rejoined by the caller, and the job
	// runs on meanwhile.
	BoxConverge = link.NewStreamOp[ConvergeReq, struct{}](cat, "box.converge", link.ServedByAgent)

	// BoxType, BoxInterrupt and BoxKill act on the agent terminal of the ref's run, and do nothing
	// when it is gone.
	BoxType      = link.NewOp[TypeReq, struct{}](cat, "box.type", link.ServedByAgent)
	BoxInterrupt = link.NewOp[BoxRef, struct{}](cat, "box.interrupt", link.ServedByAgent)
	BoxKill      = link.NewOp[BoxRef, struct{}](cat, "box.kill", link.ServedByAgent)

	// BoxObserve reports the agent terminal of the ref's run. BoxInspect reports the settled box.
	BoxObserve = link.NewOp[BoxRef, ObserveResp](cat, "box.observe", link.ServedByAgent)
	BoxInspect = link.NewOp[BoxRef, InspectResp](cat, "box.inspect", link.ServedByAgent)

	// BoxScreen reads the agent terminal of the ref's run as text: hc's terminal.read.
	BoxScreen = link.NewOp[ScreenReq, ScreenResp](cat, "box.screen", link.ServedByAgent)

	// BoxStop and BoxRelease preempt any job for the box first and wait for it to end.
	BoxStop    = link.NewOp[BoxRef, struct{}](cat, "box.stop", link.ServedByAgent)
	BoxRelease = link.NewOp[BoxRef, struct{}](cat, "box.release", link.ServedByAgent)

	// BoxSave submits or joins the save job and waits for its handle. The job outlives a cut
	// call, and the caller calls again to rejoin it.
	BoxSave = link.NewOp[SaveReq, SaveResp](cat, "box.save", link.ServedByAgent)

	// File access inside the box.
	BoxRead   = link.NewOp[BoxPath, FileResp](cat, "box.fs.read", link.ServedByAgent)
	BoxExists = link.NewOp[BoxPath, FileResp](cat, "box.fs.exists", link.ServedByAgent)
	BoxRemove = link.NewOp[BoxPath, struct{}](cat, "box.fs.remove", link.ServedByAgent)

	// BoxWatch is held open by the server with link.Hold, one per machine. Each opening sends the
	// current level of every box first, then edges, as WatchLine JSON lines.
	BoxWatch = link.NewResumableOp[struct{}, struct{}](cat, "box.watch", link.ServedByAgent)
)
