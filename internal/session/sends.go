// The send registry. A td_api send answers immediately with a message whose id
// is temporary and local to this TDLib client; the final server id arrives only
// later, as updateMessageSendSucceeded. /call stays a faithful passthrough and
// keeps returning the temporary id, so both halves of the mapping are written
// to Postgres instead and the caller resolves one into the other afterwards
// (GET /v1/{kind}/{id}/sends). Persisting rather than remembering in memory is
// what makes the mapping survive a worker restart or the session moving hosts,
// and lets the gateway answer without a worker round-trip.
package session

import (
	"context"
	"encoding/json"
	"log"

	"github.com/zelenin/go-tdlib/client"
)

// sendOutcome is a resolved send on its way to the registry.
type sendOutcome struct {
	sessionID string
	chatID    int64
	tempID    int64
	finalID   int64
	tdErr     []byte // rendered td_api error; nil unless the send failed
}

// sendQueue bounds the handoff from the TDLib receiver goroutine to the
// registry writer. Sends are rare next to updates, so this only fills if
// Postgres is wedged.
const sendQueue = 256

// registerSends records every still-sending message in a send result, so its
// temporary id can later be resolved. Best effort: the message is already on
// its way, so a registry failure is logged rather than failing the call.
func (m *Manager) registerSends(ctx context.Context, sessionID string, res json.RawMessage) {
	for _, r := range extractSentRefs(res) {
		if !r.pending {
			continue
		}
		if err := m.st.RegisterSend(ctx, sessionID, r.chatID, r.id); err != nil {
			log.Printf("send registry: register %s/%d: %v", sessionID, r.id, err)
		}
	}
}

// noteSendOutcome queues a completed send for the registry. It runs on the
// TDLib receiver goroutine, so it only parses and hands off; a full queue drops
// the outcome, leaving the caller to see "pending" until retention clears it.
func (m *Manager) noteSendOutcome(sessionID string, update client.Type) {
	b, err := json.Marshal(update)
	if err != nil {
		return
	}
	var u struct {
		OldMessageID int64 `json:"old_message_id"`
		Message      struct {
			ID     int64 `json:"id"`
			ChatID int64 `json:"chat_id"`
		} `json:"message"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &u) != nil || u.OldMessageID == 0 || u.Message.ChatID == 0 {
		return
	}
	o := sendOutcome{
		sessionID: sessionID,
		chatID:    u.Message.ChatID,
		tempID:    u.OldMessageID,
		finalID:   u.Message.ID,
	}
	if u.Error != nil {
		// Store the error in the same envelope /call renders, so a resolved
		// failure reads like any other tdlib rejection.
		o.tdErr, _ = json.Marshal(map[string]any{
			"code": u.Error.Code, "message": u.Error.Message, "source": "tdlib",
		})
	}
	select {
	case m.sends <- o:
	default:
		log.Printf("send registry: queue full, dropping outcome for %s/%d", sessionID, u.OldMessageID)
	}
}

// runSendWriter drains resolved sends into the registry.
func (m *Manager) runSendWriter() {
	ctx := context.Background()
	for o := range m.sends {
		if err := m.st.RecordSendOutcome(ctx, o.sessionID, o.chatID, o.tempID, o.finalID, o.tdErr); err != nil {
			log.Printf("send registry: record %s/%d: %v", o.sessionID, o.tempID, err)
		}
	}
}
