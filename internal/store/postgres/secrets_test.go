package postgres_test

import (
	"testing"

	"github.com/IniZio/outpost/internal/ports/portstest"
)

func TestPostgresSecretRowsMeetTheContract(t *testing.T) {
	portstest.SecretRowsContract(t, newStore(t))
}
