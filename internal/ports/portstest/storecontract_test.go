package portstest_test

import (
	"testing"

	"github.com/IniZio/outpost/internal/ports/portstest"
)

func TestFakeStoreMeetsTheStoreContract(t *testing.T) {
	portstest.StoreContract(t, func(*testing.T) portstest.StoreRig {
		return portstest.StoreRigFor(portstest.NewStore())
	})
}
