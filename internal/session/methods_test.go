package session

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestIsBlockedMethod(t *testing.T) {
	// Gateway-managed methods must stay blocked; notably `close` would crash the
	// worker via a go-tdlib receiver race.
	blocked := []string{
		"close", "destroy", "logOut", "setTdlibParameters",
		"checkAuthenticationCode", "addProxy", "enableProxy",
		"setLogVerbosityLevel",
	}
	for _, m := range blocked {
		if !IsBlockedMethod(m) {
			t.Errorf("%q should be blocked", m)
		}
	}
	allowed := []string{"getMe", "sendMessage", "getProxies", "pingProxy", "getChat"}
	for _, m := range allowed {
		if IsBlockedMethod(m) {
			t.Errorf("%q should be allowed", m)
		}
	}
}

func TestPositiveChatID(t *testing.T) {
	cases := []struct {
		in string
		id int64
		ok bool
	}{
		{`{"chat_id":123}`, 123, true},
		{`{"chat_id":-100}`, 0, false}, // channel/group: not force-resolvable
		{`{"chat_id":0}`, 0, false},
		{`{}`, 0, false},
		{``, 0, false},
		{`not json`, 0, false},
	}
	for _, c := range cases {
		id, ok := positiveChatID(json.RawMessage(c.in))
		if id != c.id || ok != c.ok {
			t.Errorf("positiveChatID(%q) = (%d, %v), want (%d, %v)", c.in, id, ok, c.id, c.ok)
		}
	}
}

func TestIsChatNotFound(t *testing.T) {
	if !isChatNotFound(errors.New("tdlib 400: Chat not found")) {
		t.Error(`should match "Chat not found"`)
	}
	if isChatNotFound(errors.New("tdlib 400: USERNAME_INVALID")) {
		t.Error("should not match an unrelated error")
	}
	if isChatNotFound(nil) {
		t.Error("nil should not match")
	}
}
