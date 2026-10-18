package session

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/zelenin/go-tdlib/client"

	"github.com/FINWAX/tg-control-api-server/internal/store"
)

// payload marshals a real go-tdlib update the way updateHandler does, so the
// tests see the JSON shape a subscription is actually matched against.
func payload(t *testing.T, u client.Type) []byte {
	t.Helper()
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestUpdateChat(t *testing.T) {
	cases := []struct {
		name   string
		update client.Type
		want   int64
		ok     bool
	}{
		{"top-level chat", &client.UpdateNewChatJoinRequest{ChatId: -1001}, -1001, true},
		{"message chat", &client.UpdateNewMessage{Message: &client.Message{ChatId: -1002}}, -1002, true},
		{"sent message chat", &client.UpdateMessageSendSucceeded{
			Message: &client.Message{ChatId: 42}, OldMessageId: 1}, 42, true},
		{"failed send chat", &client.UpdateMessageSendFailed{
			Message: &client.Message{ChatId: 43}, OldMessageId: 1}, 43, true},
		{"no chat", &client.UpdateUserStatus{UserId: 7}, 0, false},
	}
	for _, c := range cases {
		got, ok := updateChat(payload(t, c.update))
		if got != c.want || ok != c.ok {
			t.Errorf("%s: updateChat = (%d, %v), want (%d, %v)", c.name, got, ok, c.want, c.ok)
		}
	}
	if _, ok := updateChat([]byte("not json")); ok {
		t.Error("garbage reported a chat")
	}
}

func filters(id, raw string) store.SubscriptionFilter {
	f := store.SubscriptionFilter{ID: id, SessionID: "s"}
	if raw != "" {
		f.Filters = []byte(raw)
	}
	return f
}

// TestRoute is the fan-out contract: each subscription gets what its own
// filter admits, and a chat filter excludes updates about no chat.
func TestRoute(t *testing.T) {
	snap := newSnapshot([]store.SubscriptionFilter{
		filters("all", ""),
		filters("msgs", `{"types":["updateNewMessage"]}`),
		filters("chat", `{"chats":[-1001]}`),
		filters("both", `{"types":["updateNewMessage"],"chats":[-1002]}`),
	})
	cases := []struct {
		name   string
		update client.Type
		want   []string
	}{
		{"message in -1001", &client.UpdateNewMessage{Message: &client.Message{ChatId: -1001}},
			[]string{"all", "msgs", "chat"}},
		{"message in -1002", &client.UpdateNewMessage{Message: &client.Message{ChatId: -1002}},
			[]string{"all", "msgs", "both"}},
		{"join request in -1001", &client.UpdateNewChatJoinRequest{ChatId: -1001},
			[]string{"all", "chat"}},
		{"chatless update", &client.UpdateUserStatus{UserId: 7},
			[]string{"all"}},
	}
	for _, c := range cases {
		b := payload(t, c.update)
		if got := snap.route(c.update.GetConstructor(), b); !slices.Equal(got, c.want) {
			t.Errorf("%s: routed to %v, want %v", c.name, got, c.want)
		}
	}
}

// TestWants checks the receiver's pre-check: only types some subscription
// could take are serialized, and an unfiltered subscription takes them all.
func TestWants(t *testing.T) {
	var none *subSnapshot
	if none.wants("updateNewMessage") {
		t.Error("a session without subscriptions wants updates")
	}
	typed := newSnapshot([]store.SubscriptionFilter{
		filters("a", `{"types":["updateNewMessage"]}`),
		filters("b", `{"types":["updateNewChatJoinRequest"],"chats":[1]}`),
	})
	for ctor, want := range map[string]bool{
		"updateNewMessage": true, "updateNewChatJoinRequest": true, "updateUserStatus": false,
	} {
		if got := typed.wants(ctor); got != want {
			t.Errorf("wants(%s) = %v, want %v", ctor, got, want)
		}
	}
	// A chat-only filter restricts chats, not types.
	if !newSnapshot([]store.SubscriptionFilter{filters("c", `{"chats":[1]}`)}).wants("updateUserStatus") {
		t.Error("a chat-only subscription refused a type")
	}
}

// TestSnapshotSkipsUnreadableFilter: a filter that cannot be parsed must not
// silently become "deliver everything".
func TestSnapshotSkipsUnreadableFilter(t *testing.T) {
	if snap := newSnapshot([]store.SubscriptionFilter{filters("bad", `{"types":"oops"}`)}); snap != nil {
		t.Fatalf("snapshot = %+v, want nil", snap)
	}
	if newSnapshot(nil) != nil {
		t.Fatal("no subscriptions should give a nil snapshot")
	}
}

func TestGroupBySubscription(t *testing.T) {
	batch := []store.Delivery{
		{ID: "1", SubscriptionID: "b"}, {ID: "2", SubscriptionID: "a"},
		{ID: "3", SubscriptionID: "b"}, {ID: "4", SubscriptionID: "a"},
	}
	groups, order := groupBySubscription(batch)
	if !slices.Equal(order, []string{"b", "a"}) {
		t.Fatalf("order = %v, want [b a] (by first row)", order)
	}
	ids := func(ds []store.Delivery) (out []string) {
		for _, d := range ds {
			out = append(out, d.ID)
		}
		return out
	}
	if !slices.Equal(ids(groups["b"]), []string{"1", "3"}) || !slices.Equal(ids(groups["a"]), []string{"2", "4"}) {
		t.Fatalf("groups = %v, want claim order kept within each", groups)
	}
}
