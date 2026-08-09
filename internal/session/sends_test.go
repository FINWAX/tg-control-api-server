package session

import (
	"encoding/json"
	"testing"

	"github.com/zelenin/go-tdlib/client"
)

// The outcome parse is driven off go-tdlib's own marshaling rather than a
// hand-written JSON literal: a renamed td_api field then breaks these tests,
// instead of silently leaving every send stuck at "pending" forever.

func TestNoteSendOutcomeSucceeded(t *testing.T) {
	m := &Manager{sends: make(chan sendOutcome, 1)}
	m.noteSendOutcome("sess", &client.UpdateMessageSendSucceeded{
		Message:      &client.Message{Id: 4096, ChatId: -100500},
		OldMessageId: 42,
	})

	o := <-m.sends
	if o.sessionID != "sess" || o.chatID != -100500 || o.tempID != 42 || o.finalID != 4096 {
		t.Fatalf("outcome = %+v", o)
	}
	if o.tdErr != nil {
		t.Fatalf("succeeded send carries an error: %s", o.tdErr)
	}
}

func TestNoteSendOutcomeFailed(t *testing.T) {
	m := &Manager{sends: make(chan sendOutcome, 1)}
	m.noteSendOutcome("sess", &client.UpdateMessageSendFailed{
		Message:      &client.Message{Id: 43, ChatId: -100500},
		OldMessageId: 42,
		Error:        &client.Error{Code: 400, Message: "CHAT_WRITE_FORBIDDEN"},
	})

	o := <-m.sends
	if o.chatID != -100500 || o.tempID != 42 {
		t.Fatalf("outcome = %+v", o)
	}
	var e struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Source  string `json:"source"`
	}
	if err := json.Unmarshal(o.tdErr, &e); err != nil {
		t.Fatalf("error payload %q: %v", o.tdErr, err)
	}
	if e.Code != 400 || e.Message != "CHAT_WRITE_FORBIDDEN" || e.Source != "tdlib" {
		t.Fatalf("error payload = %+v", e)
	}
}

// A full queue must not block the TDLib receiver goroutine: the outcome is
// dropped and the send stays pending, which is recoverable — a wedged receiver
// is not.
func TestNoteSendOutcomeDoesNotBlockOnFullQueue(t *testing.T) {
	m := &Manager{sends: make(chan sendOutcome, 1)}
	for i := 0; i < 3; i++ {
		m.noteSendOutcome("sess", &client.UpdateMessageSendSucceeded{
			Message:      &client.Message{Id: 4096, ChatId: -1},
			OldMessageId: int64(i + 1),
		})
	}
	if len(m.sends) != 1 {
		t.Fatalf("queued %d outcomes, want the buffer's 1", len(m.sends))
	}
}
