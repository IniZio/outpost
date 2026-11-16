package portstest_test

import (
	"testing"

	"github.com/IniZio/outpost/internal/ports/portstest"
)

func TestFakeBoxesMeetTheBoxesContract(t *testing.T) {
	portstest.BoxesContract(t, func(*testing.T) portstest.BoxesRig {
		return portstest.BoxesRigFor(portstest.NewBoxes())
	})
}
