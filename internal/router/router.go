// Package router is the gateway's control plane: a stateless HTTP front that
// holds no TDLib clients. It resolves, per request, which worker owns the target
// session (from the Postgres registry) and reverse-proxies to it. Session-scoped
// requests go to the owning worker; stateless ones (app/proxy/execute/create) go
// to any live worker, which becomes the owner for a create.
package router

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/FINWAX/tg-control-api-server/internal/store"
	"github.com/FINWAX/tg-control-api-server/internal/upload"
)

// tokenIDKey tags a request with the authenticated scoped token's id, so a
// handler can filter its result by that token's grants. Absent on master-token
// requests, which see everything.
type tokenIDKey struct{}

type Router struct {
	st        *store.Store
	stale     time.Duration
	master    string // the env master token: full admin access
	uploads   *upload.Store
	transport http.RoundTripper
}

func New(st *store.Store, stale time.Duration, token string, uploads *upload.Store) http.Handler {
	rt := &Router{
		st:      st,
		stale:   stale,
		master:  token,
		uploads: uploads,
		// otelhttp.NewTransport adds client spans and injects the traceparent
		// header, so a request's trace continues into the owning worker.
		transport: otelhttp.NewTransport(&http.Transport{
			DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			MaxIdleConns:        100,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 5 * time.Second,
			// No response-header/overall timeout: login and media can be slow.
		}),
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})

	// Registry reads: served from the gateway directly (pure Postgres, no worker
	// round-trip). These back the management UI's dashboard.
	mux.HandleFunc("GET /v1/apps", rt.listApps)
	mux.HandleFunc("GET /v1/proxies", rt.listProxies)
	mux.HandleFunc("GET /v1/sessions", rt.listSessions)
	mux.HandleFunc("GET /v1/workers", rt.listWorkers)

	// Registry writes served directly by the gateway (rename only; ids immutable).
	mux.HandleFunc("PATCH /v1/apps/{id}", rt.updateApp)
	mux.HandleFunc("PATCH /v1/proxies/{id}", rt.updateProxy)

	// File uploads onto the shared volume (single-shot + resumable). Allowed for
	// any enabled token; the send-time path guard + per-session scope enforce
	// what may actually be sent.
	rt.registerFileRoutes(mux)

	// Scoped API token management (master only, enforced in auth).
	mux.HandleFunc("GET /v1/tokens", rt.listTokens)
	mux.HandleFunc("POST /v1/tokens", rt.createToken)
	mux.HandleFunc("PATCH /v1/tokens/{id}", rt.updateToken)
	mux.HandleFunc("DELETE /v1/tokens/{id}", rt.deleteToken)

	// Stateless / placement: routed to any live worker.
	mux.HandleFunc("POST /v1/execute", rt.anyWorker)
	mux.HandleFunc("POST /v1/apps", rt.anyWorker)
	mux.HandleFunc("POST /v1/proxies", rt.anyWorker)
	mux.HandleFunc("POST /v1/bot", rt.anyWorker)
	mux.HandleFunc("POST /v1/user", rt.anyWorker)

	// Session-scoped: routed to the worker that owns {id}.
	mux.HandleFunc("POST /v1/user/{id}/login/code", rt.scoped)
	mux.HandleFunc("POST /v1/user/{id}/login/password", rt.scoped)
	mux.HandleFunc("POST /v1/user/{id}/call", rt.scoped)
	mux.HandleFunc("POST /v1/bot/{id}/call", rt.scoped)
	mux.HandleFunc("PUT /v1/user/{id}/updates/webhook", rt.scoped)
	mux.HandleFunc("PUT /v1/bot/{id}/updates/webhook", rt.scoped)
	mux.HandleFunc("DELETE /v1/user/{id}/updates/webhook", rt.scoped)
	mux.HandleFunc("DELETE /v1/bot/{id}/updates/webhook", rt.scoped)
	mux.HandleFunc("GET /v1/user/{id}", rt.scoped)
	mux.HandleFunc("GET /v1/bot/{id}", rt.scoped)
	mux.HandleFunc("GET /v1/user/{id}/files/{file_id}", rt.scoped)
	mux.HandleFunc("GET /v1/bot/{id}/files/{file_id}", rt.scoped)
	mux.HandleFunc("DELETE /v1/user/{id}", rt.deleteSession)
	mux.HandleFunc("DELETE /v1/bot/{id}", rt.deleteSession)
	mux.HandleFunc("PATCH /v1/user/{id}", rt.updateSession)
	mux.HandleFunc("PATCH /v1/bot/{id}", rt.updateSession)

	return rt.auth(mux)
}

// auth enforces the token model. The master token (from env) grants everything.
// A scoped DB token (looked up by secret hash) may only invoke a session's /call
// or read its status, and only for sessions its scope covers; everything else is
// master-only. GET /healthz is always open for liveness probes.
func (rt *Router) auth(next http.Handler) http.Handler {
	masterWant := []byte("Bearer " + rt.master)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		authz := r.Header.Get("Authorization")
		if authz == "" {
			writeErr(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		if subtle.ConstantTimeCompare([]byte(authz), masterWant) == 1 {
			next.ServeHTTP(w, r) // master: full access
			return
		}
		tok, ok := strings.CutPrefix(authz, "Bearer ")
		if !ok || tok == "" {
			writeErr(w, http.StatusUnauthorized, "invalid bearer token")
			return
		}
		sum := sha256.Sum256([]byte(tok))
		id, enabled, found, err := rt.st.ResolveToken(r.Context(), sum[:])
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		if !found {
			writeErr(w, http.StatusUnauthorized, "invalid bearer token")
			return
		}
		if !enabled {
			writeErr(w, http.StatusForbidden, "token is disabled")
			return
		}
		// Uploading bytes to the shared volume isn't session-scoped; any enabled
		// token may do it. The file only becomes sendable through a session's
		// /call, which the path guard + token scope still gate.
		if isFilesPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		// /v1/execute is td_execute: pure local computation (parseTextEntities,
		// getFileMimeType, …) against no session and no account state, so there
		// is no scope for it to violate. Blocked methods are still refused.
		if r.Method == http.MethodPost && r.URL.Path == "/v1/execute" {
			next.ServeHTTP(w, r)
			return
		}
		// The session listing is filtered to this token's own grants (see
		// listSessions), so it reveals nothing the token cannot already address.
		// It exists so an integrator can discover full session ids and kinds
		// without being handed the master token.
		if r.Method == http.MethodGet && r.URL.Path == "/v1/sessions" {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tokenIDKey{}, id)))
			return
		}
		sid, ok := sessionTarget(r.Method, r.URL.Path)
		if !ok {
			writeErr(w, http.StatusForbidden, "token limited to session calls; management requires the master token")
			return
		}
		granted, err := rt.st.TokenGrantsSession(r.Context(), id, sid)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		if !granted {
			writeErr(w, http.StatusForbidden, "token not permitted for this session")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sessionTarget reports whether (method, path) is a route a scoped token may use
// — a session's /call or status read — and returns that session id. Everything
// else is master-only.
func sessionTarget(method, path string) (string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 3 || parts[0] != "v1" || (parts[1] != "user" && parts[1] != "bot") {
		return "", false
	}
	id := parts[2]
	if method == http.MethodGet && len(parts) == 3 {
		return id, true // GET /v1/{kind}/{id} — status
	}
	if method == http.MethodPost && len(parts) == 4 && parts[3] == "call" {
		return id, true // POST /v1/{kind}/{id}/call
	}
	if method == http.MethodGet && len(parts) == 5 && parts[3] == "files" {
		return id, true // GET /v1/{kind}/{id}/files/{file_id} — download
	}
	return "", false
}

// --- registry read handlers ---

func (rt *Router) listApps(w http.ResponseWriter, r *http.Request) {
	items, err := rt.st.ListApps(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeOK(w, items)
}

func (rt *Router) listProxies(w http.ResponseWriter, r *http.Request) {
	items, err := rt.st.ListProxies(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeOK(w, items)
}

// listSessions returns the whole registry for the master token, or — when auth
// tagged the request with a scoped token — only the sessions that token's grants
// cover.
func (rt *Router) listSessions(w http.ResponseWriter, r *http.Request) {
	var (
		items []store.SessionInfo
		err   error
	)
	if tokenID, scoped := r.Context().Value(tokenIDKey{}).(string); scoped {
		items, err = rt.st.ListSessionsForToken(r.Context(), tokenID)
	} else {
		items, err = rt.st.ListSessions(r.Context())
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeOK(w, items)
}

func (rt *Router) listWorkers(w http.ResponseWriter, r *http.Request) {
	items, err := rt.st.ListWorkers(r.Context(), time.Now().Add(-rt.stale))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeOK(w, items)
}

// --- registry write handlers (rename; ids stay immutable) ---

func (rt *Router) updateApp(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Label string `json:"label"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	found, err := rt.st.UpdateAppLabel(r.Context(), r.PathValue("id"), req.Label)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "app not found")
		return
	}
	writeOK(w, map[string]any{"status": "updated"})
}

func (rt *Router) updateProxy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Label string `json:"label"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	found, err := rt.st.UpdateProxyLabel(r.Context(), r.PathValue("id"), req.Label)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "proxy not found")
		return
	}
	writeOK(w, map[string]any{"status": "updated"})
}

// --- scoped API token handlers (master only, per auth middleware) ---

func (rt *Router) listTokens(w http.ResponseWriter, r *http.Request) {
	items, err := rt.st.ListTokens(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeOK(w, items)
}

func (rt *Router) createToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string   `json:"name"`
		Enabled     *bool    `json:"enabled"`
		AllSessions bool     `json:"all_sessions"`
		AppIDs      []string `json:"app_ids"`
		SessionIDs  []string `json:"session_ids"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	secret, err := newToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	sum := sha256.Sum256([]byte(secret))
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	id, err := rt.st.CreateToken(r.Context(), req.Name, sum[:], enabled, req.AllSessions, req.AppIDs, req.SessionIDs)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	// The plaintext token is returned once and never stored.
	writeOK(w, map[string]any{"id": id, "token": secret})
}

func (rt *Router) updateToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        *string   `json:"name"`
		Enabled     *bool     `json:"enabled"`
		AllSessions *bool     `json:"all_sessions"`
		AppIDs      *[]string `json:"app_ids"`
		SessionIDs  *[]string `json:"session_ids"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	found, err := rt.st.UpdateToken(r.Context(), r.PathValue("id"), store.TokenPatch{
		Name: req.Name, Enabled: req.Enabled, AllSessions: req.AllSessions,
		AppIDs: req.AppIDs, SessionIDs: req.SessionIDs,
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "token not found")
		return
	}
	writeOK(w, map[string]any{"status": "updated"})
}

func (rt *Router) deleteToken(w http.ResponseWriter, r *http.Request) {
	found, err := rt.st.DeleteToken(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "token not found")
		return
	}
	writeOK(w, map[string]any{"status": "deleted"})
}

// newToken returns a fresh 256-bit URL-safe secret for a scoped API token.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// scoped forwards to the worker currently owning the path's {id}.
func (rt *Router) scoped(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	addr, err := rt.st.SessionRoute(r.Context(), id, time.Now().Add(-rt.stale))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if addr == "" {
		writeErr(w, http.StatusServiceUnavailable, "session not currently hosted by a live worker")
		return
	}
	rt.forward(w, r, addr)
}

// deleteSession routes a session deletion to its owning worker when one is alive
// (so the live client is closed on the worker actually holding it); otherwise to
// any live worker, which removes the orphan's registry row and on-disk directory.
func (rt *Router) deleteSession(w http.ResponseWriter, r *http.Request) {
	cutoff := time.Now().Add(-rt.stale)
	addr, err := rt.st.SessionRoute(r.Context(), r.PathValue("id"), cutoff)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if addr == "" {
		if addr, err = rt.st.PickWorker(r.Context(), cutoff); err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	if addr == "" {
		writeErr(w, http.StatusServiceUnavailable, "no live workers available")
		return
	}
	rt.forward(w, r, addr)
}

// updateSession routes a session edit (label/proxy) to its owning worker when
// alive (so a proxy change is applied on the live client), otherwise to any live
// worker, which updates only the registry. Same routing as deleteSession.
func (rt *Router) updateSession(w http.ResponseWriter, r *http.Request) {
	cutoff := time.Now().Add(-rt.stale)
	addr, err := rt.st.SessionRoute(r.Context(), r.PathValue("id"), cutoff)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if addr == "" {
		if addr, err = rt.st.PickWorker(r.Context(), cutoff); err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	if addr == "" {
		writeErr(w, http.StatusServiceUnavailable, "no live workers available")
		return
	}
	rt.forward(w, r, addr)
}

// anyWorker forwards to the least-loaded live worker.
func (rt *Router) anyWorker(w http.ResponseWriter, r *http.Request) {
	addr, err := rt.st.PickWorker(r.Context(), time.Now().Add(-rt.stale))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if addr == "" {
		writeErr(w, http.StatusServiceUnavailable, "no live workers available")
		return
	}
	rt.forward(w, r, addr)
}

func (rt *Router) forward(w http.ResponseWriter, r *http.Request, base string) {
	target, err := url.Parse(base)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "bad worker address")
		return
	}
	p := httputil.NewSingleHostReverseProxy(target)
	p.Transport = rt.transport
	p.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		writeErr(w, http.StatusBadGateway, "worker unreachable: "+err.Error())
	}
	p.ServeHTTP(w, r)
}

// maxBodyBytes caps management request bodies (labels, token grants) — all
// small — so an oversized payload can't exhaust memory during decode. Session
// /call bodies are streamed to the worker, which enforces its own larger cap.
const maxBodyBytes = 1 << 20 // 1 MiB

// decodeJSON reads a JSON body into dst, writing a 400 and returning false on
// malformed input. An empty body is treated as an empty object (partial updates).
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil || r.ContentLength == 0 {
		return true
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

func writeOK(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

// writeErr renders a control-plane failure (auth, routing, no live worker). It
// is tagged source:"gateway" to match the worker's envelope, so a client can
// separate infrastructure failures from Telegram's own rejections
// (source:"tdlib") without matching on the message text.
func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":    false,
		"error": map[string]any{"message": msg, "source": "gateway"},
	})
}
