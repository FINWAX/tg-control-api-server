package router

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/FINWAX/tg-control-api-server/internal/quota"
	"github.com/FINWAX/tg-control-api-server/internal/store"
)

// The operator's rate limits, as VK-Control has them (see package quota for
// the policy). Rules are managed here, on the gateway, straight in Postgres;
// workers pick changes up within seconds. Reads show each rule with what it
// has counted in its current window.

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// usage is what a rule has counted in its current window.
type usage struct {
	Used      int       `json:"used"`
	WindowEnd time.Time `json:"window_end"`
}

// ruleView is a rule with its usage; on a session, also the rule that takes
// its place there.
type ruleView struct {
	*store.Limit
	Usage      usage   `json:"usage"`
	ReplacedBy *string `json:"replaced_by,omitempty"`
}

// validLimit checks what may change in a rule: methods, max and per.
func validLimit(l *store.Limit) string {
	if len(l.Methods) == 0 {
		return `methods must list at least one of "*", a method group ("@send") or a method ("sendMessage")`
	}
	for _, m := range l.Methods {
		if !quota.ValidPattern(m) {
			return "bad method " + m + `: use "*", a method group ("@send") or a td_api method name`
		}
	}
	if l.Max <= 0 {
		return "max must be positive"
	}
	if !slices.Contains(quota.Pers, l.Per) {
		return "per must be " + strings.Join(quota.Pers, ", ")
	}
	return ""
}

// validTarget checks that a rule names exactly the target of its level.
func validTarget(l *store.Limit) string {
	isID := func(p *string) bool { return p != nil && uuidPattern.MatchString(*p) }
	switch l.Level {
	case "session":
		const want = "a session limit takes session_id, or kind (user or bot) for the default of every session of the kind"
		if l.TokenID != nil {
			return want
		}
		if l.SessionID == nil {
			if l.Kind == nil || (*l.Kind != "user" && *l.Kind != "bot") {
				return want
			}
		} else if !isID(l.SessionID) || l.Kind != nil {
			return want
		}
	case "token":
		if !isID(l.TokenID) || l.Kind != nil || l.SessionID != nil {
			return "a token limit takes only token_id"
		}
	default:
		return "level must be session or token"
	}
	return ""
}

func (rt *Router) createLimit(w http.ResponseWriter, r *http.Request) {
	in := store.Limit{Enabled: true}
	if !decodeJSON(w, r, &in) {
		return
	}
	if msg := validTarget(&in); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	if msg := validLimit(&in); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	out, err := rt.st.CreateLimit(r.Context(), in)
	if errors.Is(err, store.ErrLimitTarget) {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeOK(w, rt.limitView(r.Context(), out))
}

func (rt *Router) listLimits(w http.ResponseWriter, r *http.Request) {
	all, err := rt.st.ListLimits(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	out := make([]ruleView, len(all))
	for i, l := range all {
		out[i] = rt.limitView(r.Context(), l)
	}
	writeOK(w, out)
}

// limitByPath loads the rule the path names, answering 404 itself.
func (rt *Router) limitByPath(w http.ResponseWriter, r *http.Request) (*store.Limit, bool) {
	id := r.PathValue("id")
	var l *store.Limit
	var err error
	if uuidPattern.MatchString(id) {
		l, err = rt.st.GetLimit(r.Context(), id)
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return nil, false
	}
	if l == nil {
		writeErr(w, http.StatusNotFound, "limit not found")
		return nil, false
	}
	return l, true
}

func (rt *Router) getLimit(w http.ResponseWriter, r *http.Request) {
	if l, ok := rt.limitByPath(w, r); ok {
		writeOK(w, rt.limitView(r.Context(), l))
	}
}

// updateLimit changes methods, max, per, label and enabled; the level and
// target stay.
func (rt *Router) updateLimit(w http.ResponseWriter, r *http.Request) {
	l, ok := rt.limitByPath(w, r)
	if !ok {
		return
	}
	var f map[string]json.RawMessage
	if !decodeJSON(w, r, &f) {
		return
	}
	for _, k := range []string{"id", "level", "kind", "session_id", "token_id"} {
		if _, ok := f[k]; ok {
			writeErr(w, http.StatusBadRequest, k+" cannot change: create another limit")
			return
		}
	}
	for k, dst := range map[string]any{"methods": &l.Methods, "max": &l.Max, "per": &l.Per, "label": &l.Label, "enabled": &l.Enabled} {
		if raw, ok := f[k]; ok {
			if err := json.Unmarshal(raw, dst); err != nil {
				writeErr(w, http.StatusBadRequest, k+": "+err.Error())
				return
			}
		}
	}
	if msg := validLimit(l); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	out, err := rt.st.UpdateLimit(r.Context(), *l)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeOK(w, rt.limitView(r.Context(), out))
}

func (rt *Router) deleteLimit(w http.ResponseWriter, r *http.Request) {
	l, ok := rt.limitByPath(w, r)
	if !ok {
		return
	}
	if _, err := rt.st.DeleteLimit(r.Context(), l.ID); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeOK(w, map[string]any{"status": "deleted"})
}

// limitView is a rule with its usage across sessions: in the current window,
// the most any one session has counted.
func (rt *Router) limitView(ctx context.Context, l *store.Limit) ruleView {
	start, end := quota.Window(l.Per, time.Now())
	v := ruleView{Limit: l, Usage: usage{WindowEnd: end}}
	v.Usage.Used, _ = rt.st.MaxCounter(ctx, l.ID, start)
	return v
}

// sessionRules are the enabled rules bearing on a session for a caller
// (tokenID "" - master), with what each has counted there.
func (rt *Router) sessionRules(ctx context.Context, all []*store.Limit, sessionID, kind, tokenID string) ([]ruleView, error) {
	now := time.Now()
	applied := quota.Applicable(all, sessionID, kind, tokenID)
	keys := make([]store.CounterKey, len(applied))
	for i, a := range applied {
		start, _ := quota.Window(a.Limit.Per, now)
		keys[i] = store.CounterKey{LimitID: a.Limit.ID, Bucket: sessionID, WindowStart: start}
	}
	counts, err := rt.st.Counters(ctx, keys)
	if err != nil {
		return nil, err
	}
	out := make([]ruleView, len(applied))
	for i, a := range applied {
		_, end := quota.Window(a.Limit.Per, now)
		out[i] = ruleView{Limit: a.Limit, Usage: usage{Used: counts[keys[i]], WindowEnd: end}}
		if a.ReplacedBy != "" {
			out[i].ReplacedBy = &a.ReplacedBy
		}
	}
	return out, nil
}

// myLimits is everything that holds a scoped token back, on every session it
// may use: account flags, cooldowns, and the rules with usage.
func (rt *Router) myLimits(w http.ResponseWriter, r *http.Request) {
	tok := tokenOf(r)
	if tok == "" {
		writeErr(w, http.StatusForbidden, "for scoped tokens; the master token reads /v1/{user|bot}/{id}/limits")
		return
	}
	items, err := rt.st.ListSessionsForToken(r.Context(), tok)
	if err == nil {
		var all []*store.Limit
		if all, err = rt.st.ListLimits(r.Context()); err == nil {
			type entry struct {
				store.SessionInfo
				Cooldowns []store.Cooldown `json:"cooldowns"`
				Rules     []ruleView       `json:"rules"`
			}
			out := []entry{}
			for _, it := range items {
				e := entry{SessionInfo: it}
				var l store.Limits
				if l, _, err = rt.st.SessionLimits(r.Context(), it.ID); err != nil {
					break
				}
				e.Cooldowns = l.Cooldowns
				if e.Rules, err = rt.sessionRules(r.Context(), all, it.ID, it.Kind, tok); err != nil {
					break
				}
				out = append(out, e)
			}
			if err == nil {
				writeOK(w, map[string]any{"sessions": out})
				return
			}
		}
	}
	writeErr(w, http.StatusBadGateway, err.Error())
}
