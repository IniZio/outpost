package secrets

import (
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// KeySize is the length of OUTPOST_SECRET_KEY once decoded.
const KeySize = 32

// Key is the server's master key, from OUTPOST_SECRET_KEY. Each use derives its own subkey, so the
// vault's key and the proxy logins' key are never the same bytes.
type Key struct {
	b [KeySize]byte
}

// ErrBadKey is a key that is not KeySize bytes of base64.
var ErrBadKey = errors.New("secrets: OUTPOST_SECRET_KEY must be 32 bytes of base64")

// ParseKey reads a base64 key, standard or URL alphabet, padded or not.
func ParseKey(s string) (Key, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "=")
	raw, err := base64.RawStdEncoding.DecodeString(s)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(s)
	}
	if err != nil || len(raw) != KeySize {
		return Key{}, ErrBadKey
	}
	var k Key
	copy(k.b[:], raw)
	return k, nil
}

// NewKey makes a random key and returns it in the form ParseKey reads, for `outpost serve --init-key`.
func NewKey() string {
	b := make([]byte, KeySize)
	_, _ = rand.Read(b) // crypto/rand.Read never fails; it crashes the program instead
	return base64.StdEncoding.EncodeToString(b)
}

// Derive returns the 32-byte subkey for purpose.
func (k Key) Derive(purpose string) []byte {
	b, err := hkdf.Key(sha256.New, k.b[:], nil, "outpost "+purpose, KeySize)
	if err != nil {
		panic(fmt.Sprintf("secrets: hkdf: %v", err)) // only for a length sha256 cannot give
	}
	return b
}
