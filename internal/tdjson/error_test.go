package tdjson

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestParseTdError(t *testing.T) {
	err := parseTdError(json.RawMessage(`{"@type":"error","code":400,"message":"BAD"}`))
	var te *Error
	if !errors.As(err, &te) {
		t.Fatalf("expected *Error, got %T", err)
	}
	if te.Code != 400 || te.Message != "BAD" {
		t.Errorf("parsed = %+v, want code=400 message=BAD", te)
	}
	if te.Error() != "tdlib 400: BAD" {
		t.Errorf("Error() = %q, want %q", te.Error(), "tdlib 400: BAD")
	}

	// Unparseable payload -> code 0, raw JSON as message.
	err = parseTdError(json.RawMessage(`not json`))
	if !errors.As(err, &te) {
		t.Fatalf("expected *Error, got %T", err)
	}
	if te.Code != 0 || te.Message != "not json" {
		t.Errorf("malformed parsed = %+v, want code=0 message=%q", te, "not json")
	}
}

func TestErrorRetryAfter(t *testing.T) {
	cases := []struct {
		code int
		msg  string
		want int
		ok   bool
	}{
		{420, "Too Many Requests: retry after 5", 5, true},
		{420, "Too Many Requests: retry after 300", 300, true},
		{420, "no number here", 0, false},
		{400, "retry after 5", 0, false}, // only FLOOD_WAIT carries a wait
		{0, "", 0, false},
	}
	for _, c := range cases {
		e := &Error{Code: c.code, Message: c.msg}
		if got, ok := e.RetryAfter(); got != c.want || ok != c.ok {
			t.Errorf("(%d, %q).RetryAfter() = (%d, %v), want (%d, %v)",
				c.code, c.msg, got, ok, c.want, c.ok)
		}
	}
}
