package domain_test

import (
	"testing"

	"github.com/IniZio/outpost/internal/domain"
)

func TestStatusAndBoxWantHaveNames(t *testing.T) {
	if domain.StatusWorking.String() != "working" || domain.Status(99).String() != "unknown" {
		t.Errorf("status names: %q, %q", domain.StatusWorking, domain.Status(99))
	}
	if domain.BoxAsleep.String() != "asleep" || domain.BoxWant(99).String() != "unknown" {
		t.Errorf("box names: %q, %q", domain.BoxAsleep, domain.BoxWant(99))
	}
}
