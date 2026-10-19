// Package quota is the policy of the operator's rate limits, shared by the
// worker that enforces them and the gateway that reports them: which td_api
// methods a rule covers, how much one call counts, which window it falls in,
// and which rules bear on a session for a given caller. It also names the
// method groups Telegram limits together, which the worker's cooldowns use.
package quota

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/FINWAX/tg-control-api-server/internal/store"
)

// groups gathers the td_api methods Telegram limits together.
var groups = map[string]string{
	"sendMessage":                  "@send",
	"sendMessageAlbum":             "@send",
	"forwardMessages":              "@send",
	"resendMessages":               "@send",
	"sendInlineQueryResultMessage": "@send",
	"joinChat":                     "@join",
	"joinChatByInviteLink":         "@join",
	"addChatMember":                "@invite",
	"addChatMembers":               "@invite",
	"searchPublicChat":             "@resolve",
	"searchPublicChats":            "@resolve",
	"searchUserByPhoneNumber":      "@resolve",
	"checkChatInviteLink":          "@resolve",
	"importContacts":               "@contacts",
	"addContact":                   "@contacts",
	"createNewSupergroupChat":      "@create",
	"createNewBasicGroupChat":      "@create",
	"addMessageReaction":           "@react",
	"setMessageReactions":          "@react",
	"viewMessages":                 "@view",
	"getChatHistory":               "@history",
	"searchChatMessages":           "@history",
	"deleteMessages":               "@delete",
	"deleteChatHistory":            "@delete",
	"getSupergroupMembers":         "@members",
	"searchChatMembers":            "@members",
}

// Scope is a method's group, or the method itself when it is in none.
func Scope(method string) string {
	if g, ok := groups[method]; ok {
		return g
	}
	return method
}

// IsGroup reports whether name is one of the method groups.
func IsGroup(name string) bool {
	for _, g := range groups {
		if g == name {
			return true
		}
	}
	return false
}

var methodName = regexp.MustCompile(`^[a-z][A-Za-z0-9]*$`)

// ValidPattern reports whether p may stand in a rule's methods: "*", a method
// group, or a td_api method name.
func ValidPattern(p string) bool {
	return p == "*" || IsGroup(p) || methodName.MatchString(p)
}

// Matches reports whether pattern ("*", "@send" or "sendMessage") covers method.
func Matches(pattern, method string) bool {
	return pattern == "*" || pattern == method || (strings.HasPrefix(pattern, "@") && groups[method] == pattern)
}

// Count is how many calls one call of method counts against rule l: none when
// l does not cover it or the call stays local (only_local); one for a "*"
// rule, which counts requests; otherwise the messages, people or contacts the
// call carries, since that is what Telegram counts.
func Count(l *store.Limit, method string, params json.RawMessage) int {
	if !slices.ContainsFunc(l.Methods, func(p string) bool { return Matches(p, method) }) {
		return 0
	}
	var p struct {
		OnlyLocal bool              `json:"only_local"`
		Messages  []json.RawMessage `json:"message_ids"`
		Contents  []json.RawMessage `json:"input_message_contents"`
		Users     []json.RawMessage `json:"user_ids"`
		Contacts  []json.RawMessage `json:"contacts"`
	}
	_ = json.Unmarshal(params, &p)
	if p.OnlyLocal {
		return 0
	}
	if slices.Contains(l.Methods, "*") {
		return 1
	}
	n := 0
	switch method {
	case "forwardMessages":
		n = len(p.Messages)
	case "sendMessageAlbum":
		n = len(p.Contents)
	case "addChatMembers":
		n = len(p.Users)
	case "importContacts":
		n = len(p.Contacts)
	}
	return max(n, 1)
}

var moscow = time.FixedZone("MSK", 3*60*60)

// Pers are the windows a rule may count in.
var Pers = []string{"minute", "hour", "day", "week"}

// Window is the fixed window of per that holds now: minutes and hours by the
// UTC clock, days from midnight in Moscow, weeks from Monday, as VK-Control.
func Window(per string, now time.Time) (start, end time.Time) {
	switch per {
	case "minute":
		start = now.Truncate(time.Minute)
		return start, start.Add(time.Minute)
	case "hour":
		start = now.Truncate(time.Hour)
		return start, start.Add(time.Hour)
	}
	m := now.In(moscow)
	start = time.Date(m.Year(), m.Month(), m.Day(), 0, 0, 0, 0, moscow).UTC()
	if per == "week" {
		start = start.AddDate(0, 0, -(int(m.Weekday())+6)%7)
		return start, start.AddDate(0, 0, 7)
	}
	return start, start.AddDate(0, 0, 1)
}

// signature tells rules that cover the same calls in the same window.
func signature(l *store.Limit) string {
	ms := slices.Clone(l.Methods)
	slices.Sort(ms)
	return l.Per + "|" + strings.Join(ms, ",")
}

// Applied is an enabled rule as it bears on one session, and the rule that
// takes its place there, if any.
type Applied struct {
	Limit      *store.Limit
	ReplacedBy string
}

// Applicable lists the enabled rules bearing on a session of kind for a caller
// (tokenID "" - the master token): the defaults of the kind, the session's own
// and the caller's token rules. A session's own rule takes the place of the
// default with the same signature, and the caller's token rule takes the place
// of both. Every rule counts per session.
func Applicable(rules []*store.Limit, sessionID, kind, tokenID string) []Applied {
	own, mine := map[string]string{}, map[string]string{}
	for _, l := range rules {
		switch {
		case !l.Enabled:
		case l.Level == "session" && l.SessionID != nil && *l.SessionID == sessionID:
			own[signature(l)] = l.ID
		case l.Level == "token" && tokenID != "" && l.TokenID != nil && *l.TokenID == tokenID:
			mine[signature(l)] = l.ID
		}
	}
	var out []Applied
	for _, l := range rules {
		if !l.Enabled {
			continue
		}
		sig := signature(l)
		switch {
		case l.Level == "session" && l.SessionID == nil && l.Kind != nil && *l.Kind == kind:
			by := own[sig]
			if by == "" {
				by = mine[sig]
			}
			out = append(out, Applied{l, by})
		case l.Level == "session" && l.SessionID != nil && *l.SessionID == sessionID:
			out = append(out, Applied{l, mine[sig]})
		case l.Level == "token" && tokenID != "" && l.TokenID != nil && *l.TokenID == tokenID:
			out = append(out, Applied{l, ""})
		}
	}
	return out
}
