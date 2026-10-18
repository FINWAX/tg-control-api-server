// Update subscriptions as a worker sees them. The registry is the source of
// truth — subscriptions are created and removed through the gateway — and each
// worker keeps a snapshot per session it holds, reloaded on a short cadence, so
// the TDLib receiver can decide what to queue without a database round-trip.
package session

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/FINWAX/tg-control-api-server/internal/store"
)

// subSyncInterval bounds how long a subscription created or removed elsewhere
// takes to reach a session this worker holds.
const subSyncInterval = time.Second

// FilterSpec is a subscription's stored filter. Both lists are allowlists and
// either may be empty, meaning "no restriction on this axis".
type FilterSpec struct {
	Types []string `json:"types,omitempty"`
	Chats []int64  `json:"chats,omitempty"`
}

// subscription is one subscription's routing rule.
type subscription struct {
	id    string
	types map[string]bool // nil = every update type
	chats map[int64]bool  // nil = every chat, and updates about no chat
}

// subSnapshot is a session's subscriptions as last loaded. It is replaced
// whole and never mutated, so the receiver can hand it to the outbox writer
// without either holding a lock.
type subSnapshot struct {
	subs    []subscription
	anyType bool            // some subscription takes every type
	types   map[string]bool // union of the other subscriptions' types
}

func newSnapshot(fs []store.SubscriptionFilter) *subSnapshot {
	if len(fs) == 0 {
		return nil
	}
	snap := &subSnapshot{types: map[string]bool{}}
	for _, f := range fs {
		var spec FilterSpec
		if len(f.Filters) > 0 {
			if err := json.Unmarshal(f.Filters, &spec); err != nil {
				// A filter this code cannot read must not widen into "everything".
				log.Printf("subscription %s: unreadable filter, skipped: %v", f.ID, err)
				continue
			}
		}
		sub := subscription{id: f.ID}
		if len(spec.Types) > 0 {
			sub.types = make(map[string]bool, len(spec.Types))
			for _, t := range spec.Types {
				sub.types[t] = true
				snap.types[t] = true
			}
		} else {
			snap.anyType = true
		}
		if len(spec.Chats) > 0 {
			sub.chats = make(map[int64]bool, len(spec.Chats))
			for _, c := range spec.Chats {
				sub.chats[c] = true
			}
		}
		snap.subs = append(snap.subs, sub)
	}
	if len(snap.subs) == 0 {
		return nil
	}
	return snap
}

// wants is the receiver's cheap pre-check: could any subscription take an
// update of this type? Only those are serialized and queued.
func (s *subSnapshot) wants(ctor string) bool {
	return s != nil && (s.anyType || s.types[ctor])
}

// route returns the subscriptions an update goes to. The chat is looked up only
// when some subscription filters on it.
func (s *subSnapshot) route(ctor string, payload []byte) []string {
	var (
		ids             []string
		chat            int64
		hasChat, looked bool
	)
	for _, sub := range s.subs {
		if sub.types != nil && !sub.types[ctor] {
			continue
		}
		if sub.chats != nil {
			if !looked {
				chat, hasChat = updateChat(payload)
				looked = true
			}
			// "Only these chats" excludes updates that concern no chat at all.
			if !hasChat || !sub.chats[chat] {
				continue
			}
		}
		ids = append(ids, sub.id)
	}
	return ids
}

// updateChat finds the chat an update concerns: its own chat_id, else the chat
// of the message it carries. ok is false for an update about no chat.
func updateChat(payload []byte) (id int64, ok bool) {
	var u struct {
		ChatID  *int64 `json:"chat_id"`
		Message *struct {
			ChatID int64 `json:"chat_id"`
		} `json:"message"`
	}
	if json.Unmarshal(payload, &u) != nil {
		return 0, false
	}
	switch {
	case u.ChatID != nil:
		return *u.ChatID, true
	case u.Message != nil:
		return u.Message.ChatID, true
	}
	return 0, false
}

// runSubscriptionSync keeps the held sessions' snapshots current and reports
// lost updates to the registry.
func (m *Manager) runSubscriptionSync() {
	ctx := context.Background()
	t := time.NewTicker(subSyncInterval)
	defer t.Stop()
	for range t.C {
		m.syncSubscriptions(ctx, m.liveSessions())
		m.flushDropped(ctx)
	}
}

// RefreshSubscriptions reloads one session's snapshot now, if this worker holds
// it, instead of waiting for the next sync.
func (m *Manager) RefreshSubscriptions(ctx context.Context, id string) {
	if ls := m.get(id); ls != nil {
		m.syncSubscriptions(ctx, []*liveSession{ls})
	}
}

func (m *Manager) syncSubscriptions(ctx context.Context, sessions []*liveSession) {
	if len(sessions) == 0 {
		return
	}
	ids := make([]string, len(sessions))
	for i, ls := range sessions {
		ids[i] = ls.id
	}
	fs, err := m.st.SubscriptionFilters(ctx, ids)
	if err != nil {
		log.Printf("subscriptions: sync: %v", err) // keep the last good snapshots
		return
	}
	bySession := map[string][]store.SubscriptionFilter{}
	for _, f := range fs {
		bySession[f.SessionID] = append(bySession[f.SessionID], f)
	}
	for _, ls := range sessions {
		snap := newSnapshot(bySession[ls.id])
		ls.mu.Lock()
		ls.subs = snap
		ls.mu.Unlock()
	}
}

// flushDropped moves each session's count of lost updates into the registry,
// where the subscription status reads it. A failed write keeps the count for
// the next round.
func (m *Manager) flushDropped(ctx context.Context) {
	for _, ls := range m.liveSessions() {
		n := ls.dropped.Swap(0)
		if n == 0 {
			continue
		}
		if err := m.st.AddDropped(ctx, ls.id, n); err != nil {
			ls.dropped.Add(n)
			log.Printf("subscriptions: record %d dropped for %s: %v", n, ls.id, err)
		}
	}
}

// liveSessions returns the sessions this worker currently holds.
func (m *Manager) liveSessions() []*liveSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*liveSession, 0, len(m.live))
	for _, ls := range m.live {
		out = append(out, ls)
	}
	return out
}
