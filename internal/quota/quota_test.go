package quota

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/FINWAX/tg-control-api-server/internal/store"
)

func TestScope(t *testing.T) {
	cases := map[string]string{
		"sendMessage":         "@send",
		"forwardMessages":     "@send",
		"searchPublicChat":    "@resolve",
		"checkChatInviteLink": "@resolve",
		"getChatHistory":      "@history",
		"getMe":               "getMe", // outside every group: its own scope
		"sendChatAction":      "sendChatAction",
	}
	for method, want := range cases {
		if got := Scope(method); got != want {
			t.Errorf("Scope(%q) = %q, want %q", method, got, want)
		}
	}
}

func TestPatterns(t *testing.T) {
	for _, p := range []string{"*", "@send", "@members", "sendMessage", "getChatHistory"} {
		if !ValidPattern(p) {
			t.Errorf("%q should be valid", p)
		}
	}
	for _, p := range []string{"", "@nope", "send*", "wall.*", "Send", "send message"} {
		if ValidPattern(p) {
			t.Errorf("%q should be invalid", p)
		}
	}
	if !Matches("@send", "forwardMessages") || Matches("@send", "sendChatAction") ||
		!Matches("*", "getMe") || !Matches("getMe", "getMe") || Matches("getMe", "getChat") {
		t.Error("Matches disagrees with the groups")
	}
}

func TestCount(t *testing.T) {
	send := &store.Limit{Methods: []string{"@send"}}
	all := &store.Limit{Methods: []string{"*"}}
	hist := &store.Limit{Methods: []string{"getChatHistory"}}
	cases := []struct {
		l      *store.Limit
		method string
		params string
		want   int
	}{
		{send, "sendMessage", `{"chat_id":1}`, 1},
		{send, "forwardMessages", `{"message_ids":[1,2,3]}`, 3},
		{send, "sendMessageAlbum", `{"input_message_contents":[{},{}]}`, 2},
		{send, "forwardMessages", `{"message_ids":[]}`, 1}, // still a call
		{send, "getChat", `{}`, 0},                         // not covered
		{all, "forwardMessages", `{"message_ids":[1,2,3]}`, 1},
		{hist, "getChatHistory", `{"only_local":true}`, 0}, // never reaches Telegram
		{hist, "getChatHistory", `{"only_local":false}`, 1},
		{&store.Limit{Methods: []string{"@invite"}}, "addChatMembers", `{"user_ids":[1,2]}`, 2},
		{&store.Limit{Methods: []string{"@contacts"}}, "importContacts", `{"contacts":[{},{},{}]}`, 3},
	}
	for _, c := range cases {
		if got := Count(c.l, c.method, json.RawMessage(c.params)); got != c.want {
			t.Errorf("Count(%v, %s, %s) = %d, want %d", c.l.Methods, c.method, c.params, got, c.want)
		}
	}
}

func TestWindow(t *testing.T) {
	// Tuesday 2026-10-20 01:30 in Moscow = Monday 22:30 UTC.
	now := time.Date(2026, 10, 19, 22, 30, 15, 0, time.UTC)
	check := func(per string, start, end time.Time) {
		t.Helper()
		s, e := Window(per, now)
		if !s.Equal(start) || !e.Equal(end) {
			t.Errorf("%s: [%v, %v), want [%v, %v)", per, s, e, start, end)
		}
	}
	check("minute", time.Date(2026, 10, 19, 22, 30, 0, 0, time.UTC), time.Date(2026, 10, 19, 22, 31, 0, 0, time.UTC))
	check("hour", time.Date(2026, 10, 19, 22, 0, 0, 0, time.UTC), time.Date(2026, 10, 19, 23, 0, 0, 0, time.UTC))
	// The day began at Moscow midnight, 21:00 UTC.
	check("day", time.Date(2026, 10, 19, 21, 0, 0, 0, time.UTC), time.Date(2026, 10, 20, 21, 0, 0, 0, time.UTC))
	// The week began on Monday 2026-10-19 in Moscow, 2026-10-18 21:00 UTC.
	check("week", time.Date(2026, 10, 18, 21, 0, 0, 0, time.UTC), time.Date(2026, 10, 25, 21, 0, 0, 0, time.UTC))
}

func TestApplicable(t *testing.T) {
	s := func(v string) *string { return &v }
	rule := func(id, level string, kind, sess, tok *string, per string, methods ...string) *store.Limit {
		return &store.Limit{ID: id, Level: level, Kind: kind, SessionID: sess, TokenID: tok,
			Methods: methods, Max: 10, Per: per, Enabled: true}
	}
	rules := []*store.Limit{
		rule("def-join", "session", s("user"), nil, nil, "day", "@join"),
		rule("def-send", "session", s("user"), nil, nil, "hour", "@send"),
		rule("def-bot", "session", s("bot"), nil, nil, "hour", "@send"),
		rule("own-join", "session", nil, s("S"), nil, "day", "@join"),
		rule("other-own", "session", nil, s("X"), nil, "day", "@join"),
		rule("tok-send", "token", nil, nil, s("T"), "hour", "@send"),
		rule("tok-other", "token", nil, nil, s("U"), "hour", "@send"),
		rule("tok-own", "token", nil, nil, s("T"), "day", "@join"),
		{ID: "off", Level: "session", Kind: s("user"), Methods: []string{"*"}, Max: 1, Per: "minute"},
	}
	got := func(tok string) map[string]string {
		out := map[string]string{}
		for _, a := range Applicable(rules, "S", "user", tok) {
			out[a.Limit.ID] = a.ReplacedBy
		}
		return out
	}
	want := map[string]string{"def-join": "own-join", "def-send": "", "own-join": ""}
	if g := got(""); !equal(g, want) {
		t.Errorf("master: %v, want %v", g, want)
	}
	// The token's own rules replace the session's with the same signature,
	// defaults included; another token's do not bear at all.
	want = map[string]string{"def-join": "own-join", "def-send": "tok-send", "own-join": "tok-own",
		"tok-send": "", "tok-own": ""}
	if g := got("T"); !equal(g, want) {
		t.Errorf("token T: %v, want %v", g, want)
	}
}

func equal(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}
