package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FINWAX/tg-control-api-server/internal/tdjson"
)

func TestHTTPStatusForTd(t *testing.T) {
	cases := map[int]int{
		0:   http.StatusBadGateway,      // unparseable / no code
		420: http.StatusTooManyRequests, // raw FLOOD_WAIT
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

func TestWriteCallErrFloodWait(t *testing.T) {
	// The shape TDLib hands back once a flood wait outlasts its own patience.
	rec := httptest.NewRecorder()
	writeCallErr(rec, &tdjson.Error{Code: 429, Message: "Too Many Requests: retry after 61"})
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "61" {
		t.Errorf("Retry-After = %q, want 61", got)
	}
	var body struct {
		Error struct {
			RetryAfter int    `json:"retry_after"`
			Source     string `json:"source"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.RetryAfter != 61 || body.Error.Source != "tdlib" {
		t.Errorf("error = %+v, want retry_after 61 from tdlib", body.Error)
	}
}
