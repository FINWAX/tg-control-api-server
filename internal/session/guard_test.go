package session

import (
	"errors"
	"testing"
	"time"

	"github.com/FINWAX/tg-control-api-server/internal/store"
	"github.com/FINWAX/tg-control-api-server/internal/tdjson"
)

func TestMethodScope(t *testing.T) {
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
		if got := methodScope(method); got != want {
			t.Errorf("methodScope(%q) = %q, want %q", method, got, want)
		}
	}
}

func TestPacedGroupsAreGroups(t *testing.T) {
	groups := map[string]bool{}
	for _, g := range methodGroups {
		groups[g] = true
	}
	for g := range pacedGroups {
		if !groups[g] {
			t.Errorf("paced group %q has no methods", g)
		}
	}
}

func TestGuardReserve(t *testing.T) {
	var g guard
	now := time.Now()
	for i := 0; i <= int(maxPaceWait/paceInterval); i++ {
		wait, ok := g.reserve(now)
		if !ok || wait != time.Duration(i)*paceInterval {
			t.Fatalf("slot %d: wait %v ok %v, want %v true", i, wait, ok, time.Duration(i)*paceInterval)
		}
	}
	if wait, ok := g.reserve(now); ok || wait <= maxPaceWait {
		t.Errorf("past the queue: wait %v ok %v, want refused", wait, ok)
	}
	// A refusal books nothing, and an idle session starts fresh.
	if wait, ok := g.reserve(now.Add(time.Minute)); !ok || wait != 0 {
		t.Errorf("after idle: wait %v ok %v, want 0 true", wait, ok)
	}
}

func TestGuardCooldowns(t *testing.T) {
	var g guard
	now := time.Now()
	if !g.set(store.Cooldown{Scope: "@send", Until: now.Add(time.Hour), Reason: "long"}) {
		t.Fatal("first cooldown not set")
	}
	if g.set(store.Cooldown{Scope: "@send", Until: now.Add(time.Minute), Reason: "short"}) {
		t.Error("a shorter cooldown replaced a longer one")
	}
	if c, ok := g.active("@send", now); !ok || c.Reason != "long" {
		t.Errorf("active = %+v %v, want the long one", c, ok)
	}
	if _, ok := g.active("@send", now.Add(2*time.Hour)); ok {
		t.Error("cooldown still active after it ended")
	}
	if _, ok := g.active("@join", now); ok {
		t.Error("cooldown leaked to another scope")
	}

	g.load(store.Limits{Cooldowns: []store.Cooldown{
		{Scope: store.SpamLimitScope, Until: now.Add(time.Hour), Reason: "PEER_FLOOD"},
	}})
	if _, ok := g.active("@send", now); ok {
		t.Error("load kept a cooldown the registry no longer has")
	}
	if _, _, spam := g.flags(now); !spam {
		t.Error("spam_limit cooldown not reported as spam_limited")
	}
}

func TestCheckCooldown(t *testing.T) {
	ls := &liveSession{id: "s", kind: "user"}
	if err := checkCooldown(ls, "@resolve"); err != nil {
		t.Fatalf("no cooldown: %v", err)
	}
	ls.g.set(store.Cooldown{Scope: "@resolve", Until: time.Now().Add(90 * time.Second),
		Reason: "Too Many Requests: retry after 90"})
	err := checkCooldown(ls, "@resolve")
	re, ok := err.(*RefusedError)
	if !ok {
		t.Fatalf("err = %v, want *RefusedError", err)
	}
	if re.Reason != "cooldown" || re.Scope != "@resolve" || re.RetryAfter() < 89 || re.RetryAfter() > 90 {
		t.Errorf("refusal = %+v retry %d", re, re.RetryAfter())
	}
}

func TestSignals(t *testing.T) {
	now := time.Now()
	flood := &tdjson.Error{Code: 429, Message: "Too Many Requests: retry after 3600"}
	cds, frozen := signals("@resolve", flood, now)
	if len(cds) != 1 || cds[0].Scope != "@resolve" || !cds[0].Until.Equal(now.Add(time.Hour)) || frozen != nil {
		t.Errorf("flood wait: %+v %v", cds, frozen)
	}

	// A send that failed after /call returned arrives as its update's error.
	cds, _ = signals("@send", &tdjson.Error{Code: 400, Message: "PEER_FLOOD"}, now)
	if len(cds) != 1 || cds[0].Scope != store.SpamLimitScope || !cds[0].Until.Equal(now.Add(spamLimitCooldown)) {
		t.Errorf("PEER_FLOOD: %+v", cds)
	}

	cds, frozen = signals("@send", &tdjson.Error{Code: 420, Message: "FROZEN_METHOD_INVALID"}, now)
	if len(cds) != 0 || frozen == nil || !*frozen {
		t.Errorf("frozen: %+v %v", cds, frozen)
	}

	if _, frozen = signals("@send", nil, now); frozen == nil || *frozen {
		t.Errorf("a write through should clear frozen, got %v", frozen)
	}
	if cds, frozen = signals("getMe", nil, now); cds != nil || frozen != nil {
		t.Errorf("a read says nothing: %+v %v", cds, frozen)
	}
	if cds, frozen = signals("@send", &tdjson.Error{Code: 400, Message: "Chat not found"}, now); cds != nil || frozen != nil {
		t.Errorf("an ordinary error says nothing: %+v %v", cds, frozen)
	}
	if cds, frozen = signals("@send", errors.New("session is connecting"), now); cds != nil || frozen != nil {
		t.Errorf("a gateway error says nothing: %+v %v", cds, frozen)
	}
}
