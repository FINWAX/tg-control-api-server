package session

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/FINWAX/tg-control-api-server/internal/tdjson"
)

// newTestManager builds a manager with only the live map wired up. The store is
// nil, so a session under test must not persist — that is what persist=nil is
// for; the persistence itself is covered by TestSetStatusPersists.
func newTestManager(ls *liveSession) *Manager {
	return &Manager{live: map[string]*liveSession{ls.id: ls}, retry: map[string]time.Time{}}
}

// waitingSession is a session parked in one of the login states, with the
// authorizer's side of the channel driven by fake, so a submit can be tested
// without TDLib.
func waitingSession(status string) *liveSession {
	return &liveSession{
		id: "s1", kind: "user", status: status, loggingIn: true,
		cancel:     make(chan struct{}),
		loginDone:  make(chan struct{}),
		codeCh:     make(chan string, 1),
		passwordCh: make(chan string, 1),
	}
}

// fakeAuthorizer stands in for authHandler.await: it takes the submitted value,
// answers with verdict, and — this is the contract under test — leaves the
// session in the same waiting state when the verdict is a refusal.
func fakeAuthorizer(ls *liveSession, in <-chan string, verdict error, next string) {
	go func() {
		<-in
		ls.reportAttempt(verdict)
		if verdict == nil {
			ls.setStatus(next)
		}
	}()
}

func TestSubmitCodeRejectedKeepsLoginAlive(t *testing.T) {
	ls := waitingSession("awaiting_code")
	m := newTestManager(ls)
	refusal := &tdjson.Error{Code: 400, Message: "PHONE_CODE_INVALID"}
	fakeAuthorizer(ls, ls.codeCh, refusal, "")

	st, err := m.SubmitCode("s1", "11111")
	if err == nil {
		t.Fatal("a refused code must surface as an error")
	}
	var te *tdjson.Error
	if !errors.As(err, &te) || te.Message != "PHONE_CODE_INVALID" {
		t.Fatalf("want the TDLib refusal verbatim, got %v", err)
	}
	// The whole point: the attempt is still open, so the operator retypes the
	// code instead of paying Telegram for a new one.
	if st.Status != "awaiting_code" {
		t.Errorf("status = %q, want awaiting_code", st.Status)
	}
	if st.LastError == "" {
		t.Error("last_error should explain the refusal")
	}
}

func TestSubmitCodeAcceptedAdvances(t *testing.T) {
	ls := waitingSession("awaiting_code")
	m := newTestManager(ls)
	fakeAuthorizer(ls, ls.codeCh, nil, "awaiting_password")

	st, err := m.SubmitCode("s1", "12345")
	if err != nil {
		t.Fatalf("SubmitCode: %v", err)
	}
	if st.Status != "awaiting_password" {
		t.Errorf("status = %q, want awaiting_password", st.Status)
	}
	if st.LastError != "" {
		t.Errorf("last_error = %q, want empty after an accepted code", st.LastError)
	}
}

func TestSubmitPasswordRejectedKeepsLoginAlive(t *testing.T) {
	ls := waitingSession("awaiting_password")
	m := newTestManager(ls)
	fakeAuthorizer(ls, ls.passwordCh, &tdjson.Error{Code: 400, Message: "PASSWORD_HASH_INVALID"}, "")

	st, err := m.SubmitPassword("s1", "wrong")
	if err == nil {
		t.Fatal("a refused password must surface as an error")
	}
	// A wrong password used to burn the code that had already been accepted.
	if st.Status != "awaiting_password" {
		t.Errorf("status = %q, want awaiting_password", st.Status)
	}
}

func TestSubmitWrongStateIsRefused(t *testing.T) {
	ls := waitingSession("awaiting_password")
	m := newTestManager(ls)

	if _, err := m.SubmitCode("s1", "12345"); err == nil {
		t.Error("submitting a code while waiting for a password must fail")
	}
	if _, err := m.SubmitCode("nope", "12345"); err == nil {
		t.Error("submitting to an unknown session must fail")
	}
}

func TestSetStatusPersists(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	ls := waitingSession("connecting")
	ls.persist = func(st string) {
		mu.Lock()
		seen = append(seen, st)
		mu.Unlock()
	}

	ls.setStatus("awaiting_code")
	ls.setStatus("awaiting_code") // unchanged: no second write
	ls.setStatus("authorized")

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "awaiting_code" || seen[1] != "authorized" {
		t.Errorf("persisted %v, want [awaiting_code authorized]", seen)
	}
}

func TestStateExposesCodeInfoOnlyWhileWaiting(t *testing.T) {
	ls := waitingSession("awaiting_code")
	ls.codeInfo = []byte(`{"@type":"authenticationCodeInfo","timeout":60}`)

	if got := ls.state().CodeInfo; string(got) == "" {
		t.Error("code_info should be reported while a code is awaited")
	}
	ls.setStatus("authorized")
	if got := ls.state().CodeInfo; got != nil {
		t.Errorf("code_info leaked into an authorized session: %s", got)
	}
}

func TestAbortLoginSignalsOnce(t *testing.T) {
	ls := waitingSession("awaiting_code")
	if !ls.abortLogin() {
		t.Fatal("first abort should report a login in flight")
	}
	select {
	case <-ls.cancel:
	default:
		t.Error("cancel channel should be closed")
	}
	if ls.abortLogin() {
		t.Error("second abort should be a no-op")
	}

	// A session that never ran an interactive login has nothing to abort.
	done := waitingSession("authorized")
	done.loginDone = nil
	if done.abortLogin() {
		t.Error("a non-interactive session has no login to abort")
	}
}

func TestAbortLoginAfterCompletionIsNoOp(t *testing.T) {
	ls := waitingSession("authorized")
	close(ls.loginDone)
	if ls.abortLogin() {
		t.Error("a finished login must not be signalled")
	}
	ls.awaitLogin(time.Second) // must return immediately
}
