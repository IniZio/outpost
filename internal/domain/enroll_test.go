package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
)

func pairReq() domain.PairRequest {
	return domain.PairRequest{Key: domain.MachineKey{1}, Name: "engine", Role: domain.RoleSandbox, Slots: 2}
}

func begin(t *testing.T, r domain.PairRequest) domain.Enrollment {
	t.Helper()
	e, err := r.Begin([32]byte{9}, "BCDF-GHJK", t0, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestPairRequestBounds(t *testing.T) {
	long := make([]byte, 65)
	for i := range long {
		long[i] = 'a'
	}
	tests := []struct {
		name string
		edit func(*domain.PairRequest)
		want error
	}{
		{"valid", func(*domain.PairRequest) {}, nil},
		{"no key", func(r *domain.PairRequest) { r.Key = domain.MachineKey{} }, domain.ErrBadPairing},
		{"no name", func(r *domain.PairRequest) { r.Name = "" }, domain.ErrBadPairing},
		{"long name", func(r *domain.PairRequest) { r.Name = string(long) }, domain.ErrBadPairing},
		{"no role", func(r *domain.PairRequest) { r.Role = 0 }, domain.ErrBadPairing},
		{"no slots", func(r *domain.PairRequest) { r.Slots = 0 }, domain.ErrBadPairing},
		{"too many slots", func(r *domain.PairRequest) { r.Slots = 65 }, domain.ErrBadPairing},
		{"exec on a sandbox machine", func(r *domain.PairRequest) { r.Exec = true }, domain.ErrExecNotTool},
		{"exec on a tool machine", func(r *domain.PairRequest) { r.Role, r.Exec = domain.RoleTool, true }, nil},
	}
	for _, tc := range tests {
		r := pairReq()
		tc.edit(&r)
		if err := r.Validate(); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
		}
		if _, err := r.Begin([32]byte{}, "BCDF-GHJK", t0, time.Minute); !errors.Is(err, tc.want) {
			t.Errorf("%s: Begin got %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestEnrollmentHandsOutNothingBeforeItIsEnrolled(t *testing.T) {
	e := begin(t, pairReq())
	if _, ok, err := e.Collect(t0); ok || err != nil {
		t.Fatalf("a waiting attempt hands nothing out: %v %v", ok, err)
	}
	e, err := e.Confirm("alice", t0.Add(time.Minute))
	if err != nil || e.State != domain.EnrollEnrolling || e.Owner != "alice" {
		t.Fatalf("confirm: %+v %v", e, err)
	}
	if _, err := e.Confirm("mallory", t0.Add(time.Minute)); !errors.Is(err, domain.ErrEnrollUsed) {
		t.Fatalf("a second confirm must lose: %v", err)
	}
	if _, ok, _ := e.Collect(t0); ok {
		t.Fatal("an enrolling attempt hands nothing out")
	}
}

func TestEnrollmentIsCollectedOnce(t *testing.T) {
	e, _ := begin(t, pairReq()).Confirm("alice", t0)
	e = e.Enrolled("m1")
	next, ok, err := e.Collect(t0.Add(time.Hour))
	if !ok || err != nil || next.Machine != "m1" || next.State != domain.EnrollCollected {
		t.Fatalf("collect: %+v %v %v", next, ok, err)
	}
	if _, ok, _ := next.Collect(t0); ok {
		t.Fatal("the machine id is handed out once")
	}
	if got := e.Row(); got.Owner != "alice" || got.Name != "engine" || got.Slots != 2 || got.ID != "" {
		t.Fatalf("row: %+v", got)
	}
}

func TestEnrollmentFailureReturnsToWaiting(t *testing.T) {
	e, _ := begin(t, pairReq()).Confirm("alice", t0)
	e = e.Failed()
	if e.State != domain.EnrollWaiting || e.Owner != "" {
		t.Fatalf("failed: %+v", e)
	}
	if e.Enrolled("m1").State != domain.EnrollWaiting || e.Failed().State != domain.EnrollWaiting {
		t.Fatal("Enrolled and Failed do nothing unless enrolling")
	}
	if _, err := e.Confirm("alice", t0); err != nil {
		t.Fatalf("a failed write lets the person confirm again: %v", err)
	}
}

func TestEnrollmentExpiry(t *testing.T) {
	e := begin(t, pairReq())
	late := t0.Add(11 * time.Minute)
	if _, err := e.Confirm("alice", late); !errors.Is(err, domain.ErrEnrollExpired) {
		t.Fatalf("confirm after expiry: %v", err)
	}
	if _, _, err := e.Collect(late); !errors.Is(err, domain.ErrEnrollExpired) {
		t.Fatalf("poll after expiry: %v", err)
	}
	confirmed, _ := e.Confirm("alice", t0)
	if _, ok, err := confirmed.Enrolled("m1").Collect(late); !ok || err != nil {
		t.Fatalf("an enrolled machine is collected even after the code expired: %v %v", ok, err)
	}
}
