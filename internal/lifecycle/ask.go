package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

// answer is the event that answers an ask, and the repo row its commit writes when Apply accepts
// it. Only an accepted template save has a row.
type answer struct {
	ev       domain.Event
	template *ports.TemplateRow
}

var errNotAsk = errors.New("lifecycle: effect has no answer")

// ask carries out one effect that has an answer and returns it. It never returns without one for an
// ask: a port error becomes domain.FailedAnswer, which owns the neutral value for the answers with
// no Err field.
func (r *Runner) ask(ctx context.Context, it item) answer {
	ans, err := r.try(ctx, it)
	if err == nil {
		return ans
	}
	ev, _ := domain.FailedAnswer(it.effect, errText(ctx, err))
	return answer{ev: ev}
}

func (r *Runner) try(ctx context.Context, it item) (answer, error) {
	t, b := it.task, r.cfg.Boxes
	switch e := it.effect.(type) {
	case domain.StartTask:
		return r.start(ctx, t, e)
	case domain.EnsureRunning:
		return r.ensure(ctx, t, e)
	case domain.ProbeDoneMarker:
		present, err := b.Exists(ctx, target(t), domain.DoneMarkerPath)
		return answer{ev: domain.DoneMarker{Ask: e.Ask, Present: present}}, wrapPort("probe", err)
	case domain.ReadSummary:
		text, err := r.cfg.Agent.LastMessage(ctx, b, target(t))
		return answer{ev: domain.SummaryRead{Ask: e.Ask, Text: text}}, wrapPort("read summary", err)
	case domain.SaveTemplate:
		return r.saveTemplate(ctx, t, e)
	case domain.InspectBox:
		tg := target(t)
		tg.Gen = e.Gen
		st, err := b.Inspect(ctx, tg)
		return answer{ev: domain.BoxInspected{Ask: e.Ask, Running: st.Running, OOMKilled: st.OOMKilled}}, wrapPort("inspect", err)
	case domain.AbortStart, domain.SendToAgent, domain.InterruptAgent, domain.KillAgent,
		domain.ClearDoneMarker, domain.StopBox, domain.Release, domain.RunScheduler:
	}
	return answer{}, errNotAsk
}

func wrapPort(what string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("lifecycle: %s: %w", what, err)
}

// start answers StartTask. Composing it is data, not steps: lifecycle builds the Plan and the box
// side owns the order of image pull, volumes, clones and agent launch. Each phase the box reports
// is applied as Progressed before Converge returns. The task's lock is free while Converge runs,
// because the job is not inside step.
func (r *Runner) start(ctx context.Context, t domain.Task, e domain.StartTask) (answer, error) {
	p, err := r.plan(ctx, t, e.Run, e.Gen, true)
	if err != nil {
		return answer{}, err
	}
	progress := func(phase, note string) {
		ev := domain.Progressed{Ask: e.Ask, Phase: phase, Note: note}
		if _, err := r.step(r.base, t.ID, ev); err != nil {
			r.cfg.Log.WarnContext(ctx, "progress not applied", "task", t.ID, "err", err)
		}
	}
	if _, err := r.cfg.Boxes.Converge(ctx, p, progress); err != nil {
		return answer{}, fmt.Errorf("lifecycle: start: %w", err)
	}
	return answer{ev: domain.Started{Ask: e.Ask}}, nil
}

// ensure answers EnsureRunning: the same Converge with a resume Plan, which reuses the volumes.
func (r *Runner) ensure(ctx context.Context, t domain.Task, e domain.EnsureRunning) (answer, error) {
	p, err := r.plan(ctx, t, e.Run, e.Gen, false)
	if err != nil {
		return answer{}, err
	}
	got, err := r.cfg.Boxes.Converge(ctx, p, nil)
	if err != nil {
		return answer{}, fmt.Errorf("lifecycle: ensure: %w", err)
	}
	return answer{ev: domain.Ensured{Ask: e.Ask, Revived: got.Revived}}, nil
}

// saveTemplate answers SaveTemplate, with the repo row to write if the domain accepts the answer.
func (r *Runner) saveTemplate(ctx context.Context, t domain.Task, e domain.SaveTemplate) (answer, error) {
	notes, _ := r.cfg.Boxes.ReadFile(ctx, target(t), domain.NotesPath) // no notes keeps the old ones
	tpl, err := r.cfg.Boxes.SaveTemplate(ctx, target(t))
	if err != nil {
		return answer{}, fmt.Errorf("lifecycle: save template: %w", err)
	}
	row := &ports.TemplateRow{Repo: t.Spec.Repo, Template: tpl, At: r.cfg.Clock.Now(), Notes: strings.TrimSpace(string(notes))}
	return answer{ev: domain.TemplateSaved{Ask: e.Ask, Tag: tpl.Handle}, template: row}, nil
}

// plan assembles the Plan Converge makes real. The image and the prompt are the domain's rules.
func (r *Runner) plan(ctx context.Context, t domain.Task, run domain.AgentRun, gen domain.BoxGen, fresh bool) (ports.Plan, error) {
	repo, err := r.cfg.Store.Repo(ctx, t.Spec.Repo)
	if err != nil {
		return ports.Plan{}, fmt.Errorf("lifecycle: repo %s: %w", t.Spec.Repo, err)
	}
	tg := ports.Target{Task: t.ID, Machine: t.Machine, Run: run, Gen: gen}
	repos := append([]domain.RepoName{t.Spec.Repo}, t.Spec.Also...)
	f := ports.LaunchFor{Target: tg, Owner: t.Owner, Resume: !fresh, Repos: repos}
	if fresh {
		f.Prompt = domain.LaunchPrompt(t, repo)
	}
	launch, err := r.cfg.Agent.Launch(ctx, f)
	if err != nil {
		return ports.Plan{}, fmt.Errorf("lifecycle: agent launch: %w", err)
	}
	creds, err := r.cfg.Secrets.Credentials(ctx, tg, t.Owner, repos)
	if err != nil {
		return ports.Plan{}, fmt.Errorf("lifecycle: credentials: %w", err)
	}
	env := maps.Clone(creds.Env)
	if env == nil {
		env = map[string]string{}
	}
	maps.Copy(env, launch.Env)
	return ports.Plan{
		Target: tg, Image: repo.ReadyFor(t.Kind, r.cfg.BaseImage, t.Machine), Fresh: fresh,
		Repos: repos, Env: env, CA: creds.CA, Launch: launch,
	}, nil
}

// errText is the Err of a failed answer: a budget message when the effect's deadline passed, and
// the port's own text otherwise.
func errText(ctx context.Context, err error) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timed out"
	}
	if text := err.Error(); text != "" {
		return text
	}
	return "failed"
}
