package api

import (
	"net/http"
	"testing"
)

func TestHTTPStatusForTd(t *testing.T) {
	cases := map[int]int{
		0:   http.StatusBadGateway,      // unparseable / no code
		420: http.StatusTooManyRequests, // FLOOD_WAIT
		400: http.StatusBadRequest,
		401: http.StatusUnauthorized,
		403: http.StatusForbidden,
		404: http.StatusNotFound,
		429: http.StatusTooManyRequests,
		500: http.StatusInternalServerError,
		503: http.StatusServiceUnavailable,
		8:   http.StatusBadRequest, // small positive -> client error
		600: http.StatusBadRequest, // out of HTTP range
	}
	for code, want := range cases {
		if got := httpStatusForTd(code); got != want {
			t.Errorf("httpStatusForTd(%d) = %d, want %d", code, got, want)
		}
	}
}
