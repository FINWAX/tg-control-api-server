package router

import "testing"

// TestSessionTarget locks down which routes a scoped (non-master) token may use.
// This is security-critical: anything returning ok=false is master-only, so a
// regression that widens it would let scoped tokens reach management endpoints.
func TestSessionTarget(t *testing.T) {
	cases := []struct {
		method, path string
		wantID       string
		wantOK       bool
	}{
		// allowed: a session's /call and its status read
		{"GET", "/v1/user/abc", "abc", true},
		{"GET", "/v1/bot/xyz", "xyz", true},
		{"POST", "/v1/user/abc/call", "abc", true},
		{"POST", "/v1/bot/xyz/call", "xyz", true},

		// denied: management and lifecycle
		{"POST", "/v1/user/abc", "", false},
		{"DELETE", "/v1/user/abc", "", false},
		{"PATCH", "/v1/bot/abc", "", false},
		{"POST", "/v1/user/abc/login/code", "", false},
		{"PUT", "/v1/user/abc/updates/webhook", "", false},
		{"GET", "/v1/sessions", "", false},
		{"POST", "/v1/tokens", "", false},
		{"GET", "/healthz", "", false},

		// malformed / boundary
		{"GET", "/v1/user", "", false},
		{"POST", "/v1/bot/abc/call/extra", "", false},
		{"GET", "/v1/group/abc", "", false},
	}
	for _, c := range cases {
		gotID, gotOK := sessionTarget(c.method, c.path)
		if gotID != c.wantID || gotOK != c.wantOK {
			t.Errorf("sessionTarget(%q, %q) = (%q, %v), want (%q, %v)",
				c.method, c.path, gotID, gotOK, c.wantID, c.wantOK)
		}
	}
}
