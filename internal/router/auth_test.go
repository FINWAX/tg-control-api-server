package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSessionTarget locks down which routes a scoped (non-master) token may use.
// This is security-critical: anything returning ok=false is master-only, so a
// regression that widens it would let scoped tokens reach management endpoints.
func TestSessionTarget(t *testing.T) {
	cases := []struct {
		method, path string
		wantID       string
		wantOK       bool
	}{
		// allowed: a session's /call, status read, file download, send resolve,
		// update subscriptions
		{"GET", "/v1/user/abc", "abc", true},
		{"GET", "/v1/bot/xyz", "xyz", true},
		{"POST", "/v1/user/abc/call", "abc", true},
		{"POST", "/v1/bot/xyz/call", "xyz", true},
		{"GET", "/v1/user/abc/files/123", "abc", true},
		{"GET", "/v1/bot/xyz/files/123", "xyz", true},
		{"GET", "/v1/user/abc/sends", "abc", true},
		{"GET", "/v1/bot/xyz/sends", "xyz", true},
		{"GET", "/v1/user/abc/updates/subscriptions", "abc", true},
		{"POST", "/v1/bot/xyz/updates/subscriptions", "xyz", true},
		{"GET", "/v1/user/abc/updates/subscriptions/s1", "abc", true},
		{"DELETE", "/v1/bot/xyz/updates/subscriptions/s1", "xyz", true},
		{"GET", "/v1/user/abc/limits", "abc", true},
		{"GET", "/v1/bot/xyz/limits", "xyz", true},

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
		{"POST", "/v1/user/abc/files/123", "", false}, // download is GET-only
		{"GET", "/v1/user/abc/files", "", false},      // needs a file id
		{"POST", "/v1/user/abc/sends", "", false},     // resolve is GET-only
		{"GET", "/v1/user/abc/sends/1", "", false},    // ids go in the query

		{"DELETE", "/v1/user/abc/updates/subscriptions", "", false},   // needs an id
		{"POST", "/v1/user/abc/updates/subscriptions/s1", "", false},  // create has no id
		{"PUT", "/v1/user/abc/updates/subscriptions/s1", "", false},   // no updates in place
		{"GET", "/v1/user/abc/updates/subscriptions/s1/x", "", false}, // nothing below one
		{"GET", "/v1/user/abc/updates", "", false},
		{"POST", "/v1/user/abc/limits", "", false},                        // read-only
		{"DELETE", "/v1/user/abc/limits/cooldowns/spam_limit", "", false}, // master only
	}
	for _, c := range cases {
		gotID, gotOK := sessionTarget(c.method, c.path)
		if gotID != c.wantID || gotOK != c.wantOK {
			t.Errorf("sessionTarget(%q, %q) = (%q, %v), want (%q, %v)",
				c.method, c.path, gotID, gotOK, c.wantID, c.wantOK)
		}
	}
}

// TestPathKind locks the {user|bot} segment extraction that resolveSession
// checks against the stored session kind.
func TestPathKind(t *testing.T) {
	cases := map[string]string{
		"/v1/user/abc":           "user",
		"/v1/bot/abc/call":       "bot",
		"/v1/user/abc/files/123": "user",
		"/v1/sessions":           "sessions",
		"/healthz":               "",
		"/v1":                    "",
		"":                       "",
	}
	for path, want := range cases {
		if got := pathKind(path); got != want {
			t.Errorf("pathKind(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestIsFilesPath locks the file-upload routes an enabled scoped token may use.
func TestIsFilesPath(t *testing.T) {
	yes := []string{"/v1/files", "/v1/files/abc123", "/v1/files/abc123/complete"}
	no := []string{"/v1/file", "/v1/filesx", "/v1/sessions", "/v1/tokens", "/healthz"}
	for _, p := range yes {
		if !isFilesPath(p) {
			t.Errorf("isFilesPath(%q) = false, want true", p)
		}
	}
	for _, p := range no {
		if isFilesPath(p) {
			t.Errorf("isFilesPath(%q) = true, want false", p)
		}
	}
}

// TestForwardTokenHeader: the worker records whoever the gateway says created a
// subscription, so the header must carry only the authenticated token — never
// a value the client sent.
func TestForwardTokenHeader(t *testing.T) {
	var got []string
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = r.Header.Values(tokenIDHeader)
	}))
	defer upstream.Close()
	rt := &Router{transport: http.DefaultTransport}

	send := func(ctxToken string) []string {
		req := httptest.NewRequest(http.MethodPost, "/v1/bot/x/updates/subscriptions", nil)
		req.Header.Set(tokenIDHeader, "forged")
		if ctxToken != "" {
			req = req.WithContext(context.WithValue(req.Context(), tokenIDKey{}, ctxToken))
		}
		got = nil
		rt.forward(httptest.NewRecorder(), req, upstream.URL)
		return got
	}
	if h := send(""); len(h) != 0 {
		t.Errorf("master request forwarded %q, want no token header", h)
	}
	if h := send("tok-1"); len(h) != 1 || h[0] != "tok-1" {
		t.Errorf("scoped request forwarded %q, want [tok-1]", h)
	}
}
