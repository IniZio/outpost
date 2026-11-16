package domain

import (
	"encoding/json"
	"fmt"
	"time"
)

// Record is the flat shape the store keeps. Columns the queue and the UI query are real fields.
// The rest of the Life variant is one JSON payload, so a new field in Active changes no schema.
// Status is derived on every write and exists only so SQL can filter on it. Wake is the task's
// NextDeadline, zero when none.
type Record struct {
	ID        TaskID
	Version   uint64
	Owner     Login
	Kind      Kind
	Machine   MachineID
	CreatedAt time.Time
	NextAsk   AskID
	AgentRun  AgentRun
	BoxGen    BoxGen
	Spec      Spec
	Phase     LifePhase
	Status    Status
	Wake      time.Time
	Payload   json.RawMessage
}

// LifePhase tags the Life variant in a Record.
type LifePhase string

const (
	PhaseQueued   LifePhase = "queued"
	PhaseStarting LifePhase = "starting"
	PhaseActive   LifePhase = "active"
	PhaseClosing  LifePhase = "closing"
	PhaseEnded    LifePhase = "ended"
)

// Record flattens the task for the store. The due-task query after a restart is one index on Wake.
func (t Task) Record() Record {
	phase, payload := encodeLife(t.Life)
	wake, _ := t.NextDeadline()
	return Record{
		ID: t.ID, Version: t.Version, Owner: t.Owner, Kind: t.Kind, Machine: t.Machine,
		CreatedAt: t.CreatedAt, NextAsk: t.NextAsk, AgentRun: t.AgentRun, BoxGen: t.BoxGen,
		Spec: t.Spec, Phase: phase, Status: t.Status(), Wake: wake, Payload: payload,
	}
}

// encodeLife marshals a Life variant. Marshalling these plain structs fails only for a year
// outside 0..9999, which Apply never produces from a real clock, so a failure is a bug.
func encodeLife(l Life) (LifePhase, json.RawMessage) {
	var phase LifePhase
	switch l.(type) {
	case Queued:
		phase = PhaseQueued
	case Starting:
		phase = PhaseStarting
	case Active:
		phase = PhaseActive
	case Closing:
		phase = PhaseClosing
	case Ended:
		phase = PhaseEnded
	}
	payload, err := json.Marshal(l)
	if err != nil {
		panic(fmt.Sprintf("domain: encode %s life: %v", phase, err))
	}
	return phase, payload
}

// Restore is the parse at the store boundary. It decodes Payload by Phase, then runs Validate and
// checks the derived Status column, so a corrupt row fails here and not three calls later.
func Restore(r Record) (Task, error) {
	life, err := decodeLife(r.Phase, r.Payload)
	if err != nil {
		return Task{}, err
	}
	t := Task{
		ID: r.ID, Version: r.Version, Owner: r.Owner, Kind: r.Kind, Spec: r.Spec, Machine: r.Machine,
		CreatedAt: r.CreatedAt, NextAsk: r.NextAsk, AgentRun: r.AgentRun, BoxGen: r.BoxGen, Life: life,
	}
	if err := t.Validate(); err != nil {
		return Task{}, err
	}
	if t.Status() != r.Status {
		return Task{}, fmt.Errorf("%w: status column %d, derived %d", ErrCorrupt, r.Status, t.Status())
	}
	return t, nil
}

func decodeLife(phase LifePhase, payload json.RawMessage) (Life, error) {
	switch phase {
	case PhaseQueued:
		return decode[Queued](payload)
	case PhaseStarting:
		return decode[Starting](payload)
	case PhaseActive:
		return decode[Active](payload)
	case PhaseClosing:
		return decode[Closing](payload)
	case PhaseEnded:
		return decode[Ended](payload)
	}
	return nil, fmt.Errorf("%w: unknown life phase %q", ErrCorrupt, phase)
}

func decode[L Life](payload json.RawMessage) (Life, error) {
	var l L
	if err := json.Unmarshal(payload, &l); err != nil {
		return nil, fmt.Errorf("%w: life payload: %w", ErrCorrupt, err)
	}
	return l, nil
}
