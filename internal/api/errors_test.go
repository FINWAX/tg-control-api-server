package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/FINWAX/tg-control-api-server/internal/session"
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

func TestWriteCallErrRefused(t *testing.T) {
	rec := httptest.NewRecorder()
	writeCallErr(rec, &session.RefusedError{Reason: "cooldown", Scope: "@resolve",
		Until: time.Now().Add(30 * time.Second), Cause: "Too Many Requests: retry after 3600"})
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "30" {
		t.Errorf("status %d Retry-After %q, want 429 30", rec.Code, rec.Header().Get("Retry-After"))
	}
	var body struct {
		Error struct {
			Source     string         `json:"source"`
			Code       string         `json:"code"`
			RetryAfter int            `json:"retry_after"`
			Details    map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	e := body.Error
	if e.Source != "gateway" || e.Code != "rate_limited" || e.RetryAfter != 30 {
		t.Errorf("error = %+v", e)
	}
	if e.Details["source"] != "cooldown" || e.Details["scope"] != "@resolve" ||
		e.Details["error"] != "Too Many Requests: retry after 3600" {
		t.Errorf("details = %v", e.Details)
	}
}
