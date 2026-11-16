package domain

import (
	"fmt"
	"slices"
)

// rule is one invariant: ok must hold, or msg says what broke.
type rule struct {
	ok  bool
	msg string
}

func firstBroken(rules ...rule) error {
	if i := slices.IndexFunc(rules, func(r rule) bool { return !r.ok }); i >= 0 {
		return fmt.Errorf("%w: %s", ErrCorrupt, rules[i].msg)
	}
	return nil
}

// Validate checks every invariant a persisted or produced Task must hold. Restore calls it at
// the store boundary. The property tests call it after every Apply.
func (t Task) Validate() error {
	if err := firstBroken(
		rule{t.Version > 0, "version is zero"},
		rule{t.Kind.Valid(), "unknown kind"},
		rule{t.Owner != "" && t.Spec.Repo != "", "no owner or repo"},
	); err != nil {
		return err
	}
	switch l := t.Life.(type) {
	case Queued:
		return nil
	case Starting:
		return t.validateStarting(l)
	case Active:
		return t.validateActive(l)
	case Closing:
		return t.validateClosing(l)
	case Ended:
		return t.validateEnded(l)
	}
	return fmt.Errorf("%w: no life", ErrCorrupt)
}

func (t Task) asked(id AskID) bool { return id != 0 && id <= t.NextAsk }

func (t Task) keyed() rule {
	return rule{t.AgentRun > 0 && t.BoxGen > 0, "a running task has no agent run or box generation"}
}

func (t Task) validateStarting(s Starting) error {
	earlyOK := !slices.ContainsFunc(s.Early, func(e Early) bool { return (e.Report == nil) == (e.Exit == nil) })
	return firstBroken(
		t.keyed(),
		rule{t.asked(s.Ask), "starting ask not issued"},
		rule{earlyOK, "buffered event is not exactly one report or exit"},
		rule{s.Resume != nil || len(s.Messages) == 0, "a message is parked without a resume"},
		rule{s.Resume == nil || s.Resume.Outcome.Valid() && !s.Resume.IsReleased(), "resume of an unresumable run"},
	)
}

func (t Task) validateActive(a Active) error {
	unattended := t.Kind.Unattended()
	return firstBroken(
		t.keyed(),
		rule{a.Turn.State >= TurnPending && a.Turn.State <= TurnBlocked, "unknown turn state"},
		rule{(a.Turn.State == TurnRunning) == !a.Turn.WorkingSince.IsZero(), "working since set outside a running turn"},
		rule{a.Turn.State == TurnRunning || a.Turn.Phase == "", "phase set outside a running turn"},
		rule{!a.ladderArmed() || unattended && a.Turn.idle(), "nudge armed outside an idle unattended run"},
		rule{!unattended || !a.Turn.idle() || a.Box.Want != BoxUp || a.ladderArmed() || a.Asks[AskDoneMarker].ID != 0, "an idle unattended run has nothing that would end it"},
		rule{(a.Box.Want == BoxReplacing) == a.Deadlines.Armed(DlReplace), "replace deadline and box want disagree"},
		rule{!a.Deadlines.Armed(DlSleep) || !unattended && a.status(t.Kind) == StatusWaiting && a.Box.Want == BoxUp, "sleep armed outside a waiting attended run with the box up"},
		rule{a.Deadlines.Armed(DlTimeout) == (t.Spec.Timeout > 0), "timeout deadline and spec disagree"},
		t.validateBox(a),
		t.validateAsks(a),
	)
}

func (a Active) ladderArmed() bool {
	return a.Deadlines.Armed(DlNudge) || a.Deadlines.Armed(DlGiveUp)
}

func (t Task) validateBox(a Active) rule {
	b := a.Box
	switch b.Want {
	case BoxUp, BoxAsleep, BoxReplacing, BoxWaking:
	case BoxNone, BoxStopped, BoxReleased:
		return rule{false, "an active task's box cannot be none, stopped or released"}
	}
	return firstRule(
		rule{b.Lost == "" || b.Want == BoxReplacing || b.Want == BoxWaking, "lost reason outside a replacement"},
		rule{len(b.Parked) == 0 || b.Want != BoxUp, "messages parked while the box is up"},
		rule{b.Tries == 0 || b.Want == BoxReplacing || b.Want == BoxWaking, "replacement tries outside a replacement"},
		rule{!t.Kind.Unattended() || b.Want != BoxAsleep, "an unattended run's box is asleep with nobody to wake it"},
	)
}

func firstRule(rules ...rule) rule {
	if i := slices.IndexFunc(rules, func(r rule) bool { return !r.ok }); i >= 0 {
		return rules[i]
	}
	return rule{true, ""}
}

func (t Task) validateAsks(a Active) rule {
	issued := !slices.ContainsFunc(a.Asks[:], func(k Ask) bool { return k.ID != 0 && !t.asked(k.ID) })
	return firstRule(
		rule{issued, "an ask id was never issued"},
		rule{(a.Asks[AskEnsure].ID != 0) == (a.Box.Want == BoxWaking), "ensure ask and box want disagree"},
		rule{a.Asks[AskDoneMarker].ID == 0 || a.Turn.idle(), "done marker ask outstanding while the agent works"},
		rule{a.Asks[AskInspect].ID == 0 || a.Box.Want == BoxUp, "box inspect outstanding while the box is not up"},
	)
}

func (t Task) validateClosing(c Closing) error {
	return firstBroken(
		t.keyed(),
		rule{t.asked(c.Ask), "closing ask not issued"},
		rule{c.Want.Valid(), "unknown outcome"},
		rule{c.From >= StatusQueued && c.From <= StatusStopped, "unknown status shown while closing"},
		rule{c.Step == CloseSummary || c.Step == CloseTemplate && c.Want == OutcomeDone && t.Kind.SavesTemplate(), "closing step does not fit the task"},
		rule{!c.Until.IsZero(), "closing has no deadline"},
		rule{c.Ensure == 0 || t.asked(c.Ensure), "closing waits for an ask never issued"},
	)
}

func (t Task) validateEnded(e Ended) error {
	return firstBroken(
		rule{e.Outcome.Valid(), "unknown outcome"},
		rule{!e.At.IsZero(), "ended without a time"},
		rule{!e.BoxKept || e.Outcome == OutcomeFailed && t.Kind.SavesTemplate(), "box kept on a run that does not save a template"},
		rule{e.Ensure == 0 || t.asked(e.Ensure), "ended waits for an ask never issued"},
	)
}
