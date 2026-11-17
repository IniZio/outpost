package ops

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/link"
)

// SetupBuildReq is ports.SetupSpec on the wire, plus the caller's budget. The machine runs the
// build under that budget from its first submission, so a build whose callers all died still ends.
type SetupBuildReq struct {
	Repo    domain.RepoName   `json:"repo"`
	Rev     int               `json:"rev"`
	Commit  string            `json:"commit,omitempty"`
	Files   map[string]string `json:"files"`
	Base    string            `json:"base"`
	Profile [][]string        `json:"profile,omitempty"`
	Keep    []int             `json:"keep,omitempty"`
	Budget  time.Duration     `json:"budget"`
}

var (
	commitID = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	innerKey = regexp.MustCompile(`^[0-9a-f]{16,64}$`)
)

// Validate is the one place the build's rules live. Nothing here reaches a shell: the machine
// writes the files into a directory of its own and passes argv as lists.
func (r SetupBuildReq) Validate() error {
	if !repoName.MatchString(string(r.Repo)) || strings.Contains(string(r.Repo), "..") || r.Rev <= 0 || !imageRef.MatchString(r.Base) || r.Budget <= 0 {
		return errors.New("a setup build needs a repo, a revision, a base image and a budget")
	}
	if r.Commit != "" && !commitID.MatchString(r.Commit) {
		return fmt.Errorf("bad commit %q", r.Commit)
	}
	if slices.ContainsFunc(r.Profile, func(argv []string) bool { return len(argv) == 0 }) {
		return errors.New("a profile command is empty")
	}
	if slices.ContainsFunc(r.Keep, func(rev int) bool { return rev <= 0 }) {
		return errors.New("a kept revision is not positive")
	}
	return domain.ValidSetupFiles(r.Files) //nolint:wrapcheck // the domain's message is the reply
}

// ValidInner checks an inner images key, which the machine turns into a path of its own.
func ValidInner(key string) error {
	if key != "" && !innerKey.MatchString(key) {
		return fmt.Errorf("bad inner images key %q", key)
	}
	return nil
}

// SetupBuilt is ports.SetupBuilt on the wire.
type SetupBuilt struct {
	Image  string `json:"image"`
	Digest string `json:"digest"`
	Inner  string `json:"inner,omitempty"`
	Commit string `json:"commit"`
	Log    string `json:"log,omitempty"`
}

// SetupLine is one JSON line on the setup build stream. Exactly one field is set. The stream ends
// with Done or Err. An EOF before either means the link was cut, not that the build ended.
type SetupLine struct {
	Phase *Phase      `json:"phase,omitempty"`
	Done  *SetupBuilt `json:"done,omitempty"`
	Err   *link.Error `json:"err,omitempty"`
}

// SetupBuild submits or joins the machine's build of one repo revision, then streams SetupLine
// values until it ends. A cut stream is rejoined by the caller, and the build runs on meanwhile.
var SetupBuild = link.NewStreamOp[SetupBuildReq, struct{}](cat, "setup.build", link.ServedByAgent)
