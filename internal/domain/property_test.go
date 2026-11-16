package domain_test

import (
	"encoding/json"
	"errors"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
)

const (
	streams       = 200
	stepsInStream = 80
)

// fuzzer feeds one task a random stream of events and checks, after every Apply, the properties
// the lifecycle relies on: the input is never mutated, a dropped event changes nothing, every
// produced task is valid and survives the store, and a duplicate delivery is dropped or harmless.
type fuzzer struct {
	t       *testing.T
	rng     *rand.Rand
	task    domain.Task
	now     time.Time
	policy  domain.Policy
	effects []domain.Effect
	reached map[string]bool
	applied int
}

func newFuzzer(t *testing.T, seed uint64) *fuzzer {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	f := &fuzzer{t: t, rng: rng, now: t0, policy: domain.DefaultPolicy(), reached: map[string]bool{}}
	f.fresh()
	return f
}

// fresh starts over with a new queued task, as the scheduler would once a computer is released.
func (f *fuzzer) fresh() {
	kind := pick(f.rng, domain.KindInteractive, domain.KindScheduled, domain.KindSetup)
	sp := domain.Spec{Repo: "acme/app", Schedule: 1}
	if kind == domain.KindInteractive {
		sp.Schedule = 0
	}
	if f.rng.IntN(2) == 0 {
		sp.Timeout = 20 * time.Minute
	}
	f.task, f.effects = newTask(1, kind, sp), nil
}

func pick[T any](rng *rand.Rand, items ...T) T { return items[rng.IntN(len(items))] }

func (f *fuzzer) run() domain.AgentRun {
	cur := f.task.AgentRun
	switch n := f.rng.IntN(20); {
	case n == 0 && cur > 0:
		return cur - 1
	case n == 1:
		return cur + 1
	}
	return cur
}

func (f *fuzzer) gen() domain.BoxGen {
	if cur := f.task.BoxGen; f.rng.IntN(10) == 0 && cur > 0 {
		return cur - 1
	}
	return f.task.BoxGen
}

func (f *fuzzer) who() domain.Login {
	if f.rng.IntN(15) == 0 {
		return stranger
	}
	return owner
}

// event draws the next event. Answers are built from effects the task really emitted, and
// sometimes get the wrong ask, so both the fresh and the stale paths are exercised.
func (f *fuzzer) event() domain.Event {
	if f.rng.IntN(2) == 0 {
		return f.guided()
	}
	switch n := f.rng.IntN(100); {
	case n < 10:
		return domain.SlotGranted{Machine: "m1"}
	case n < 24:
		return domain.Tick{}
	case n < 42:
		return domain.AgentReport{Run: f.run(), Status: domain.HCStatus(1 + f.rng.IntN(5)), Phase: pick(f.rng, "", "Editing", "Testing")}
	case n < 46:
		return domain.AgentExited{Run: f.run(), Code: pick(f.rng, 0, 1, 137)}
	case n < 54:
		return domain.BoxEvent{Gen: f.gen(), Kind: domain.BoxEventKind(1 + f.rng.IntN(3)), StillRunning: f.rng.IntN(2) == 0, OOMKilled: f.rng.IntN(3) == 0}
	case n < 58:
		return domain.Recovered{Agent: domain.AgentObservation{Present: f.rng.IntN(5) > 0, Finished: f.rng.IntN(4) == 0, ExitCode: f.rng.IntN(2)}}
	case n < 66:
		return f.command()
	}
	return f.answer()
}

func (f *fuzzer) command() domain.Event {
	switch f.rng.IntN(8) {
	case 0, 1, 2, 3:
		return domain.UserMessage{By: f.who(), Text: pick(f.rng, "continue", "stop that", "thanks")}
	case 4, 5:
		return domain.UserStop{By: f.who()}
	case 6:
		return domain.UserResume{By: f.who()}
	}
	return domain.SaveTemplateRequested{By: f.who()}
}

// guided draws the event that moves a task in its current life along, so the streams reach
// the states that pure noise rarely does.
func (f *fuzzer) guided() domain.Event {
	switch f.task.Life.(type) {
	case domain.Queued:
		return domain.SlotGranted{Machine: "m1"}
	case domain.Active:
		if f.rng.IntN(2) == 0 {
			return domain.AgentReport{Run: f.task.AgentRun, Status: pick(f.rng, domain.HCWorking, domain.HCIdle, domain.HCBlocked), Phase: pick(f.rng, "", "Editing")}
		}
	case domain.Ended:
		if f.rng.IntN(2) == 0 {
			return pick[domain.Event](f.rng, domain.UserResume{By: owner}, domain.UserMessage{By: owner, Text: "carry on"}, domain.SaveTemplateRequested{By: owner})
		}
	case domain.Starting, domain.Closing:
	}
	return f.answerTo(f.latestAsk(), 0)
}

func (f *fuzzer) latestAsk() domain.Effect {
	for i := len(f.effects) - 1; i >= 0; i-- {
		switch f.effects[i].(type) {
		case domain.StartTask, domain.EnsureRunning, domain.ProbeDoneMarker, domain.ReadSummary, domain.SaveTemplate:
			return f.effects[i]
		}
	}
	return nil
}

func (f *fuzzer) answer() domain.Event {
	if len(f.effects) == 0 {
		return domain.Tick{}
	}
	return f.answerTo(f.effects[f.rng.IntN(len(f.effects))], domain.AskID(f.rng.IntN(3))*100)
}

// answerTo builds the answer the lifecycle would feed back for fx. A nonzero wrong makes it
// answer a question that was never asked.
func (f *fuzzer) answerTo(fx domain.Effect, wrong domain.AskID) domain.Event {
	flip, err := f.rng.IntN(2) == 0, pick(f.rng, "", "", "boom")
	switch fx := fx.(type) {
	case domain.StartTask:
		return pick[domain.Event](f.rng, domain.Started{Ask: fx.Ask + wrong}, domain.Started{Ask: fx.Ask + wrong}, domain.StartFailed{Ask: fx.Ask + wrong, Err: "clone failed"}, domain.Progressed{Ask: fx.Ask + wrong, Phase: "Cloning", Note: "Cloning"})
	case domain.EnsureRunning:
		return domain.Ensured{Ask: fx.Ask + wrong, Revived: flip, Err: err}
	case domain.ProbeDoneMarker:
		return domain.DoneMarker{Ask: fx.Ask + wrong, Present: flip}
	case domain.ReadSummary:
		return domain.SummaryRead{Ask: fx.Ask + wrong, Text: pick(f.rng, "", "All done.")}
	case domain.SaveTemplate:
		return domain.TemplateSaved{Ask: fx.Ask + wrong, Tag: "ready-1", Err: err}
	}
	return domain.Tick{}
}

func (f *fuzzer) advance() {
	f.now = f.now.Add(pick(f.rng, 0, 0, time.Second, 5*time.Second, 3*time.Minute, 11*time.Minute, 31*time.Minute, 3*time.Hour, 25*time.Hour))
}

func recordJSON(t *testing.T, task domain.Task) string {
	t.Helper()
	b, err := json.Marshal(task.Record())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// sameTask compares two tasks as the store would see them, ignoring the version.
func sameTask(t *testing.T, a, b domain.Task) bool {
	t.Helper()
	a.Version, b.Version = 0, 0
	return recordJSON(t, a) == recordJSON(t, b)
}

func isRefusal(err error) bool {
	return errors.Is(err, domain.ErrForbidden) || errors.Is(err, domain.ErrNotReady) ||
		errors.Is(err, domain.ErrClosing) || errors.Is(err, domain.ErrReleased)
}

func (f *fuzzer) step() {
	f.t.Helper()
	if e, ok := f.task.Life.(domain.Ended); ok && e.IsReleased() {
		f.fresh()
	}
	f.advance()
	ev := f.event()
	before := recordJSON(f.t, f.task)
	res, err := domain.Apply(f.task, ev, f.now, f.policy)
	if recordJSON(f.t, f.task) != before {
		f.t.Fatalf("Apply(%T) mutated its input", ev)
	}
	switch {
	case err != nil:
		if !isRefusal(err) {
			f.t.Fatalf("Apply(%T) failed with %v, want a refusal", ev, err)
		}
	case res.Dropped != domain.Applied:
		f.checkDropped(ev, res)
	default:
		f.checkApplied(ev, res)
	}
}

func (f *fuzzer) checkDropped(ev domain.Event, res domain.Result) {
	f.t.Helper()
	if !sameTask(f.t, res.Task, f.task) || res.Task.Version != f.task.Version || len(res.Effects)+len(res.Timeline) != 0 {
		f.t.Fatalf("a dropped %T (%v) changed something: %+v", ev, res.Dropped, res)
	}
}

func (f *fuzzer) checkApplied(ev domain.Event, res domain.Result) {
	f.t.Helper()
	f.applied++
	if err := res.Task.Validate(); err != nil {
		f.t.Fatalf("Apply(%T) made an invalid task: %v\n%+v", ev, err, res.Task)
	}
	if res.Task.Version != f.task.Version+1 {
		f.t.Fatalf("version %d after %d", res.Task.Version, f.task.Version)
	}
	back, err := domain.Restore(res.Task.Record())
	if err != nil || !sameTask(f.t, back, res.Task) || back.Version != res.Task.Version {
		f.t.Fatalf("Apply(%T) made a task the store cannot round-trip: %v", ev, err)
	}
	f.checkDuplicate(ev, res.Task)
	f.checkBoxStopped(ev, res)
	f.task = res.Task
	f.effects = append(f.effects, res.Effects...)
	f.effects = f.effects[max(0, len(f.effects)-16):]
	f.reached[reachKey(res.Task)] = true
}

// checkBoxStopped requires a step that abandons a wake, or takes a wake's late answer, to stop
// the box again whenever the ended task wants it stopped.
func (f *fuzzer) checkBoxStopped(ev domain.Event, res domain.Result) {
	f.t.Helper()
	s, wasWaking := f.task.Life.(domain.Starting)
	_, wasEnded := f.task.Life.(domain.Ended)
	_, isEnsured := ev.(domain.Ensured)
	_, endedNow := res.Task.Life.(domain.Ended)
	abandoned := wasWaking && s.Resume != nil && endedNow
	if (abandoned || wasEnded && isEnsured) && res.Task.BoxWant() == domain.BoxStopped && !has[domain.StopBox](res.Effects) {
		f.t.Fatalf("%T left a woken box running: effects %q", ev, kinds(res.Effects))
	}
}

// checkDuplicate delivers the same event again at the same instant. A system event must then be
// dropped or leave the task as it was. A person's message is exempt: two messages are two messages.
func (f *fuzzer) checkDuplicate(ev domain.Event, after domain.Task) {
	f.t.Helper()
	if _, isMessage := ev.(domain.UserMessage); isMessage {
		return
	}
	again, err := domain.Apply(after, ev, f.now, f.policy)
	if err != nil || again.Dropped != domain.Applied {
		return
	}
	if !sameTask(f.t, again.Task, after) {
		f.t.Fatalf("delivering %T twice changed the task the second time:\n%s\n%s", ev, recordJSON(f.t, after), recordJSON(f.t, again.Task))
	}
}

func reachKey(t domain.Task) string {
	return string(t.Record().Phase) + "/" + t.Status().String() + "/" + t.BoxWant().String()
}

func TestRandomEventStreamsKeepEveryInvariant(t *testing.T) {
	reached := map[string]bool{}
	var applied, total int
	for seed := range uint64(streams) {
		f := newFuzzer(t, seed)
		for range stepsInStream {
			f.step()
			total++
		}
		applied += f.applied
		for k := range f.reached {
			reached[k] = true
		}
	}
	if applied < total/4 {
		t.Errorf("only %d of %d random events applied: the generator is not reaching the machine", applied, total)
	}
	for _, want := range []string{"starting/starting/up", "active/working/up", "active/waiting/up", "active/waiting/asleep", "active/working/replacing", "active/working/waking", "closing/working/up", "ended/done/stopped", "ended/failed/stopped", "ended/stopped/stopped"} {
		if !reached[want] {
			t.Errorf("no stream reached %s; reached %v", want, sortedKeys(reached))
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
