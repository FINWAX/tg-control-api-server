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
