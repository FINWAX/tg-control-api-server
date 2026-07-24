package session

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/FINWAX/tg-control-api-server/internal/tdjson"
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

func TestFloodWaitSeconds(t *testing.T) {
	if n, ok := floodWaitSeconds(&tdjson.Error{Code: 420, Message: "Too Many Requests: retry after 5"}); !ok || n != 5 {
		t.Errorf("flood 5 = (%d, %v)", n, ok)
	}
	if n, ok := floodWaitSeconds(&tdjson.Error{Code: 420, Message: "Too Many Requests: retry after 300"}); !ok || n != 300 {
		t.Errorf("flood 300 = (%d, %v)", n, ok)
	}
	if _, ok := floodWaitSeconds(&tdjson.Error{Code: 420, Message: "no number here"}); ok {
		t.Error("unparseable 420 should be ok=false")
	}
	if _, ok := floodWaitSeconds(&tdjson.Error{Code: 400, Message: "retry after 5"}); ok {
		t.Error("non-420 code should be ok=false")
	}
	if _, ok := floodWaitSeconds(errors.New("plain error")); ok {
		t.Error("non-tdlib error should be ok=false")
	}
}

func TestCollectLocalFilePaths(t *testing.T) {
	// nested inputFileLocal in a sendMessage photo + a thumbnail
	params := json.RawMessage(`{
		"chat_id": 1,
		"input_message_content": {
			"@type": "inputMessagePhoto",
			"photo": {"@type": "inputFileLocal", "path": "/uploads/a/pic.jpg"},
			"thumbnail": {"thumbnail": {"@type": "inputFileLocal", "path": "/uploads/b/thumb.jpg"}}
		}
	}`)
	got := collectLocalFilePaths(params)
	if len(got) != 2 {
		t.Fatalf("collected %v, want 2 paths", got)
	}
	// a remote file / no local path -> nothing
	if p := collectLocalFilePaths(json.RawMessage(`{"photo":{"@type":"inputFileRemote","id":"http://x"}}`)); len(p) != 0 {
		t.Fatalf("remote file should yield no local paths, got %v", p)
	}
}

func TestGuardLocalPaths(t *testing.T) {
	m := &Manager{uploadsDir: "/uploads"}

	// inside the volume: allowed, dedup to one dir
	dirs, err := m.guardLocalPaths(json.RawMessage(
		`{"a":{"@type":"inputFileLocal","path":"/uploads/x/one.jpg"},` +
			`"b":{"@type":"inputFileLocal","path":"/uploads/x/two.jpg"}}`))
	if err != nil {
		t.Fatalf("guard inside volume: %v", err)
	}
	if len(dirs) != 1 || dirs[0] != "/uploads/x" {
		t.Fatalf("dirs = %v, want [/uploads/x]", dirs)
	}

	// escaping the volume: denied
	for _, p := range []string{"/etc/passwd", "/uploads/../etc/passwd", "/data/secret"} {
		if _, err := m.guardLocalPaths(json.RawMessage(
			`{"f":{"@type":"inputFileLocal","path":"` + p + `"}}`)); !errors.Is(err, ErrLocalPathDenied) {
			t.Errorf("guard(%q) err = %v, want ErrLocalPathDenied", p, err)
		}
	}

	// no local file: no dirs, no error
	if dirs, err := m.guardLocalPaths(json.RawMessage(`{"chat_id":1}`)); err != nil || dirs != nil {
		t.Fatalf("guard no-file = (%v, %v)", dirs, err)
	}
}

func TestExtractMessageIDs(t *testing.T) {
	if ids := extractMessageIDs(json.RawMessage(`{"@type":"message","id":42}`)); len(ids) != 1 || ids[0] != 42 {
		t.Fatalf("single message ids = %v", ids)
	}
	if ids := extractMessageIDs(json.RawMessage(`{"@type":"messages","messages":[{"id":1},{"id":2}]}`)); len(ids) != 2 {
		t.Fatalf("album ids = %v, want 2", ids)
	}
	if ids := extractMessageIDs(json.RawMessage(`{"@type":"ok"}`)); len(ids) != 0 {
		t.Fatalf("non-message ids = %v, want none", ids)
	}
}

func TestStringChatID(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{`{"chat_id":"@durov"}`, "@durov", true},
		{`{"chat_id":"durov"}`, "durov", true},
		{`{"chat_id":123}`, "", false}, // numeric passes through
		{`{"chat_id":-100}`, "", false},
		{`{}`, "", false},
		{``, "", false},
		{`not json`, "", false},
	}
	for _, c := range cases {
		got, ok := stringChatID(json.RawMessage(c.in))
		if got != c.want || ok != c.ok {
			t.Errorf("stringChatID(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
