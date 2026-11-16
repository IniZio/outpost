package domain_test

import (
	"time"

	"github.com/IniZio/outpost/internal/domain"
)

const (
	owner    domain.Login = "alice"
	stranger domain.Login = "mallory"
)

var t0 = time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)

func spec() domain.Spec { return domain.Spec{Repo: "acme/app", Prompt: "upgrade the dependencies"} }

func newTask(id domain.TaskID, kind domain.Kind, sp domain.Spec) domain.Task {
	if kind == domain.KindScheduled && sp.Schedule == 0 {
		sp.Schedule = 1
	}
	res, err := domain.New(id, owner, kind, sp, t0)
	if err != nil {
		panic(err)
	}
	return res.Task
}

// fixture is a stored task with the given life, for tests that start mid-run.
func fixture(kind domain.Kind, life domain.Life) domain.Task {
	t := newTask(1, kind, spec())
	t.Life = life
	t.NextAsk, t.AgentRun, t.BoxGen = 9, 2, 2
	return t
}

func active(turn domain.TurnState) domain.Active {
	return domain.Active{
		Since: t0,
		Turn:  domain.Turn{State: turn},
		Box:   domain.Box{Want: domain.BoxUp, StartedAt: t0},
	}
}
