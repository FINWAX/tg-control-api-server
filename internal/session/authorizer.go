package session

import (
	"context"
	"errors"

	"github.com/zelenin/go-tdlib/client"
)

// errRehydrateExpired is returned when a session being restored from its binlog
// is no longer authorized (TDLib asks for phone/code/password). Such a session
// needs a fresh login via the API.
var errRehydrateExpired = errors.New("session not authorized in binlog (needs re-login)")

// authHandler implements client.AuthorizationStateHandler for both bots and
// users. It applies TDLib parameters and the proxy up front, then either
// checks the bot token or drives the interactive user flow, taking the code
// and password from the session's channels (fed by HTTP endpoints).
type authHandler struct {
	params    *client.SetTdlibParametersRequest
	proxy     *client.AddProxyRequest // nil = direct
	botToken  string                  // "" = user login
	phone     string
	rehydrate bool // true = restore from binlog only; any login prompt means expired
	ls        *liveSession
}

func (h *authHandler) Handle(cl *client.Client, state client.AuthorizationState) error {
	ctx := context.Background()

	switch state.AuthorizationStateConstructor() {
	case client.ConstructorAuthorizationStateWaitTdlibParameters:
		if _, err := cl.SetTdlibParameters(ctx, h.params); err != nil {
			return err
		}
		if h.proxy != nil {
			if _, err := cl.AddProxy(ctx, h.proxy); err != nil {
				return err
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
			return err
		}
		h.ls.setStatus("awaiting_code")
		_, err := cl.SetAuthenticationPhoneNumber(ctx, &client.SetAuthenticationPhoneNumberRequest{
			PhoneNumber: h.phone,
			Settings:    &client.PhoneNumberAuthenticationSettings{},
		})
		return err

	case client.ConstructorAuthorizationStateWaitCode:
		if h.rehydrate {
			return errRehydrateExpired
		}
		h.ls.setStatus("awaiting_code")
		code := <-h.ls.codeCh
		_, err := cl.CheckAuthenticationCode(ctx, &client.CheckAuthenticationCodeRequest{Code: code})
		return err

	case client.ConstructorAuthorizationStateWaitPassword:
		if h.rehydrate {
			return errRehydrateExpired
		}
		h.ls.setStatus("awaiting_password")
		password := <-h.ls.passwordCh
		_, err := cl.CheckAuthenticationPassword(ctx, &client.CheckAuthenticationPasswordRequest{Password: password})
		return err

	case client.ConstructorAuthorizationStateReady:
		h.ls.setStatus("authorized")
		return nil

	case client.ConstructorAuthorizationStateClosing,
		client.ConstructorAuthorizationStateClosed:
		return nil

	default:
		// Email verification, registration, QR, etc. are out of scope for v0.
		return client.NotSupportedAuthorizationState(state)
	}
}

func (h *authHandler) Close() {}
