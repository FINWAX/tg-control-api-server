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
	"github.com/FINWAX/tg-control-api-server/internal/upload"
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

	// Interactive login state. The client is stashed while the authorizer is
	// still driving the login (so a resend can reach it), and loggingIn keeps
	// everything that expects a usable account away from it until it is done.
	loggingIn bool
	codeInfo  json.RawMessage // authenticationCodeInfo of the code TDLib sent
	lastErr   string          // why the last attempt was rejected, for status reads
	attempt   chan error      // outcome of the value currently being checked
	cancel    chan struct{}   // closed to abort a login waiting for input
	canceled  bool
	loginDone chan struct{} // closed once the login goroutine has fully unwound

	// persist mirrors a status change into the registry, so a login waiting for
	// input is visible to every gateway and not only to this process.
	persist func(string)

	codeCh     chan string
	passwordCh chan string
}

// newLiveSession builds the live state for one account, wiring the status
// mirror to the registry. interactive marks a user login: only those wait for
// operator input and therefore need a cancel path and a completion signal.
func (m *Manager) newLiveSession(id, kind string, interactive bool) *liveSession {
	ls := &liveSession{
		id: id, kind: kind, status: "connecting",
		cancel:     make(chan struct{}),
		codeCh:     make(chan string, 1),
		passwordCh: make(chan string, 1),
		persist: func(st string) {
			if err := m.st.UpdateSessionStatus(context.Background(), id, st); err != nil {
				log.Printf("session %s: persist status %s: %v", id, st, err)
			}
		},
	}
	if interactive {
		ls.loginDone = make(chan struct{})
	}
	return ls
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
		// A send that referenced an uploaded file is done once TDLib confirms it:
		// drop the local upload immediately (success or failure), independent of
		// any webhook subscription.
		if ctor := result.GetConstructor(); ctor == "updateMessageSendSucceeded" || ctor == "updateMessageSendFailed" {
			m.finishUpload(result)
		}
		// A resend produces a new authenticationCodeInfo (different delivery
		// type, fresh resend timeout) only on the update stream — the request
		// itself answers with `ok`. Keep the session's copy current so the
		// operator is told where the new code actually went.
		if u, ok := result.(*client.UpdateAuthorizationState); ok {
			if w, ok := u.AuthorizationState.(*client.AuthorizationStateWaitCode); ok {
				ls.setCodeInfo(w.CodeInfo)
			}
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

// setStatus records a status change and mirrors it to the registry. The mirror
// is what makes an interrupted login recoverable: awaiting_code and
// awaiting_password used to live only in this process, so the session listing
// (served from Postgres) never showed that an operator's input was expected.
func (s *liveSession) setStatus(st string) {
	s.mu.Lock()
	if s.status == st {
		s.mu.Unlock()
		return
	}
	s.status = st
	p := s.persist
	s.mu.Unlock()
	if p != nil {
		p(st)
	}
}

func (s *liveSession) status_() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *liveSession) setClient(cl *client.Client) {
	s.mu.Lock()
	s.cl = cl
	s.loggingIn = false
	s.mu.Unlock()
	s.setStatus("authorized")
}

func (s *liveSession) client() *client.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cl
}

// readyClient returns the client only once the account is fully authorized. A
// login in flight already has a TDLib client (the authorizer stashes it so a
// resend can reach it), but it is not an account yet: calls, downloads,
// rebalancing and shutdown handover must all keep off it.
func (s *liveSession) readyClient() *client.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loggingIn || s.status != "authorized" {
		return nil
	}
	return s.cl
}

// beginLogin stashes the client TDLib is building while the login still waits
// for input, and marks the session as not yet usable.
func (s *liveSession) beginLogin(cl *client.Client) {
	s.mu.Lock()
	s.cl = cl
	s.loggingIn = true
	s.mu.Unlock()
}

// failLogin marks a login that never completed. go-tdlib closes the client it
// was building when the authorizer gives up, so the stashed reference dies with
// it and must not be closed again.
func (s *liveSession) failLogin(err error) {
	s.mu.Lock()
	s.err = err
	s.cl = nil
	s.loggingIn = false
	if err != nil {
		s.lastErr = err.Error()
	}
	authorized := s.status == "authorized"
	s.mu.Unlock()
	if !authorized {
		s.setStatus("error")
	}
}

// armAttempt registers a one-shot channel for the outcome of the code or
// password about to be submitted, so the caller learns whether Telegram
// accepted it instead of only seeing whichever status happens to follow.
func (s *liveSession) armAttempt() chan error {
	ch := make(chan error, 1)
	s.mu.Lock()
	s.attempt = ch
	s.lastErr = ""
	s.mu.Unlock()
	return ch
}

// reportAttempt publishes the verdict on a checked code or password.
func (s *liveSession) reportAttempt(err error) {
	s.mu.Lock()
	ch := s.attempt
	s.attempt = nil
	if err != nil {
		s.lastErr = err.Error()
	}
	s.mu.Unlock()
	if ch != nil {
		ch <- err // buffered: never blocks the authorizer
	}
}

// setLastErr records why a login failed terminally, so the status read can say
// more than "error" after the client is gone.
func (s *liveSession) setLastErr(err error) {
	s.mu.Lock()
	s.lastErr = err.Error()
	s.mu.Unlock()
}

func (s *liveSession) setCodeInfo(info *client.AuthenticationCodeInfo) {
	if info == nil {
		return
	}
	b, err := json.Marshal(info)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.codeInfo = b
	s.mu.Unlock()
}

// abortLogin releases a login that is blocked waiting for a code or password.
// It reports whether one was actually in flight: if so the authorizer unwinds
// and closes the client itself, so the caller must not close it a second time.
func (s *liveSession) abortLogin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loginDone == nil || s.canceled {
		return false
	}
	select {
	case <-s.loginDone:
		return false // already finished on its own
	default:
	}
	s.canceled = true
	close(s.cancel)
	return true
}

// awaitLogin waits for an aborted login goroutine to unwind, bounded, so the
// caller can remove the session directory without racing a closing client.
func (s *liveSession) awaitLogin(d time.Duration) {
	s.mu.Lock()
	done := s.loginDone
	s.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(d):
	}
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

	uploadsDir string        // shared uploads volume; inputFileLocal is confined here
	uploads    *upload.Store // same volume, used only for the TTL sweep
	uploadTTL  time.Duration // abandoned/sent uploads older than this are swept

	stopping atomic.Bool // set on graceful shutdown; heartbeat/reconciler stand down

	mu    sync.RWMutex
	live  map[string]*liveSession
	retry map[string]time.Time // session id -> earliest next open attempt (backoff)

	pendMu  sync.Mutex
	pending map[int64][]string // temp message id -> upload dirs to delete once sent
}

// worker coordination cadence.
const (
	workerHeartbeat   = 10 * time.Second // how often we refresh our liveness
	workerStale       = 30 * time.Second // an owner past this is considered dead
	reconcileInterval = 3 * time.Second  // how often we reconcile ownership
	claimBatch        = 8                // max sessions claimed per reconcile
	shedPerTick       = 2                // max sessions shed per reconcile
	openBackoff       = 20 * time.Second // wait this long before retrying a failed open
	staleLoginGrace   = 2 * time.Minute  // grace before an ownerless login is written off
)

// flood-wait auto-retry bounds: TDLib FLOOD_WAIT (code 420) asks the caller to
// wait N seconds. Short waits are absorbed transparently; longer ones surface as
// a structured error so the client decides.
const (
	maxAutoFloodWaitSec = 5 // only auto-retry flood-waits up to this many seconds
	maxFloodRetries     = 2 // max transparent flood-wait retries per call
)

func NewManager(st *store.Store, sec *secret.Box, dataDir, workerID, selfAddr string, capacity int, uploadsDir string, uploadTTL time.Duration) *Manager {
	m := &Manager{
		st: st, sec: sec, dataDir: dataDir,
		workerID: workerID, selfAddr: selfAddr, capacity: capacity,
		uploadsDir: uploadsDir,
		uploads:    upload.New(uploadsDir, 0, 0, 0), // sizes irrelevant: workers only sweep
		uploadTTL:  uploadTTL,
		outbox:     make(chan outItem, 1024),
		live:       map[string]*liveSession{},
		retry:      map[string]time.Time{},
		pending:    map[int64][]string{},
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

	// (0) Reap abandoned logins. A flow waiting for a code lives in the owning
	// worker's memory, so once that worker is gone nothing can ever deliver the
	// code: the row must stop advertising that input is expected, or the
	// console would offer a "log in" that leads nowhere.
	if n, err := m.st.ExpireStaleLogins(ctx, cutoff, staleLoginGrace); err != nil {
		log.Printf("reconcile: expire stale logins: %v", err)
	} else if n > 0 {
		log.Printf("expired %d abandoned login(s)", n)
	}

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
			if ls := m.get(id); ls != nil && ls.readyClient() != nil {
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
// reclaim them: close the client (release binlog) then clear ownership. A login
// still waiting for input is never shed — no peer could take it over (the flow
// lives in this process) and handing it away would cost the operator the code.
func (m *Manager) shed(ctx context.Context, n int) {
	m.mu.RLock()
	ids := make([]string, 0, n)
	for id, ls := range m.live {
		if ls.readyClient() == nil {
			continue
		}
		ids = append(ids, id)
		if len(ids) == n {
			break
		}
	}
	m.mu.RUnlock()
	for _, id := range ids {
		if ls := m.get(id); ls != nil {
			if cl := ls.readyClient(); cl != nil {
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
	// Logins still waiting for input are released rather than closed: the
	// authorizer owns those clients and closes them as it unwinds.
	m.mu.RLock()
	clients := make([]*client.Client, 0, len(m.live))
	pending := make([]*liveSession, 0)
	for _, ls := range m.live {
		if cl := ls.readyClient(); cl != nil {
			clients = append(clients, cl)
		} else {
			pending = append(pending, ls)
		}
	}
	m.mu.RUnlock()
	for _, ls := range pending {
		if ls.abortLogin() {
			ls.awaitLogin(loginAbortWait)
		}
	}
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

	ls := m.newLiveSession(r.ID, r.Kind, false)
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

	ls := m.newLiveSession(id, "bot", false)
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
			ls.failLogin(e)
			return id, nil, e
		}
	case <-time.After(45 * time.Second):
		ls.failLogin(errors.New("bot login timeout"))
		return id, nil, errors.New("bot login timeout")
	}

	me, err = tdjson.Call(ctx, ls.client(), "getMe", nil)
	return id, me, err
}

// ErrLoginPending reports that a login for the same phone is already waiting for
// input. It carries that session's id so the caller can resume the attempt
// instead of asking Telegram for a second code.
type ErrLoginPending struct{ SessionID string }

func (e *ErrLoginPending) Error() string {
	return "a login for this phone is already waiting for input (session " + e.SessionID + ")"
}

// loginWait bounds how long a submit waits for Telegram before answering with
// whatever state the login is in; the caller can always read the status again.
const loginWait = 20 * time.Second

// loginAbortWait bounds how long a delete waits for an aborted login to unwind
// before removing its directory.
const loginAbortWait = 10 * time.Second

// CreateUser starts a user login (phone set automatically) and returns as soon
// as the flow reaches a waiting state. The code/password arrive later via
// SubmitCode/SubmitPassword.
//
// A phone that already has a login waiting for input is refused: every create
// costs a Telegram code send, and the usual cause is a repeated submit rather
// than a genuine second account. An authorized number is not refused — one
// number may legitimately hold several sessions.
func (m *Manager) CreateUser(ctx context.Context, appID, phone, proxyID, label string) (id string, st SessionState, err error) {
	if pending, found, e := m.st.PendingUserLogin(ctx, phone); e != nil {
		return "", st, e
	} else if found {
		return "", st, &ErrLoginPending{SessionID: pending}
	}
	app, err := m.st.GetApp(ctx, appID)
	if err != nil {
		return "", st, fmt.Errorf("app: %w", err)
	}
	apiHash, err := m.sec.Decrypt(app.APIHashEnc)
	if err != nil {
		return "", st, err
	}
	proxyReq, err := m.proxyRequest(ctx, proxyID)
	if err != nil {
		return "", st, fmt.Errorf("proxy: %w", err)
	}
	dbKey := make([]byte, 32)
	if _, err = rand.Read(dbKey); err != nil {
		return "", st, err
	}
	dbKeyEnc, err := m.sec.Encrypt(dbKey)
	if err != nil {
		return "", st, err
	}
	id, err = m.st.CreateSession(ctx, store.NewSession{
		Kind: "user", AppID: appID, ProxyID: proxyID, Phone: phone, DBKeyEnc: dbKeyEnc, Label: label,
	})
	if err != nil {
		return "", st, err
	}
	dbDir := filepath.Join(m.dataDir, id)
	_ = m.st.SetSessionDBDir(ctx, id, dbDir)
	_ = m.st.SetSessionOwner(ctx, id, m.workerID)

	ls := m.newLiveSession(id, "user", true)
	m.put(ls)

	h := &authHandler{
		params: m.buildParams(dbDir, dbKey, app.APIID, string(apiHash)),
		proxy:  proxyReq,
		phone:  phone,
		ls:     ls,
	}

	go func() {
		defer close(ls.loginDone)
		cl, e := client.NewClient(h, client.WithResultHandler(m.updateHandler(ls)))
		if e != nil {
			ls.failLogin(e)
			return
		}
		ls.setClient(cl)
	}()

	waitLeave(ls, "connecting", loginWait)
	return id, ls.state(), nil
}

// SubmitCode delivers the login code and reports the resulting state.
func (m *Manager) SubmitCode(id, code string) (SessionState, error) {
	return m.submit(id, "awaiting_code", code, func(ls *liveSession) chan string { return ls.codeCh })
}

// SubmitPassword delivers the 2FA password and reports the resulting state.
func (m *Manager) SubmitPassword(id, password string) (SessionState, error) {
	return m.submit(id, "awaiting_password", password, func(ls *liveSession) chan string { return ls.passwordCh })
}

// submit hands a code or password to the waiting authorizer and reports what
// Telegram made of it. A rejection is returned as its own error while the login
// stays in the same waiting state, so the operator can correct a typo without
// spending a new code — the whole point of the exercise.
func (m *Manager) submit(id, want, value string, pick func(*liveSession) chan string) (SessionState, error) {
	ls := m.get(id)
	if ls == nil {
		return SessionState{}, errors.New("session not found")
	}
	if st := ls.status_(); st != want {
		return ls.state(), fmt.Errorf("session is %s, not %s", st, want)
	}
	res := ls.armAttempt()
	select {
	case pick(ls) <- value:
	default:
		return ls.state(), errors.New("login is not accepting input right now")
	}
	select {
	case err := <-res:
		if err != nil {
			return ls.state(), err
		}
	case <-time.After(loginWait):
		return ls.state(), errors.New("timed out waiting for Telegram")
	}
	waitLeave(ls, want, loginWait)
	return ls.state(), nil
}

// ResendCode asks Telegram to send the login code again on the attempt already
// in flight. It is the only way to recover a code that never arrived without
// throwing the login away and paying for a fresh one. Telegram refuses before
// code_info.timeout has elapsed, and that refusal is surfaced as-is.
func (m *Manager) ResendCode(ctx context.Context, id string) (SessionState, error) {
	ls := m.get(id)
	if ls == nil {
		return SessionState{}, errors.New("session not found")
	}
	if st := ls.status_(); st != "awaiting_code" {
		return ls.state(), fmt.Errorf("session is %s, not awaiting_code", st)
	}
	cl := ls.client()
	if cl == nil {
		return ls.state(), errors.New("login is not ready for a resend")
	}
	if _, err := tdjson.Call(ctx, cl, "resendAuthenticationCode", nil); err != nil {
		ls.setLastErr(err)
		return ls.state(), err
	}
	return ls.state(), nil
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

// Call runs a dynamic td_api method on an authorized session. Two conveniences
// wrap the raw dispatch: (1) a string chat_id ("@username") is resolved to its
// numeric id via searchPublicChat, so callers can address public peers without a
// manual resolve/access_hash step; (2) a "Chat not found" force-loads the chat
// and retries once, since TDLib lazy-loads dialogs — for every peer type, not
// just private chats (see forceLoadChat), so callers need not know the chat_id
// encoding. Short FLOOD_WAIT errors are retried transparently; longer ones
// surface as a structured error.
func (m *Manager) Call(ctx context.Context, id, method string, params json.RawMessage) (json.RawMessage, error) {
	ls := m.get(id)
	if ls == nil {
		return nil, errors.New("session not found")
	}
	cl := ls.readyClient()
	if cl == nil {
		return nil, fmt.Errorf("session is %s, not authorized", ls.status_())
	}

	// Confine any inputFileLocal to the uploads volume, and note which upload
	// dirs this call references so they can be dropped once the send completes.
	uploadDirs, err := m.guardLocalPaths(params)
	if err != nil {
		return nil, err
	}

	params, err = m.resolveChatUsername(ctx, cl, params)
	if err != nil {
		return nil, err
	}

	res, err := m.sendWithFloodRetry(ctx, cl, method, params)
	if err != nil && isChatNotFound(err) {
		if loadMethod, loadParams, ok := forceLoadChat(params); ok {
			if _, e := tdjson.Call(ctx, cl, loadMethod, loadParams); e == nil {
				log.Printf("call %s: force-loaded chat via %s, retrying %s", id, loadMethod, method)
				res, err = m.sendWithFloodRetry(ctx, cl, method, params)
			}
		}
	}
	if err == nil && len(uploadDirs) > 0 {
		m.trackUpload(res, uploadDirs)
	}
	return res, err
}

// sendWithFloodRetry dispatches a method, transparently retrying short
// FLOOD_WAIT (code 420) responses after the wait TDLib requests.
func (m *Manager) sendWithFloodRetry(ctx context.Context, cl *client.Client, method string, params json.RawMessage) (json.RawMessage, error) {
	for attempt := 0; ; attempt++ {
		res, err := tdjson.Call(ctx, cl, method, params)
		if err == nil {
			return res, nil
		}
		secs, ok := floodWaitSeconds(err)
		if !ok || secs > maxAutoFloodWaitSec || attempt >= maxFloodRetries {
			return res, err
		}
		log.Printf("call %s: FLOOD_WAIT %ds, retrying (attempt %d)", method, secs, attempt+1)
		select {
		case <-time.After(time.Duration(secs) * time.Second):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// floodWaitSeconds returns the retry-after seconds of a TDLib FLOOD_WAIT error
// ("Too Many Requests: retry after N"), or ok=false for anything else.
func floodWaitSeconds(err error) (int, bool) {
	var te *tdjson.Error
	if !errors.As(err, &te) {
		return 0, false
	}
	return te.RetryAfter()
}

// resolveChatUsername rewrites a string chat_id ("@name" or "name") to the
// numeric id of the resolved public chat. A numeric chat_id passes through
// untouched (no extra call).
func (m *Manager) resolveChatUsername(ctx context.Context, cl *client.Client, params json.RawMessage) (json.RawMessage, error) {
	uname, ok := stringChatID(params)
	if !ok {
		return params, nil
	}
	uname = strings.TrimPrefix(strings.TrimSpace(uname), "@")
	if uname == "" {
		return params, nil
	}
	q, _ := json.Marshal(map[string]any{"username": uname})
	res, err := tdjson.Call(ctx, cl, "searchPublicChat", q)
	if err != nil {
		return nil, fmt.Errorf("resolve @%s: %w", uname, err)
	}
	var chat struct {
		ID int64 `json:"id"`
	}
	if json.Unmarshal(res, &chat) != nil || chat.ID == 0 {
		return nil, fmt.Errorf("resolve @%s: no chat id", uname)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(params, &obj); err != nil {
		return nil, err
	}
	obj["chat_id"], _ = json.Marshal(chat.ID)
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	log.Printf("call: resolved @%s -> chat %d", uname, chat.ID)
	return out, nil
}

// stringChatID returns the chat_id when it is a JSON string (a username), or
// ok=false when it is numeric or absent.
func stringChatID(params json.RawMessage) (string, bool) {
	if len(params) == 0 {
		return "", false
	}
	var obj struct {
		ChatID json.RawMessage `json:"chat_id"`
	}
	if json.Unmarshal(params, &obj) != nil || len(obj.ChatID) == 0 || obj.ChatID[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(obj.ChatID, &s) != nil {
		return "", false
	}
	return s, true
}

func isChatNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Chat not found")
}

// numericChatID extracts a numeric chat_id from params, or ok=false when it is
// absent, zero, or a username string (handled by resolveChatUsername).
func numericChatID(params json.RawMessage) (int64, bool) {
	if len(params) == 0 {
		return 0, false
	}
	var p struct {
		ChatID int64 `json:"chat_id"`
	}
	if json.Unmarshal(params, &p) != nil || p.ChatID == 0 {
		return 0, false
	}
	return p.ChatID, true
}

// TDLib packs the peer type into chat_id itself (td::DialogId), the same
// encoding the Bot API exposes: a positive id is a user, a small negative id is
// a basic group, and anything at or below chanBase is a supergroup/channel.
// Secret chats live below secretBase and are not used here (UseSecretChats is
// off), so they are left unresolved rather than misread as channels.
const (
	chanBase   = -1000000000000
	secretBase = -2000000000000
)

// forceLoadChat returns the td_api call that makes TDLib materialize a chat it
// knows of but has not loaded as a dialog yet. TDLib lazy-loads dialogs, so a
// perfectly valid chat_id can answer "Chat not found" until something creates
// the chat object; each peer type has its own constructor for that. Returns
// ok=false for a chat_id whose type cannot be decoded.
//
// This only works when the account already holds the peer's access_hash (it is
// a member, or has seen the chat). A never-seen private channel cannot be
// resolved from its id alone — that is a Telegram protocol constraint; address
// it by @username instead.
func forceLoadChat(params json.RawMessage) (method string, q json.RawMessage, ok bool) {
	id, ok := numericChatID(params)
	if !ok {
		return "", nil, false
	}
	var arg map[string]any
	switch {
	case id > 0:
		// force: build from cached user info without a network round-trip.
		method, arg = "createPrivateChat", map[string]any{"user_id": id, "force": true}
	case id <= secretBase:
		return "", nil, false
	case id <= chanBase:
		method, arg = "createSupergroupChat", map[string]any{"supergroup_id": chanBase - id}
	default:
		method, arg = "createBasicGroupChat", map[string]any{"basic_group_id": -id}
	}
	b, err := json.Marshal(arg)
	if err != nil {
		return "", nil, false
	}
	return method, b, true
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
		// A login still waiting for a code owns the client. Release it and let
		// the authorizer close it as it unwinds — closing it here as well would
		// be a double close, and removing the directory before it has unwound
		// would pull the disk out from under a live instance.
		if ls.abortLogin() {
			ls.awaitLogin(loginAbortWait)
		}
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

// SessionState is what a status read reports. Beyond the status it carries the
// login context an operator needs to act: where Telegram sent the code (and how
// long until it may be resent), and why the last attempt was refused.
type SessionState struct {
	Status    string          `json:"status"`
	CodeInfo  json.RawMessage `json:"code_info,omitempty"`  // td_api authenticationCodeInfo
	LastError string          `json:"last_error,omitempty"` // last refusal, verbatim
}

func (s *liveSession) state() SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := SessionState{Status: s.status}
	switch s.status {
	case "awaiting_code":
		st.CodeInfo = s.codeInfo
		st.LastError = s.lastErr
	case "authorized":
		// Nothing is pending and nothing failed: report a clean state rather
		// than the refusal that preceded the accepted attempt.
	default:
		st.LastError = s.lastErr
	}
	return st
}

// State returns the live state of a session held by this worker.
func (m *Manager) State(id string) (SessionState, error) {
	ls := m.get(id)
	if ls == nil {
		return SessionState{}, errors.New("session not found")
	}
	return ls.state(), nil
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
