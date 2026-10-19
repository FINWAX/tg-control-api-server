package router

import (
	"testing"

	"github.com/FINWAX/tg-control-api-server/internal/store"
)

func TestValidLimit(t *testing.T) {
	s := func(v string) *string { return &v }
	id := "0b8c4b5e-1d2a-4c3b-9e8f-0123456789ab"
	targets := []struct {
		l  store.Limit
		ok bool
	}{
		{store.Limit{Level: "session", Kind: s("user")}, true},
		{store.Limit{Level: "session", Kind: s("bot")}, true},
		{store.Limit{Level: "session", SessionID: s(id)}, true},
		{store.Limit{Level: "token", TokenID: s(id)}, true},
		{store.Limit{Level: "session"}, false},                                    // neither
		{store.Limit{Level: "session", Kind: s("group")}, false},                  // VK's kind
		{store.Limit{Level: "session", SessionID: s(id), Kind: s("user")}, false}, // both
		{store.Limit{Level: "session", SessionID: s("abc")}, false},               // not a uuid
		{store.Limit{Level: "session", Kind: s("user"), TokenID: s(id)}, false},
		{store.Limit{Level: "token", TokenID: s(id), Kind: s("user")}, false},
		{store.Limit{Level: "app", Kind: s("user")}, false}, // no app level here
		{store.Limit{Level: "proxy"}, false},
	}
	for _, c := range targets {
		if msg := validTarget(&c.l); (msg == "") != c.ok {
			t.Errorf("validTarget(%+v) = %q, want ok=%v", c.l, msg, c.ok)
		}
	}
	rules := []struct {
		l  store.Limit
		ok bool
	}{
		{store.Limit{Methods: []string{"@join"}, Max: 3, Per: "hour"}, true},
		{store.Limit{Methods: []string{"*"}, Max: 1, Per: "week"}, true},
		{store.Limit{Methods: []string{"sendMessage", "@send"}, Max: 1, Per: "day"}, true},
		{store.Limit{Methods: nil, Max: 1, Per: "day"}, false},
		{store.Limit{Methods: []string{"@nope"}, Max: 1, Per: "day"}, false},
		{store.Limit{Methods: []string{"@join"}, Max: 0, Per: "day"}, false},
		{store.Limit{Methods: []string{"@join"}, Max: 1, Per: "second"}, false}, // pace does seconds
		{store.Limit{Methods: []string{"@join"}, Max: 1, Per: "month"}, false},
	}
	for _, c := range rules {
		if msg := validLimit(&c.l); (msg == "") != c.ok {
			t.Errorf("validLimit(%+v) = %q, want ok=%v", c.l, msg, c.ok)
		}
	}
}
