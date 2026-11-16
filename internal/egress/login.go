package egress

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/secrets"
)

// Login is the proxy user of one box generation, "t<task>g<gen>" on the wire.
type Login struct {
	Task domain.TaskID
	Gen  domain.BoxGen
}

// ErrBadLogin is a proxy login that is malformed or whose password is not its MAC.
var ErrBadLogin = errors.New("egress: bad proxy login")

// User is the login's proxy user name.
func (l Login) User() string {
	return "t" + strconv.FormatInt(int64(l.Task), 10) + "g" + strconv.FormatUint(uint64(l.Gen), 10)
}

// ParseUser reads "t<task>g<gen>".
func ParseUser(s string) (Login, error) {
	var (
		task int64
		gen  uint32
	)
	if _, err := fmt.Sscanf(s, "t%dg%d", &task, &gen); err != nil || task <= 0 || gen == 0 {
		return Login{}, ErrBadLogin
	}
	l := Login{Task: domain.TaskID(task), Gen: domain.BoxGen(gen)}
	if l.User() != s {
		return Login{}, ErrBadLogin
	}
	return l, nil
}

// signer makes and checks the logins' passwords.
type signer struct {
	key []byte
}

func newSigner(k secrets.Key) signer {
	return signer{key: k.Derive("egress login")}
}

func (s signer) password(l Login) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(l.User()))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// check returns the login when pass is its password.
func (s signer) check(user, pass string) (Login, error) {
	l, err := ParseUser(user)
	if err != nil {
		return Login{}, err
	}
	if !hmac.Equal([]byte(pass), []byte(s.password(l))) {
		return Login{}, ErrBadLogin
	}
	return l, nil
}
