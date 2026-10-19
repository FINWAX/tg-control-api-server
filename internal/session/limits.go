// The operator's rate limits (package quota has the policy): every call is
// counted against the rules bearing on its session and caller before it goes
// out, all of them or none, and refused once a rule's window is full. The
// counters live in Postgres, so they survive the session moving hosts and
// the gateway reports them without asking a worker.
package session

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/FINWAX/tg-control-api-server/internal/quota"
	"github.com/FINWAX/tg-control-api-server/internal/store"
)

// ruleRefresh is how stale the rules a worker enforces may get: a rule the
// operator adds or changes on the gateway applies within this long.
const ruleRefresh = 2 * time.Second

type ruleCache struct {
	mu    sync.Mutex
	rules []*store.Limit
	at    time.Time
}

type consumerKey struct{}

// WithConsumer tags ctx with the scoped token making the call ("" - master),
// whose own rules then apply.
func WithConsumer(ctx context.Context, tokenID string) context.Context {
	return context.WithValue(ctx, consumerKey{}, tokenID)
}

func consumer(ctx context.Context) string {
	tok, _ := ctx.Value(consumerKey{}).(string)
	return tok
}

// limitRules returns the rules, reading them again once they are stale. A
// store failure keeps the last ones: limits must not stop the gateway.
func (m *Manager) limitRules(ctx context.Context) []*store.Limit {
	c := &m.rules
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) < ruleRefresh {
		return c.rules
	}
	all, err := m.st.ListLimits(ctx)
	if err != nil {
		log.Printf("limits: load rules: %v", err)
		return c.rules
	}
	c.rules, c.at = all, time.Now()
	return c.rules
}

// takeQuota counts a call against every rule bearing on the session for the
// caller, or refuses it with the first rule it would overfill. release gives
// the count back, for a call that then never went out. A store failure lets
// the call through.
func (m *Manager) takeQuota(ctx context.Context, ls *liveSession, method string, params json.RawMessage) (release func(), err error) {
	none := func() {}
	rules := m.limitRules(ctx)
	if len(rules) == 0 {
		return none, nil
	}
	now := time.Now()
	var (
		uses []store.CounterUse
		hit  []*store.Limit
	)
	for _, a := range quota.Applicable(rules, ls.id, ls.kind, consumer(ctx)) {
		if a.ReplacedBy != "" {
			continue
		}
		n := quota.Count(a.Limit, method, params)
		if n == 0 {
			continue
		}
		start, _ := quota.Window(a.Limit.Per, now)
		uses = append(uses, store.CounterUse{
			Key: store.CounterKey{LimitID: a.Limit.ID, Bucket: ls.id, WindowStart: start},
			N:   n, Max: a.Limit.Max,
		})
		hit = append(hit, a.Limit)
	}
	if len(uses) == 0 {
		return none, nil
	}
	i, err := m.st.TakeCounters(ctx, uses)
	if err != nil {
		log.Printf("limits: count %s on %s: %v", method, ls.id, err)
		return none, nil
	}
	if i >= 0 {
		l := hit[i]
		_, end := quota.Window(l.Per, now)
		log.Printf("call %s: refused %s, %s limit %s full until %s", ls.id, method, l.Level, l.ID, end.UTC().Format(time.RFC3339))
		return none, &RefusedError{Reason: "limit", Until: end, Limit: l}
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.st.GiveBackCounters(ctx, uses); err != nil {
			log.Printf("limits: give back %s on %s: %v", method, ls.id, err)
		}
	}, nil
}
