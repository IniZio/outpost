package domain_test

import (
	"strings"
	"testing"

	"github.com/IniZio/outpost/internal/domain"
)

// every names one of each effect, so a classification test covers the whole sum.
func everyEffect() []domain.Effect {
	return []domain.Effect{
		domain.StartTask{Ask: 1, Run: 1, Gen: 1},
		domain.AbortStart{},
		domain.EnsureRunning{Ask: 2},
		domain.SendToAgent{Text: "hi"},
		domain.InterruptAgent{},
		domain.KillAgent{},
		domain.ClearDoneMarker{},
		domain.ProbeDoneMarker{Ask: 3},
		domain.ReadSummary{Ask: 4},
		domain.SaveTemplate{Ask: 5},
		domain.StopBox{},
		domain.InspectBox{Ask: 6},
		domain.Release{},
		domain.RunScheduler{},
	}
}

func TestMetaClassifiesEveryEffect(t *testing.T) {
	want := map[string]domain.EffectMeta{
		"StartTask":       {Ask: 1, Delivery: domain.DeliverAsked, Abortable: true},
		"EnsureRunning":   {Ask: 2, Delivery: domain.DeliverAsked},
		"ProbeDoneMarker": {Ask: 3, Delivery: domain.DeliverAsked},
		"ReadSummary":     {Ask: 4, Delivery: domain.DeliverAsked},
		"SaveTemplate":    {Ask: 5, Delivery: domain.DeliverAsked},
		"InspectBox":      {Ask: 6, Delivery: domain.DeliverAsked},
		"SendToAgent":     {Delivery: domain.DeliverReplay},
		"InterruptAgent":  {Delivery: domain.DeliverReplay},
		"KillAgent":       {Delivery: domain.DeliverReplay},
		"ClearDoneMarker": {Delivery: domain.DeliverReplay},
		"StopBox":         {Delivery: domain.DeliverReplay},
		"Release":         {Delivery: domain.DeliverReplay},
		"AbortStart":      {Delivery: domain.DeliverOnce},
		"RunScheduler":    {Delivery: domain.DeliverOnce},
	}
	for _, e := range everyEffect() {
		name := kinds([]domain.Effect{e})
		if got := domain.Meta(e); got != want[name] {
			t.Errorf("Meta(%s) = %+v, want %+v", name, got, want[name])
		}
	}
}

func TestFailedAnswerAnswersOnlyAskedEffects(t *testing.T) {
	for _, e := range everyEffect() {
		ev, ok := domain.FailedAnswer(e, "boom")
		if asked := domain.Meta(e).Delivery == domain.DeliverAsked; ok != asked {
			t.Errorf("FailedAnswer(%s) ok = %v, but asked = %v", kinds([]domain.Effect{e}), ok, asked)
		}
		if ok && kinds([]domain.Effect{e}) == "StartTask" {
			if f, isFailed := ev.(domain.StartFailed); !isFailed || f.Err != "boom" || f.Ask != 1 {
				t.Errorf("FailedAnswer(StartTask) = %+v", ev)
			}
		}
	}
}

func TestOnlyPeopleSendCommandsAndOnlyMachinesSendSignals(t *testing.T) {
	commands := []domain.Command{domain.UserMessage{}, domain.UserStop{}, domain.UserResume{}, domain.SaveTemplateRequested{}}
	signals := []domain.Signal{domain.AgentReport{}, domain.AgentExited{}, domain.BoxEvent{}}
	for _, c := range commands {
		if _, isSignal := c.(domain.Signal); isSignal {
			t.Errorf("%T is both a command and a signal", c)
		}
	}
	for _, s := range signals {
		if _, isCommand := s.(domain.Command); isCommand {
			t.Errorf("%T is both a command and a signal", s)
		}
	}
}

func TestOnlyAnActiveTaskWithItsBoxUpNeedsObserving(t *testing.T) {
	asleep := active(domain.TurnPaused)
	asleep.Box = domain.Box{Want: domain.BoxAsleep}
	tests := []struct {
		name string
		task domain.Task
		want bool
	}{
		{"box up", fixture(domain.KindInteractive, active(domain.TurnRunning)), true},
		{"box asleep", fixture(domain.KindInteractive, asleep), false},
		{"starting", fixture(domain.KindInteractive, domain.Starting{Ask: 1}), false},
		{"queued", newTask(1, domain.KindInteractive, spec()), false},
	}
	for _, tc := range tests {
		if got := tc.task.NeedsObservation(); got != tc.want {
			t.Errorf("%s: NeedsObservation = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestAnEndedTaskIsUnfinishedOnlyWhileItWaitsForAWake(t *testing.T) {
	ended := domain.Ended{Outcome: domain.OutcomeDone, At: t0}
	waiting := ended
	waiting.Ensure = 4
	tests := []struct {
		name string
		task domain.Task
		want bool
	}{
		{"queued", newTask(1, domain.KindInteractive, spec()), true},
		{"closing", fixture(domain.KindInteractive, domain.Closing{Want: domain.OutcomeDone}), true},
		{"ended", fixture(domain.KindInteractive, ended), false},
		{"ended with a wake outstanding", fixture(domain.KindInteractive, waiting), true},
	}
	for _, tc := range tests {
		if got := tc.task.Unfinished(); got != tc.want {
			t.Errorf("%s: Unfinished = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestTheNudgeNamesTheDoneMarker(t *testing.T) {
	if !strings.Contains(domain.NudgeText, domain.DoneMarkerPath) {
		t.Errorf("the nudge %q must name %s", domain.NudgeText, domain.DoneMarkerPath)
	}
}
