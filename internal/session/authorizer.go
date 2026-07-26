package session

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/zelenin/go-tdlib/client"

	"github.com/FINWAX/tg-control-api-server/internal/tdjson"
)

// errRehydrateExpired is returned when a session being restored from its binlog
// is no longer authorized (TDLib asks for phone/code/password). Such a session
// needs a fresh login via the API.
var errRehydrateExpired = errors.New("session not authorized in binlog (needs re-login)")

// errLoginCanceled aborts a login that is waiting for input, so the client is
// closed and its directory can be removed. Raised by DeleteSession.
var errLoginCanceled = errors.New("login canceled")

// errLoginRestart reports that TDLib dropped the login back to the phone step
// after the code had already been requested — the attempt is spent (typically
// an expired code) and only a new login can continue.
var errLoginRestart = errors.New("login attempt expired; start a new login")

// authHandler implements client.AuthorizationStateHandler for both bots and
// users. It applies TDLib parameters and the proxy up front, then either
// checks the bot token or drives the interactive user flow, taking the code
// and password from the session's channels (fed by HTTP endpoints).
//
// A rejected code or password is deliberately NOT returned to go-tdlib: its
// Authorize loop closes the client on any handler error, which would destroy
// the session over a typo and force a fresh code request. Instead the rejection
// is reported to whoever submitted it and the handler returns cleanly, so the
// loop re-enters the same waiting state and the attempt survives.
type authHandler struct {
	params    *client.SetTdlibParametersRequest
	proxy     *client.AddProxyRequest // nil = direct
	botToken  string                  // "" = user login
	phone     string
	phoneSent bool // the code was already requested once for this login
	rehydrate bool // true = restore from binlog only; any login prompt means expired
	ls        *liveSession
}

func (h *authHandler) Handle(cl *client.Client, state client.AuthorizationState) error {
	ctx := context.Background()

	switch state.AuthorizationStateConstructor() {
	case client.ConstructorAuthorizationStateWaitTdlibParameters:
		if _, err := cl.SetTdlibParameters(ctx, h.params); err != nil {
			return h.fatal(err)
		}
		if h.proxy != nil {
			if _, err := cl.AddProxy(ctx, h.proxy); err != nil {
				return h.fatal(err)
			}
		}
		return nil

	case client.ConstructorAuthorizationStateWaitPhoneNumber:
		if h.rehydrate {
			return errRehydrateExpired
		}
		if h.botToken != "" {
			_, err := cl.CheckAuthenticationBotToken(ctx, &client.CheckAuthenticationBotTokenRequest{
				Token: h.botToken,
			})
			return h.fatal(err)
		}
		// Landing here a second time means TDLib abandoned the code it had
		// already sent. Silently asking for another one would spend a Telegram
		// send the operator never requested, so the login fails instead and a
		// new one has to be started deliberately.
		if h.phoneSent {
			return h.fatal(errLoginRestart)
		}
		h.phoneSent = true
		h.ls.beginLogin(cl)
		_, err := cl.SetAuthenticationPhoneNumber(ctx, &client.SetAuthenticationPhoneNumberRequest{
			PhoneNumber: h.phone,
			Settings:    &client.PhoneNumberAuthenticationSettings{},
		})
		return h.fatal(err)

	case client.ConstructorAuthorizationStateWaitCode:
		if h.rehydrate {
			return errRehydrateExpired
		}
		if s, ok := state.(*client.AuthorizationStateWaitCode); ok {
			h.ls.setCodeInfo(s.CodeInfo)
		}
		return h.await(cl, "awaiting_code", h.ls.codeCh, "checkAuthenticationCode", "code")

	case client.ConstructorAuthorizationStateWaitPassword:
		if h.rehydrate {
			return errRehydrateExpired
		}
		return h.await(cl, "awaiting_password", h.ls.passwordCh, "checkAuthenticationPassword", "password")

	case client.ConstructorAuthorizationStateReady:
		// The "authorized" transition belongs to setClient, which runs once
		// go-tdlib hands the finished client back. Declaring it here would open
		// a window where the session claims to be usable while the client is
		// not yet stored — and rebalancing could close it mid-handover.
		return nil

	case client.ConstructorAuthorizationStateClosing,
		client.ConstructorAuthorizationStateClosed:
		return nil

	default:
		// Email verification, registration, QR, etc. are out of scope for v0.
		return h.fatal(client.NotSupportedAuthorizationState(state))
	}
}

// await publishes the waiting state, blocks for the operator's value, and hands
// it to TDLib. Whatever TDLib answers, the handler returns nil so go-tdlib keeps
// the client alive and re-enters this state: a rejected value can then be
// retried on the same login, which is what makes wrong input cheap. Only a
// cancel breaks out, and it does so through an error precisely because that is
// what closes the client.
//
// The check goes through the raw dispatcher rather than a typed wrapper so a
// rejection arrives as a *tdjson.Error and reaches the caller with Telegram's
// own code (PHONE_CODE_INVALID, FLOOD_WAIT, …) instead of a flattened string.
func (h *authHandler) await(cl *client.Client, status string, in <-chan string, method, field string) error {
	h.ls.beginLogin(cl)
	h.ls.setStatus(status)

	var value string
	select {
	case value = <-in:
	case <-h.ls.cancel:
		return errLoginCanceled
	}

	params, err := json.Marshal(map[string]string{field: value})
	if err != nil {
		h.ls.reportAttempt(err)
		return nil
	}
	_, err = tdjson.Call(context.Background(), cl, method, params)
	h.ls.reportAttempt(err)
	return nil
}

// fatal records an error that ends the login before handing it to go-tdlib,
// which closes the client. The message outlives the client so a status read can
// explain what went wrong.
func (h *authHandler) fatal(err error) error {
	if err != nil {
		h.ls.setLastErr(err)
	}
	return err
}

func (h *authHandler) Close() {}
