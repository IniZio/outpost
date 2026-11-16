package postgres_test

import (
	"testing"

	"github.com/IniZio/outpost/internal/ports/portstest"
)

func TestPostgresSessionsMeetTheContract(t *testing.T) {
	portstest.SessionsContract(t, newStore(t))
}
