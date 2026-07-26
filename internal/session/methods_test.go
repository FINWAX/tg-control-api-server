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

func TestNumericChatID(t *testing.T) {
	cases := []struct {
		in string
		id int64
		ok bool
	}{
		{`{"chat_id":123}`, 123, true},
		{`{"chat_id":-1001234567890}`, -1001234567890, true},
		{`{"chat_id":-100}`, -100, true},
		{`{"chat_id":"@durov"}`, 0, false}, // username: resolved elsewhere
		{`{"chat_id":0}`, 0, false},
		{`{}`, 0, false},
		{``, 0, false},
		{`not json`, 0, false},
	}
	for _, c := range cases {
		id, ok := numericChatID(json.RawMessage(c.in))
		if id != c.id || ok != c.ok {
			t.Errorf("numericChatID(%q) = (%d, %v), want (%d, %v)", c.in, id, ok, c.id, c.ok)
		}
	}
}

// TestForceLoadChat locks the chat_id -> peer-type decoding. A wrong boundary
// would send a bogus supergroup/basic-group id to TDLib, so each range edge is
// pinned here.
func TestForceLoadChat(t *testing.T) {
	cases := []struct {
		in         string
		wantMethod string
		wantParams string
		wantOK     bool
	}{
		// private chat: chat_id == user id
		{`{"chat_id":123}`, "createPrivateChat", `{"force":true,"user_id":123}`, true},
		// supergroup/channel: chat_id == -1000000000000 - supergroup_id
		{`{"chat_id":-1001234567890}`, "createSupergroupChat", `{"supergroup_id":1234567890}`, true},
		{`{"chat_id":-1000000000001}`, "createSupergroupChat", `{"supergroup_id":1}`, true},
		// chanBase itself is still the channel range, not a basic group
		{`{"chat_id":-1000000000000}`, "createSupergroupChat", `{"supergroup_id":0}`, true},
		// basic group: chat_id == -basic_group_id
		{`{"chat_id":-100}`, "createBasicGroupChat", `{"basic_group_id":100}`, true},
		{`{"chat_id":-999999999999}`, "createBasicGroupChat", `{"basic_group_id":999999999999}`, true},
		// secret chats are out of scope, not misread as channels
		{`{"chat_id":-2000000000001}`, "", "", false},
		// nothing to load
		{`{"chat_id":"@durov"}`, "", "", false},
		{`{}`, "", "", false},
	}
	for _, c := range cases {
		m, q, ok := forceLoadChat(json.RawMessage(c.in))
		if m != c.wantMethod || ok != c.wantOK || (ok && string(q) != c.wantParams) {
			t.Errorf("forceLoadChat(%q) = (%q, %s, %v), want (%q, %s, %v)",
				c.in, m, q, ok, c.wantMethod, c.wantParams, c.wantOK)
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

func TestFileObjectParsing(t *testing.T) {
	// getRemoteFile / downloadFile return a td_api "file" object.
	file := json.RawMessage(`{"@type":"file","id":42,"size":1024,
		"local":{"path":"/data/s/files/photo.jpg","is_downloading_completed":true}}`)
	if id, err := fileIDFromObj(file); err != nil || id != 42 {
		t.Fatalf("fileIDFromObj = (%d, %v)", id, err)
	}
	if p, err := localPathFromFile(file); err != nil || p != "/data/s/files/photo.jpg" {
		t.Fatalf("localPathFromFile = (%q, %v)", p, err)
	}
	// an incomplete download (no local path) is an error
	if _, err := localPathFromFile(json.RawMessage(`{"local":{"path":"","is_downloading_completed":false}}`)); err == nil {
		t.Fatal("empty local path should be an error")
	}
	if _, err := fileIDFromObj(json.RawMessage(`{"id":0}`)); err == nil {
		t.Fatal("zero id should be an error")
	}
}

func TestFileTypeFromError(t *testing.T) {
	if ft := fileTypeFromError(&tdjson.Error{Code: 400, Message: "Can't use file of type Photo as <invalid>"}); ft != "fileTypePhoto" {
		t.Fatalf("photo -> %q, want fileTypePhoto", ft)
	}
	if ft := fileTypeFromError(&tdjson.Error{Code: 400, Message: "Can't use file of type Video as <invalid>"}); ft != "fileTypeVideo" {
		t.Fatalf("video -> %q, want fileTypeVideo", ft)
	}
	if ft := fileTypeFromError(&tdjson.Error{Code: 400, Message: "some unrelated error"}); ft != "" {
		t.Fatalf("unrelated -> %q, want empty", ft)
	}
	if ft := fileTypeFromError(errors.New("plain error")); ft != "" {
		t.Fatalf("non-tdlib -> %q, want empty", ft)
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
