// The call guard keeps a user account inside what Telegram tolerates, across
// every consumer of the session at once. It does not guess Telegram's limits:
// it acts on what Telegram itself said. A flood wait cools down the method's
// group until the wait is over, PEER_FLOOD restricts the account the way
// Telegram has restricted it, and writes are paced to about one a second. A
// call the guard stops never reaches Telegram. Bots are left alone: their
// limits are per chat and a flood costs them nothing but the wait, which the
// caller already gets as retry_after.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/zelenin/go-tdlib/client"

	"github.com/FINWAX/tg-control-api-server/internal/store"
	"github.com/FINWAX/tg-control-api-server/internal/tdjson"
)

const (
	paceInterval      = time.Second      // one paced write per session this often
	maxPaceWait       = 10 * time.Second // a write that would queue longer is refused
	spamLimitCooldown = 24 * time.Hour   // how long a PEER_FLOOD restricts the account
)

// pacedGroups are the writes Telegram's anti-spam watches; they are paced, and
// one going through shows the account is not frozen.
var pacedGroups = map[string]bool{
	"@send": true, "@join": true, "@invite": true, "@create": true, "@contacts": true,
}

// RefusedError is a call the guard stopped before it reached Telegram.
type RefusedError struct {
	Reason string       // "cooldown", "pace" or "limit"
	Scope  string       // the cooldown scope, or the paced group; "" for limit
	Until  time.Time    // when the call may be tried again
	Cause  string       // Telegram's error that started the cooldown; "" otherwise
	Limit  *store.Limit // the operator's rule that ran out; only for limit
}

func (e *RefusedError) Error() string {
	switch e.Reason {
	case "pace":
		return fmt.Sprintf("too many %s calls queued on this session", e.Scope)
	case "limit":
		l := e.Limit
		name := l.ID
		if l.Label != nil && *l.Label != "" {
			name = *l.Label
		}
		return fmt.Sprintf("%s limit %q: %d per %s (%s)", l.Level, name, l.Max, l.Per, strings.Join(l.Methods, ", "))
	}
	return fmt.Sprintf("%s is cooling down: %s", e.Scope, e.Cause)
}

// RetryAfter is the whole seconds left until the call may be tried again.
func (e *RefusedError) RetryAfter() int {
	return int(math.Ceil(time.Until(e.Until).Seconds()))
}

// guard is one session's cooldowns, pace and account flags.
type guard struct {
	mu      sync.Mutex
	cool    map[string]store.Cooldown
	next    time.Time // earliest start of the next paced write
	premium *bool
	frozen  bool
	myID    int64 // the account's own user id: its Saved Messages chat
}

// load takes over what the registry knows, when the session comes up here.
func (g *guard) load(l store.Limits) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.premium, g.frozen = l.IsPremium, l.Frozen
	g.cool = map[string]store.Cooldown{}
	for _, c := range l.Cooldowns {
		g.cool[c.Scope] = c
	}
}

// active returns the running cooldown on scope, if any.
func (g *guard) active(scope string, now time.Time) (store.Cooldown, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	c, ok := g.cool[scope]
	if !ok || !c.Until.After(now) {
		return store.Cooldown{}, false
	}
	return c, true
}

// set records a cooldown unless a longer one already runs on its scope, and
// reports whether it did.
func (g *guard) set(c store.Cooldown) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cool == nil {
		g.cool = map[string]store.Cooldown{}
	}
	if cur, ok := g.cool[c.Scope]; ok && !cur.Until.Before(c.Until) {
		return false
	}
	g.cool[c.Scope] = c
	return true
}

// clear drops a cooldown.
func (g *guard) clear(scope string) {
	g.mu.Lock()
	delete(g.cool, scope)
	g.mu.Unlock()
}

// reserve books the next paced slot and returns how long to wait for it, or
// ok=false (booking nothing) when that would be longer than maxPaceWait.
func (g *guard) reserve(now time.Time) (wait time.Duration, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.next.Before(now) {
		g.next = now
	}
	wait = g.next.Sub(now)
	if wait > maxPaceWait {
		return wait, false
	}
	g.next = g.next.Add(paceInterval)
	return wait, true
}

// flags returns the account flags for a status read.
func (g *guard) flags(now time.Time) (premium *bool, frozen, spamLimited bool) {
	_, spamLimited = g.active(store.SpamLimitScope, now)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.premium, g.frozen, spamLimited
}

// checkCooldown refuses a call whose scope is cooling down.
func checkCooldown(ls *liveSession, scope string) error {
	c, ok := ls.g.active(scope, time.Now())
	if !ok {
		return nil
	}
	log.Printf("call %s: refused %s, cooling down until %s", ls.id, scope, c.Until.UTC().Format(time.RFC3339))
	return &RefusedError{Reason: "cooldown", Scope: scope, Until: c.Until, Cause: c.Reason}
}

// admit applies the account restriction, the operator's limits and the write
// pace to a call that is about to go out; params must already carry a numeric
// chat_id. A call it refuses spends no quota.
func (m *Manager) admit(ctx context.Context, ls *liveSession, cl *client.Client, method, scope string, params json.RawMessage) error {
	if ls.kind == "user" {
		if c, ok := ls.g.active(store.SpamLimitScope, time.Now()); ok && m.spamBlocked(ctx, ls, cl, scope, params) {
			log.Printf("call %s: refused %s, account spam-limited until %s", ls.id, scope, c.Until.UTC().Format(time.RFC3339))
			return &RefusedError{Reason: "cooldown", Scope: store.SpamLimitScope, Until: c.Until, Cause: c.Reason}
		}
	}
	release, err := m.takeQuota(ctx, ls, method, params)
	if err != nil {
		return err
	}
	if ls.kind != "user" || !pacedGroups[scope] {
		return nil
	}
	now := time.Now()
	wait, ok := ls.g.reserve(now)
	if !ok {
		release()
		log.Printf("call %s: refused %s, pace queue full", ls.id, scope)
		return &RefusedError{Reason: "pace", Scope: scope, Until: now.Add(wait - maxPaceWait)}
	}
	if wait == 0 {
		return nil
	}
	select {
	case <-time.After(wait):
		return nil
	case <-ctx.Done():
		release()
		return ctx.Err()
	}
}

// spamBlocked tells whether a spam-limited account may not make this call:
// Telegram then lets it write only to mutual contacts, and invite or import
// nobody. Telling a mutual contact apart is getUser, which a user account
// answers from its local cache without asking Telegram.
func (m *Manager) spamBlocked(ctx context.Context, ls *liveSession, cl *client.Client, scope string, params json.RawMessage) bool {
	switch scope {
	case "@invite", "@contacts":
		return true
	case "@send":
	default:
		return false
	}
	id, ok := numericChatID(params)
	if !ok || id <= 0 {
		return false // a group or channel, not a private chat
	}
	ls.g.mu.Lock()
	self := id == ls.g.myID
	ls.g.mu.Unlock()
	if self {
		return false // Saved Messages
	}
	q, _ := json.Marshal(map[string]any{"user_id": id})
	res, err := tdjson.Call(ctx, cl, "getUser", q)
	if err != nil {
		return true // nobody the account knows
	}
	var u struct {
		Mutual bool `json:"is_mutual_contact"`
	}
	return json.Unmarshal(res, &u) != nil || !u.Mutual
}

// observe learns from how Telegram answered a call in scope (err == nil: it
// went through). It never blocks: registry writes go out on their own.
func (m *Manager) observe(ls *liveSession, scope string, err error) {
	if ls.kind != "user" {
		return
	}
	cds, frozen := signals(scope, err, time.Now())
	for _, c := range cds {
		m.coolDown(ls, c)
	}
	if frozen != nil {
		m.setFrozen(ls, *frozen)
	}
}

// signals reads what Telegram's answer to a call in scope says about the
// account: the cooldowns it imposes, and whether the account is frozen
// (nil: the answer says nothing either way).
func signals(scope string, err error, now time.Time) (cds []store.Cooldown, frozen *bool) {
	if err == nil {
		if pacedGroups[scope] {
			no := false
			return nil, &no // a write went through: not frozen
		}
		return nil, nil
	}
	var te *tdjson.Error
	if !errors.As(err, &te) {
		return nil, nil
	}
	if secs, ok := te.RetryAfter(); ok {
		cds = append(cds, store.Cooldown{Scope: scope, Until: now.Add(time.Duration(secs) * time.Second), Reason: te.Message})
	}
	if strings.Contains(te.Message, "PEER_FLOOD") {
		cds = append(cds, store.Cooldown{Scope: store.SpamLimitScope, Until: now.Add(spamLimitCooldown), Reason: te.Message})
	}
	if strings.Contains(te.Message, "FROZEN_") {
		yes := true
		frozen = &yes
	}
	return cds, frozen
}

// observeSendFailed learns from a message that failed after /call returned:
// a send's flood wait and PEER_FLOOD arrive only this way.
func (m *Manager) observeSendFailed(ls *liveSession, u *client.UpdateMessageSendFailed) {
	if u.Error == nil {
		return
	}
	m.observe(ls, "@send", &tdjson.Error{Code: int(u.Error.Code), Message: u.Error.Message})
}

func (m *Manager) coolDown(ls *liveSession, c store.Cooldown) {
	if !ls.g.set(c) {
		return
	}
	log.Printf("session %s: %s cooling down until %s (%s)", ls.id, c.Scope, c.Until.UTC().Format(time.RFC3339), c.Reason)
	go func() {
		if err := m.st.SetCooldown(context.Background(), ls.id, c); err != nil {
			log.Printf("session %s: record cooldown %s: %v", ls.id, c.Scope, err)
		}
	}()
}

func (m *Manager) setFrozen(ls *liveSession, frozen bool) {
	ls.g.mu.Lock()
	changed := ls.g.frozen != frozen
	ls.g.frozen = frozen
	ls.g.mu.Unlock()
	if !changed {
		return
	}
	log.Printf("session %s: frozen=%v", ls.id, frozen)
	go func() {
		if err := m.st.SetSessionFrozen(context.Background(), ls.id, frozen); err != nil {
			log.Printf("session %s: record frozen: %v", ls.id, err)
		}
	}()
}

// noteFreeze records the account's freeze as TDLib reports it. TDLib reports
// it when it changes, and on start only if the account is frozen, so the
// FROZEN_* errors and the writes that go through still keep the flag.
func (m *Manager) noteFreeze(ls *liveSession, u *client.UpdateFreezeState) {
	if !u.IsFrozen {
		m.setFrozen(ls, false)
		return
	}
	ls.g.mu.Lock()
	ls.g.frozen = true
	ls.g.mu.Unlock()
	since, deletion := time.Unix(int64(u.FreezingDate), 0), time.Time{}
	if u.DeletionDate > 0 {
		deletion = time.Unix(int64(u.DeletionDate), 0)
	}
	log.Printf("session %s: frozen since %s, deletion %s", ls.id, since.UTC().Format(time.RFC3339), deletion.UTC().Format(time.RFC3339))
	go func() {
		if err := m.st.SetSessionFreeze(context.Background(), ls.id, since, deletion, u.AppealLink); err != nil {
			log.Printf("session %s: record freeze: %v", ls.id, err)
		}
	}()
}

// notePremium records the account's Premium flag as TDLib reports it.
func (m *Manager) notePremium(ls *liveSession, v client.OptionValue) {
	b, _ := v.(*client.OptionValueBoolean) // optionValueEmpty: not Premium
	premium := b != nil && b.Value
	ls.g.mu.Lock()
	changed := ls.g.premium == nil || *ls.g.premium != premium
	ls.g.premium = &premium
	ls.g.mu.Unlock()
	if !changed {
		return
	}
	go func() {
		if err := m.st.SetSessionPremium(context.Background(), ls.id, premium); err != nil {
			log.Printf("session %s: record premium: %v", ls.id, err)
		}
	}()
}

// noteMyID records the account's own user id as TDLib reports it.
func (ls *liveSession) noteMyID(v client.OptionValue) {
	if n, ok := v.(*client.OptionValueInteger); ok {
		ls.g.mu.Lock()
		ls.g.myID = int64(n.Value)
		ls.g.mu.Unlock()
	}
}

// readAccount reads the user account's own id and Premium flag once it is
// authorized. TDLib announces an option by update only when it is set, so a
// non-Premium account would otherwise never report is_premium at all.
func (m *Manager) readAccount(ls *liveSession, cl *client.Client) {
	if ls.kind != "user" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, name := range []string{"my_id", "is_premium"} {
		q, _ := json.Marshal(map[string]string{"name": name})
		res, err := tdjson.Call(ctx, cl, "getOption", q)
		if err != nil {
			log.Printf("session %s: read option %s: %v", ls.id, name, err)
			continue
		}
		v, err := client.UnmarshalOptionValue(res)
		if err != nil {
			continue
		}
		if name == "my_id" {
			ls.noteMyID(v)
		} else {
			m.notePremium(ls, v)
		}
	}
}

// ClearCooldown lifts a cooldown before its end, for an operator who knows
// better than the guard: typically a spam limit Telegram lifted early after an
// appeal. It clears the live session's copy too when this worker holds it.
func (m *Manager) ClearCooldown(ctx context.Context, id, scope string) (bool, error) {
	if ls := m.get(id); ls != nil {
		ls.g.clear(scope)
	}
	found, err := m.st.DeleteCooldown(ctx, id, scope)
	if err == nil && found {
		log.Printf("session %s: %s cooldown lifted by the operator", id, scope)
	}
	return found, err
}

// loadGuard restores a session's cooldowns and flags from the registry.
func (m *Manager) loadGuard(ctx context.Context, ls *liveSession) {
	l, _, err := m.st.SessionLimits(ctx, ls.id)
	if err != nil {
		log.Printf("session %s: load cooldowns: %v", ls.id, err)
		return
	}
	ls.g.load(l)
}
