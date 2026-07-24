// Package session is the worker layer: it owns live TDLib clients (one per
// account), drives login through an HTTP-friendly authorizer, applies the
// per-session proxy, and routes dynamic /call requests. The Postgres store is
// the source of truth; this holds the in-process live clients keyed by id.
package session

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zelenin/go-tdlib/client"

	"github.com/FINWAX/tg-control-api-server/internal/secret"
	"github.com/FINWAX/tg-control-api-server/internal/store"
	"github.com/FINWAX/tg-control-api-server/internal/tdjson"
)

// liveSession is the in-process state of one account.
type liveSession struct {
	id   string
	kind string

	mu      sync.Mutex
	status  string
	cl      *client.Client
	err     error
	hasWH   bool            // a webhook subscription exists -> updates go to the outbox
	whTypes map[string]bool // allowed update @types; nil = all

	codeCh     chan string
	passwordCh chan string
}

// updateHandler queues td_api Update objects for the session's webhook (if
// subscribed and not filtered out). It runs on the client's receiver
// goroutine, so it must not block: the type check and json.Marshal are cheap
// and the outbox send is non-blocking.
func (m *Manager) updateHandler(ls *liveSession) client.ResultHandler {
	return client.NewCallbackResultHandler(func(result client.Type) {
		if result.GetType() != client.TypeUpdate {
			return
		}
		ls.mu.Lock()
		on := ls.hasWH
		types := ls.whTypes
		ls.mu.Unlock()
		if !on {
			return
		}
		if types != nil && !types[result.GetConstructor()] {
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			return
		}
		select {
		case m.outbox <- outItem{sessionID: ls.id, payload: b}:
		default:
			log.Printf("outbox: queue full, dropping update for %s", ls.id)
		}
	})
}

// filterSpec is the stored webhook filter: an allowlist of update @types.
type filterSpec struct {
	Types []string `json:"types"`
}

// typeSet builds an allowlist set from update types, or nil for "all".
func typeSet(types []string) map[string]bool {
	if len(types) == 0 {
		return nil
	}
	set := make(map[string]bool, len(types))
	for _, t := range types {
		set[t] = true
	}
	return set
}

func (s *liveSession) setStatus(st string) {
	s.mu.Lock()
	s.status = st
	s.mu.Unlock()
}

func (s *liveSession) status_() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *liveSession) setClient(cl *client.Client) {
	s.mu.Lock()
	s.cl = cl
	s.status = "authorized"
	s.mu.Unlock()
}

func (s *liveSession) client() *client.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cl
}

// Manager holds live clients and coordinates their lifecycle. It runs inside a
// worker: it owns a subset of sessions, heartbeats its liveness to the registry,
// and claims orphaned (unowned or dead-owner) sessions up to its capacity.
type Manager struct {
	st      *store.Store
	sec     *secret.Box
	dataDir string

	workerID string // this worker's identity in the registry
	selfAddr string // base URL the gateway dials to reach this worker
	capacity int    // max sessions this worker will hold

	outbox chan outItem

	stopping atomic.Bool // set on graceful shutdown; heartbeat/reconciler stand down

	mu    sync.RWMutex
	live  map[string]*liveSession
	retry map[string]time.Time // session id -> earliest next open attempt (backoff)
}

// worker coordination cadence.
const (
	workerHeartbeat   = 10 * time.Second // how often we refresh our liveness
	workerStale       = 30 * time.Second // an owner past this is considered dead
	reconcileInterval = 3 * time.Second  // how often we reconcile ownership
	claimBatch        = 8                // max sessions claimed per reconcile
	shedPerTick       = 2                // max sessions shed per reconcile
	openBackoff       = 20 * time.Second // wait this long before retrying a failed open
)

func NewManager(st *store.Store, sec *secret.Box, dataDir, workerID, selfAddr string, capacity int) *Manager {
	m := &Manager{
		st: st, sec: sec, dataDir: dataDir,
		workerID: workerID, selfAddr: selfAddr, capacity: capacity,
		outbox: make(chan outItem, 1024),
		live:   map[string]*liveSession{},
		retry:  map[string]time.Time{},
	}
	// Register synchronously so the first reconcile already counts this worker.
	_ = st.RegisterWorker(context.Background(), workerID, selfAddr)
	go m.runHeartbeat()
	go m.runReconciler()
	go m.runOutboxWriter()
	go m.runOutboxDispatcher()
	go m.runRetention()
	return m
}

// runHeartbeat keeps this worker's registry row fresh so the gateway routes to
// it and so its sessions aren't reclaimed by peers.
func (m *Manager) runHeartbeat() {
	ctx := context.Background()
	beat := func() {
		if err := m.st.RegisterWorker(ctx, m.workerID, m.selfAddr); err != nil {
			log.Printf("worker %s heartbeat: %v", m.workerID, err)
		}
	}
	beat()
	t := time.NewTicker(workerHeartbeat)
	defer t.Stop()
	for range t.C {
		if m.stopping.Load() {
			return
		}
		beat()
	}
}

// runReconciler drives ownership convergence on a fixed cadence. Each pass: (1)
// fencing — for sessions the registry reassigned to a live peer, re-assert if we
// still hold a working client (active holder wins), else drop them; (2) adopt
// sessions the registry assigns to us but we aren't holding yet (restart,
// handoff, retry after a backoff); (3) balance toward a fair share by claiming
// orphans or shedding excess.
func (m *Manager) runReconciler() {
	ctx := context.Background()
	m.reconcile(ctx)
	t := time.NewTicker(reconcileInterval)
	defer t.Stop()
	for range t.C {
		if m.stopping.Load() {
			return
		}
		m.reconcile(ctx)
	}
}

func (m *Manager) reconcile(ctx context.Context) {
	if m.stopping.Load() {
		return
	}
	cutoff := time.Now().Add(-workerStale)

	// (1) Fencing: for sessions the registry now assigns to a live peer, the
	// active holder wins. If we still hold a working client (e.g. a peer stole
	// it after our heartbeat merely lapsed), we re-assert ownership — this avoids
	// closing a live client, which go-tdlib v1.0.0-beta1 can't do safely while
	// the process keeps running. If we have no live client, we drop it and let
	// the peer have it.
	m.mu.RLock()
	held := make([]string, 0, len(m.live))
	for id := range m.live {
		held = append(held, id)
	}
	m.mu.RUnlock()
	if foreign, err := m.st.ForeignOwned(ctx, m.workerID, held, cutoff); err != nil {
		log.Printf("reconcile: foreign-owned: %v", err)
	} else {
		for _, id := range foreign {
			if ls := m.get(id); ls != nil && ls.client() != nil {
				if err := m.st.SetSessionOwner(ctx, id, m.workerID); err != nil {
					log.Printf("re-assert %s: %v", id, err)
				} else {
					log.Printf("re-asserted %s (holding a live client)", id)
				}
			} else {
				m.delete(id)
			}
		}
	}

	// (2) Adopt: hydrate sessions the registry assigns to us but we aren't
	// holding live yet (post-restart, directed handoff, or retry after a
	// transient open failure). Sessions in open-backoff are skipped.
	if owned, err := m.st.ListOwned(ctx, m.workerID); err != nil {
		log.Printf("reconcile: list owned: %v", err)
	} else {
		for _, r := range owned {
			if m.get(r.ID) != nil || !m.retryReady(r.ID) {
				continue
			}
			m.hydrateLogged(ctx, r, "adopted", "adopt")
		}
	}

	// (3) Balance toward a fair share = ceil(total authorized / live workers).
	mine, total, live, err := m.st.WorkerLoad(ctx, m.workerID, cutoff)
	if err != nil {
		log.Printf("reconcile: load: %v", err)
		return
	}
	if live < 1 {
		live = 1
	}
	share := (total + live - 1) / live

	m.mu.RLock()
	heldN := len(m.live)
	m.mu.RUnlock()

	switch {
	case mine > share:
		n := mine - share
		if n > shedPerTick {
			n = shedPerTick
		}
		m.shed(ctx, n)
	case mine < share && heldN < m.capacity:
		limit := share - mine
		if limit > claimBatch {
			limit = claimBatch
		}
		if limit > m.capacity-heldN {
			limit = m.capacity - heldN
		}
		if limit <= 0 {
			return
		}
		rows, err := m.st.ClaimOrphans(ctx, m.workerID, cutoff, limit)
		if err != nil {
			log.Printf("reconcile: claim: %v", err)
			return
		}
		for _, r := range rows {
			if !m.retryReady(r.ID) {
				continue
			}
			m.hydrateLogged(ctx, r, "claimed", "claim")
		}
	}
}

// hydrateLogged rehydrates one session and logs the outcome. A transient open
// failure (e.g. binlog still locked by a yielding owner) is not fatal — the
// next reconcile retries; only a genuinely expired binlog is terminal.
func (m *Manager) hydrateLogged(ctx context.Context, r store.Rehydratable, okVerb, errVerb string) {
	switch err := m.rehydrateOne(ctx, r); {
	case err == nil:
		log.Printf("%s %s (%s)", okVerb, r.ID, r.Kind)
	case errors.Is(err, errRehydrateExpired):
		log.Printf("%s %s: expired binlog", errVerb, r.ID)
	default:
		log.Printf("%s %s (%s): %v (will retry)", errVerb, r.ID, r.Kind, err)
	}
}

// shed gracefully releases up to n of this worker's sessions so lighter peers
// reclaim them: close the client (release binlog) then clear ownership.
func (m *Manager) shed(ctx context.Context, n int) {
	m.mu.RLock()
	ids := make([]string, 0, n)
	for id := range m.live {
		ids = append(ids, id)
		if len(ids) == n {
			break
		}
	}
	m.mu.RUnlock()
	for _, id := range ids {
		if ls := m.get(id); ls != nil {
			if cl := ls.client(); cl != nil {
				closeClient(ctx, cl)
			}
		}
		m.delete(id)
		if err := m.st.ReleaseSession(ctx, id); err != nil {
			log.Printf("shed %s: %v", id, err)
		} else {
			log.Printf("shed %s to rebalance", id)
		}
	}
}

// closeClient cleanly closes a TDLib client so its binlog lock releases before a
// peer opens the same directory. Best-effort with a short bound.
func closeClient(ctx context.Context, cl *client.Client) {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, _ = tdjson.Call(cctx, cl, "close", nil)
}

// Shutdown gracefully hands this worker's sessions to its peers. It stops
// claiming, cleanly closes its TDLib clients (releasing the binlog locks so a
// peer can open them), clears its ownership in the registry (sessions become
// orphans and are reclaimed on the next 3s sweep), and deregisters itself.
// Called on SIGTERM so a planned restart fails over in seconds instead of after
// the 30s staleness window.
func (m *Manager) Shutdown(ctx context.Context) {
	m.stopping.Store(true)

	// Snapshot live clients, then close each so its binlog lock releases before
	// a peer opens the same directory. Best-effort with a short per-client bound.
	m.mu.RLock()
	clients := make([]*client.Client, 0, len(m.live))
	for _, ls := range m.live {
		if cl := ls.client(); cl != nil {
			clients = append(clients, cl)
		}
	}
	m.mu.RUnlock()
	var wg sync.WaitGroup
	for _, cl := range clients {
		wg.Add(1)
		go func(cl *client.Client) {
			defer wg.Done()
			closeClient(ctx, cl)
		}(cl)
	}
	wg.Wait()

	// Release ownership so peers reclaim immediately, then leave the registry.
	if n, err := m.st.ReleaseSessions(ctx, m.workerID); err != nil {
		log.Printf("shutdown: release sessions: %v", err)
	} else {
		log.Printf("worker %s shutting down: released %d sessions to peers", m.workerID, n)
	}
	if err := m.st.DeregisterWorker(ctx, m.workerID); err != nil {
		log.Printf("shutdown: deregister: %v", err)
	}
}

func (m *Manager) put(s *liveSession) {
	m.mu.Lock()
	m.live[s.id] = s
	m.mu.Unlock()
}

func (m *Manager) get(id string) *liveSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.live[id]
}

func (m *Manager) delete(id string) {
	m.mu.Lock()
	delete(m.live, id)
	m.mu.Unlock()
}

// retryReady reports whether a session is past its open-backoff (or has none).
func (m *Manager) retryReady(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.retry[id]
	return !ok || time.Now().After(t)
}

// backoff defers the next open attempt for a session after a transient failure,
// so a locked binlog (owner mid-yield or paused) isn't hammered — repeated
// failed NewClient calls both waste work and stress go-tdlib's client cleanup.
func (m *Manager) backoff(id string) {
	m.mu.Lock()
	m.retry[id] = time.Now().Add(openBackoff)
	m.mu.Unlock()
}

func (m *Manager) clearRetry(id string) {
	m.mu.Lock()
	delete(m.retry, id)
	m.mu.Unlock()
}

func (m *Manager) buildParams(dbDir string, dbKey []byte, apiID int32, apiHash string) *client.SetTdlibParametersRequest {
	return &client.SetTdlibParametersRequest{
		UseTestDc:             false,
		DatabaseDirectory:     filepath.Join(dbDir, "db"),
		FilesDirectory:        filepath.Join(dbDir, "files"),
		DatabaseEncryptionKey: dbKey,
		UseFileDatabase:       true,
		UseChatInfoDatabase:   false,
		UseMessageDatabase:    false,
		UseSecretChats:        false,
		ApiId:                 apiID,
		ApiHash:               apiHash,
		SystemLanguageCode:    "en",
		DeviceModel:           "tgcontrol",
		SystemVersion:         "1",
		ApplicationVersion:    "0.1",
	}
}

func (m *Manager) proxyRequest(ctx context.Context, proxyID string) (*client.AddProxyRequest, error) {
	if proxyID == "" {
		return nil, nil
	}
	p, err := m.st.GetProxy(ctx, proxyID)
	if err != nil {
		return nil, err
	}
	var sec string
	if len(p.SecretEnc) > 0 {
		b, err := m.sec.Decrypt(p.SecretEnc)
		if err != nil {
			return nil, err
		}
		sec = string(b)
	}
	var pt client.ProxyType
	switch p.Type {
	case "socks5":
		pt = &client.ProxyTypeSocks5{Username: p.Username, Password: sec}
	case "http":
		pt = &client.ProxyTypeHttp{Username: p.Username, Password: sec}
	case "mtproto":
		pt = &client.ProxyTypeMtproto{Secret: sec}
	default:
		return nil, fmt.Errorf("unknown proxy type %q", p.Type)
	}
	return &client.AddProxyRequest{
		Server: p.Host,
		Port:   p.Port,
		Enable: true,
		Type:   pt,
	}, nil
}

// rehydrateOne restores one session from its on-disk binlog: the TDLib client
// is rebuilt on its existing directory (the binlog already holds the
// authorization) so it reaches Ready without phone/code, webhook delivery is
// re-armed, and the live map is repopulated. A binlog that no longer authorizes
// yields errRehydrateExpired and the session is marked expired.
func (m *Manager) rehydrateOne(ctx context.Context, r store.Rehydratable) error {
	app, err := m.st.GetApp(ctx, r.AppID)
	if err != nil {
		return fmt.Errorf("app: %w", err)
	}
	apiHash, err := m.sec.Decrypt(app.APIHashEnc)
	if err != nil {
		return err
	}
	proxyReq, err := m.proxyRequest(ctx, r.ProxyID)
	if err != nil {
		return fmt.Errorf("proxy: %w", err)
	}
	dbKey, err := m.sec.Decrypt(r.DBKeyEnc)
	if err != nil {
		return err
	}

	ls := &liveSession{id: r.ID, kind: r.Kind, status: "connecting",
		codeCh: make(chan string, 1), passwordCh: make(chan string, 1)}
	m.put(ls)

	h := &authHandler{
		params:    m.buildParams(r.DBDir, dbKey, app.APIID, string(apiHash)),
		proxy:     proxyReq,
		rehydrate: true,
		ls:        ls,
	}
	cl, err := client.NewClient(h, client.WithResultHandler(m.updateHandler(ls)))
	if err != nil {
		// Drop the half-live entry so a later reconcile can retry.
		m.delete(r.ID)
		// Only a genuinely unauthorized binlog is terminal. Other failures
		// (notably the binlog still locked by another holder) are transient:
		// leave ownership as-is and back off before retrying.
		if errors.Is(err, errRehydrateExpired) {
			m.clearRetry(r.ID)
			_ = m.st.UpdateSessionStatus(ctx, r.ID, "expired")
		} else {
			m.backoff(r.ID)
		}
		return err
	}
	m.clearRetry(r.ID)
	ls.setClient(cl)

	// Re-arm webhook delivery if a subscription exists (target lives in the DB;
	// the dispatcher reads it per delivery). Filters are applied in-process, so
	// load them into the live session.
	if w, err := m.st.GetWebhook(ctx, r.ID); err == nil && w != nil {
		var types []string
		if len(w.Filters) > 0 {
			var fs filterSpec
			if json.Unmarshal(w.Filters, &fs) == nil {
				types = fs.Types
			}
		}
		ls.mu.Lock()
		ls.hasWH = true
		ls.whTypes = typeSet(types)
		ls.mu.Unlock()
	}
	return nil
}

// CreateBot logs in a bot (non-interactive) and returns its getMe result.
func (m *Manager) CreateBot(ctx context.Context, appID, token, proxyID, label string) (id string, me json.RawMessage, err error) {
	app, err := m.st.GetApp(ctx, appID)
	if err != nil {
		return "", nil, fmt.Errorf("app: %w", err)
	}
	apiHash, err := m.sec.Decrypt(app.APIHashEnc)
	if err != nil {
		return "", nil, err
	}
	proxyReq, err := m.proxyRequest(ctx, proxyID)
	if err != nil {
		return "", nil, fmt.Errorf("proxy: %w", err)
	}
	tokEnc, err := m.sec.Encrypt([]byte(token))
	if err != nil {
		return "", nil, err
	}
	dbKey := make([]byte, 32)
	if _, err = rand.Read(dbKey); err != nil {
		return "", nil, err
	}
	dbKeyEnc, err := m.sec.Encrypt(dbKey)
	if err != nil {
		return "", nil, err
	}
	id, err = m.st.CreateSession(ctx, store.NewSession{
		Kind: "bot", AppID: appID, ProxyID: proxyID, BotTokenEnc: tokEnc, DBKeyEnc: dbKeyEnc, Label: label,
	})
	if err != nil {
		return "", nil, err
	}
	dbDir := filepath.Join(m.dataDir, id)
	_ = m.st.SetSessionDBDir(ctx, id, dbDir)
	_ = m.st.SetSessionOwner(ctx, id, m.workerID)

	ls := &liveSession{id: id, kind: "bot", status: "connecting",
		codeCh: make(chan string, 1), passwordCh: make(chan string, 1)}
	m.put(ls)

	h := &authHandler{
		params:   m.buildParams(dbDir, dbKey, app.APIID, string(apiHash)),
		proxy:    proxyReq,
		botToken: token,
		ls:       ls,
	}

	done := make(chan error, 1)
	go func() {
		cl, e := client.NewClient(h, client.WithResultHandler(m.updateHandler(ls)))
		if e == nil {
			ls.setClient(cl)
		}
		done <- e
	}()

	select {
	case e := <-done:
		if e != nil {
			ls.setStatus("error")
			_ = m.st.UpdateSessionStatus(ctx, id, "error")
			return id, nil, e
		}
	case <-time.After(45 * time.Second):
		ls.setStatus("error")
		_ = m.st.UpdateSessionStatus(ctx, id, "error")
		return id, nil, errors.New("bot login timeout")
	}

	_ = m.st.UpdateSessionStatus(ctx, id, "authorized")
	me, err = tdjson.Call(ctx, ls.client(), "getMe", nil)
	return id, me, err
}

// CreateUser starts a user login (phone set automatically) and returns as soon
// as the flow reaches a waiting state. The code/password arrive later via
// SubmitCode/SubmitPassword.
func (m *Manager) CreateUser(ctx context.Context, appID, phone, proxyID, label string) (id, status string, err error) {
	app, err := m.st.GetApp(ctx, appID)
	if err != nil {
		return "", "", fmt.Errorf("app: %w", err)
	}
	apiHash, err := m.sec.Decrypt(app.APIHashEnc)
	if err != nil {
		return "", "", err
	}
	proxyReq, err := m.proxyRequest(ctx, proxyID)
	if err != nil {
		return "", "", fmt.Errorf("proxy: %w", err)
	}
	dbKey := make([]byte, 32)
	if _, err = rand.Read(dbKey); err != nil {
		return "", "", err
	}
	dbKeyEnc, err := m.sec.Encrypt(dbKey)
	if err != nil {
		return "", "", err
	}
	id, err = m.st.CreateSession(ctx, store.NewSession{
		Kind: "user", AppID: appID, ProxyID: proxyID, Phone: phone, DBKeyEnc: dbKeyEnc, Label: label,
	})
	if err != nil {
		return "", "", err
	}
	dbDir := filepath.Join(m.dataDir, id)
	_ = m.st.SetSessionDBDir(ctx, id, dbDir)
	_ = m.st.SetSessionOwner(ctx, id, m.workerID)

	ls := &liveSession{id: id, kind: "user", status: "connecting",
		codeCh: make(chan string, 1), passwordCh: make(chan string, 1)}
	m.put(ls)

	h := &authHandler{
		params: m.buildParams(dbDir, dbKey, app.APIID, string(apiHash)),
		proxy:  proxyReq,
		phone:  phone,
		ls:     ls,
	}

	go func() {
		cl, e := client.NewClient(h, client.WithResultHandler(m.updateHandler(ls)))
		if e != nil {
			ls.mu.Lock()
			ls.err = e
			if ls.status != "authorized" {
				ls.status = "error"
			}
			ls.mu.Unlock()
			_ = m.st.UpdateSessionStatus(context.Background(), id, "error")
			return
		}
		ls.setClient(cl)
		_ = m.st.UpdateSessionStatus(context.Background(), id, "authorized")
	}()

	status = waitLeave(ls, "connecting", 20*time.Second)
	return id, status, nil
}

// SubmitCode delivers the login code and waits for the next state.
func (m *Manager) SubmitCode(id, code string) (string, error) {
	ls := m.get(id)
	if ls == nil {
		return "", errors.New("session not found")
	}
	if ls.status_() != "awaiting_code" {
		return ls.status_(), fmt.Errorf("session is %s, not awaiting_code", ls.status_())
	}
	ls.codeCh <- code
	return waitLeave(ls, "awaiting_code", 20*time.Second), nil
}

// SubmitPassword delivers the 2FA password and waits for the next state.
func (m *Manager) SubmitPassword(id, password string) (string, error) {
	ls := m.get(id)
	if ls == nil {
		return "", errors.New("session not found")
	}
	if ls.status_() != "awaiting_password" {
		return ls.status_(), fmt.Errorf("session is %s, not awaiting_password", ls.status_())
	}
	ls.passwordCh <- password
	return waitLeave(ls, "awaiting_password", 20*time.Second), nil
}

// blockedMethods are td_api functions the gateway manages itself; callers must
// not invoke them via /call or /execute. They would break the session lifecycle
// or auth (close/destroy/logOut/setTdlibParameters/checkAuthentication*), bypass
// the mandatory per-session proxy (add/edit/remove/enable/disableProxy), or
// reconfigure process-global logging. Notably `close` would crash the worker via
// a go-tdlib receiver race. Read-only proxy diagnostics stay allowed.
var blockedMethods = map[string]bool{
	"close":                        true,
	"destroy":                      true,
	"logOut":                       true,
	"setTdlibParameters":           true,
	"setDatabaseEncryptionKey":     true,
	"setAuthenticationPhoneNumber": true,
	"checkAuthenticationCode":      true,
	"checkAuthenticationPassword":  true,
	"checkAuthenticationBotToken":  true,
	"checkAuthenticationEmailCode": true,
	"resendAuthenticationCode":     true,
	"registerUser":                 true,
	"requestQrCodeAuthentication":  true,
	"addProxy":                     true,
	"editProxy":                    true,
	"removeProxy":                  true,
	"enableProxy":                  true,
	"disableProxy":                 true,
	"setLogStream":                 true,
	"setLogVerbosityLevel":         true,
	"setLogTagVerbosityLevel":      true,
}

// IsBlockedMethod reports whether a td_api method is reserved to the gateway and
// must be rejected when requested through the public API.
func IsBlockedMethod(method string) bool { return blockedMethods[method] }

// Call runs a dynamic td_api method on an authorized session. On a "Chat not
// found" error for a private chat (positive chat_id == user id), it force-loads
// the chat via createPrivateChat and retries once — TDLib lazy-loads dialogs, so
// a first send to a not-yet-materialized private chat would otherwise fail.
func (m *Manager) Call(ctx context.Context, id, method string, params json.RawMessage) (json.RawMessage, error) {
	ls := m.get(id)
	if ls == nil {
		return nil, errors.New("session not found")
	}
	cl := ls.client()
	if cl == nil {
		return nil, fmt.Errorf("session is %s, not authorized", ls.status_())
	}
	res, err := tdjson.Call(ctx, cl, method, params)
	if err != nil && isChatNotFound(err) {
		if uid, ok := positiveChatID(params); ok {
			if _, e := tdjson.Call(ctx, cl, "createPrivateChat", privateChatParams(uid)); e == nil {
				log.Printf("call %s: resolved private chat %d, retrying %s", id, uid, method)
				res, err = tdjson.Call(ctx, cl, method, params)
			}
		}
	}
	return res, err
}

func isChatNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Chat not found")
}

// positiveChatID extracts chat_id from params when it is a private chat (a
// positive id equal to the peer's user id); groups/channels have negative ids
// and cannot be force-loaded this way.
func positiveChatID(params json.RawMessage) (int64, bool) {
	if len(params) == 0 {
		return 0, false
	}
	var p struct {
		ChatID int64 `json:"chat_id"`
	}
	if json.Unmarshal(params, &p) != nil || p.ChatID <= 0 {
		return 0, false
	}
	return p.ChatID, true
}

func privateChatParams(userID int64) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"user_id": userID, "force": true})
	return b
}

// SetWebhook persists the session's webhook subscription and (re)arms live
// delivery. Secret is encrypted at rest; the plaintext is kept in-process for
// HMAC signing.
func (m *Manager) SetWebhook(ctx context.Context, id, url, secret string, types []string) error {
	ls := m.get(id)
	if ls == nil {
		return errors.New("session not found")
	}
	var secEnc []byte
	if secret != "" {
		var err error
		if secEnc, err = m.sec.Encrypt([]byte(secret)); err != nil {
			return err
		}
	}
	var filters []byte
	if len(types) > 0 {
		var err error
		if filters, err = json.Marshal(filterSpec{Types: types}); err != nil {
			return err
		}
	}
	if err := m.st.SetWebhook(ctx, id, url, secEnc, filters); err != nil {
		return err
	}
	ls.mu.Lock()
	ls.hasWH = true
	ls.whTypes = typeSet(types)
	ls.mu.Unlock()
	return nil
}

// DeleteWebhook removes the session's webhook subscription, cancels its
// still-pending deliveries, and disarms in-process delivery. Idempotent.
func (m *Manager) DeleteWebhook(ctx context.Context, id string) error {
	if err := m.st.DeleteWebhook(ctx, id); err != nil {
		return err
	}
	if err := m.st.CancelPendingDeliveries(ctx, id); err != nil {
		return err
	}
	if ls := m.get(id); ls != nil {
		ls.mu.Lock()
		ls.hasWH = false
		ls.whTypes = nil
		ls.mu.Unlock()
	}
	return nil
}

// UpdateSession changes a session's mutable fields (label and/or proxy). When
// the proxy changes and this worker holds the live client, the new proxy is
// applied on the running connection (TDLib addProxy/disableProxy) so the change
// takes effect without a restart; otherwise it applies on the next hydrate.
func (m *Manager) UpdateSession(ctx context.Context, id string, label, proxyID *string) error {
	found, err := m.st.UpdateSessionMeta(ctx, id, label, proxyID)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("session not found")
	}
	if proxyID != nil {
		if ls := m.get(id); ls != nil {
			if cl := ls.client(); cl != nil {
				if err := m.applyProxy(ctx, cl, *proxyID); err != nil {
					return fmt.Errorf("apply proxy: %w", err)
				}
				log.Printf("update %s: applied proxy change live", id)
			}
		}
	}
	return nil
}

// applyProxy enables the given proxy on a live client, or disables proxying when
// proxyID is empty. Built as raw td_api params so it doesn't depend on go-tdlib
// request marshaling.
func (m *Manager) applyProxy(ctx context.Context, cl *client.Client, proxyID string) error {
	if proxyID == "" {
		_, err := tdjson.Call(ctx, cl, "disableProxy", nil)
		return err
	}
	p, err := m.st.GetProxy(ctx, proxyID)
	if err != nil {
		return err
	}
	var sec string
	if len(p.SecretEnc) > 0 {
		b, err := m.sec.Decrypt(p.SecretEnc)
		if err != nil {
			return err
		}
		sec = string(b)
	}
	var pt map[string]any
	switch p.Type {
	case "socks5":
		pt = map[string]any{"@type": "proxyTypeSocks5", "username": p.Username, "password": sec}
	case "http":
		pt = map[string]any{"@type": "proxyTypeHttp", "username": p.Username, "password": sec}
	case "mtproto":
		pt = map[string]any{"@type": "proxyTypeMtproto", "secret": sec}
	default:
		return fmt.Errorf("unknown proxy type %q", p.Type)
	}
	params, err := json.Marshal(map[string]any{
		"server": p.Host, "port": p.Port, "enable": true, "type": pt,
	})
	if err != nil {
		return err
	}
	_, err = tdjson.Call(ctx, cl, "addProxy", params)
	return err
}

// DeleteSession permanently removes a session. If this worker holds the live
// client, it is closed first (releasing the binlog lock); then the registry row
// is dropped (cascading its webhook subscription and pending deliveries) and the
// on-disk directory is removed. The gateway routes deletes to the owning worker
// when one is alive, so the client closed here is the one actually holding it.
func (m *Manager) DeleteSession(ctx context.Context, id string) error {
	if ls := m.get(id); ls != nil {
		if cl := ls.client(); cl != nil {
			closeClient(ctx, cl)
		}
		m.delete(id)
	}
	m.clearRetry(id)
	dbDir, found, err := m.st.DeleteSession(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("session not found")
	}
	if dbDir != "" {
		if err := os.RemoveAll(dbDir); err != nil {
			log.Printf("delete %s: remove dir %s: %v", id, dbDir, err)
		}
	}
	log.Printf("deleted session %s", id)
	return nil
}

// Status returns the current status of a live session.
func (m *Manager) Status(id string) (string, error) {
	ls := m.get(id)
	if ls == nil {
		return "", errors.New("session not found")
	}
	return ls.status_(), nil
}

// waitLeave polls until the status differs from from, or timeout.
func waitLeave(ls *liveSession, from string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for {
		st := ls.status_()
		if st != from {
			return st
		}
		if time.Now().After(deadline) {
			return st
		}
		time.Sleep(100 * time.Millisecond)
	}
}
