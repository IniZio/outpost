package api

import (
	"errors"
	"net/http"
)

// StatusOf is the status fail would answer for err.
func StatusOf(err error) int {
	for _, p := range problems {
		if errors.Is(err, p.err) {
			return p.status
		}
	}
	return http.StatusInternalServerError
}
