// Package store is the Postgres registry: apps, proxies, and sessions. It is
// the source of truth for creds (encrypted) and session metadata.
package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

// schemaLockKey serializes concurrent migration. With a gateway and several
// workers all starting at once, unguarded DDL can collide on catalog inserts
// (duplicate pg_type). A transaction-scoped advisory lock makes the first
// starter run pending migrations while the rest wait, then see none pending.
const schemaLockKey = 0x7467617069 // "tgapi"

// New connects, verifies, and brings the schema up to date by running any
// pending migrations (serialized across processes; see applyMigrations).
func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		pool.Close()
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(schemaLockKey)); err != nil {
		pool.Close()
		return nil, err
	}
	if err := applyMigrations(ctx, tx); err != nil {
		pool.Close()
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

type App struct {
	ID         string
	APIID      int32
	APIHashEnc []byte
	Label      string
}

type Proxy struct {
	ID        string
	Type      string
	Host      string
	Port      int32
	Username  string
	SecretEnc []byte
	Label     string
}

type NewSession struct {
	Kind        string
	AppID       string
	ProxyID     string // "" -> NULL
	Phone       string // "" -> NULL
	BotTokenEnc []byte
	DBKeyEnc    []byte
	Label       string
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *Store) CreateApp(ctx context.Context, apiID int32, apiHashEnc []byte, label string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO tg_app (api_id, api_hash_enc, label) VALUES ($1,$2,$3) RETURNING id`,
		apiID, apiHashEnc, nullStr(label)).Scan(&id)
	return id, err
}

func (s *Store) GetApp(ctx context.Context, id string) (*App, error) {
	a := &App{}
	var label *string
	err := s.pool.QueryRow(ctx,
		`SELECT id, api_id, api_hash_enc, label FROM tg_app WHERE id=$1`, id).
		Scan(&a.ID, &a.APIID, &a.APIHashEnc, &label)
	if err != nil {
		return nil, err
	}
	if label != nil {
		a.Label = *label
	}
	return a, nil
}

func (s *Store) CreateProxy(ctx context.Context, p Proxy) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO proxy (type, host, port, username, secret_enc, label)
		 VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		p.Type, p.Host, p.Port, nullStr(p.Username), p.SecretEnc, nullStr(p.Label)).Scan(&id)
	return id, err
}

func (s *Store) GetProxy(ctx context.Context, id string) (*Proxy, error) {
	p := &Proxy{}
	var username *string
	err := s.pool.QueryRow(ctx,
		`SELECT id, type, host, port, username, secret_enc FROM proxy WHERE id=$1`, id).
		Scan(&p.ID, &p.Type, &p.Host, &p.Port, &username, &p.SecretEnc)
	if err != nil {
		return nil, err
	}
	if username != nil {
		p.Username = *username
	}
	return p, nil
}

func (s *Store) CreateSession(ctx context.Context, n NewSession) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO session (kind, app_id, proxy_id, phone, bot_token_enc, db_key_enc, label, status)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,'connecting') RETURNING id`,
		n.Kind, n.AppID, nullStr(n.ProxyID), nullStr(n.Phone), n.BotTokenEnc, n.DBKeyEnc, nullStr(n.Label)).
		Scan(&id)
	return id, err
}

// Rehydratable is the subset of a session needed to restore its live client
// from the on-disk binlog after a restart.
type Rehydratable struct {
	ID       string
	Kind     string
	AppID    string
	ProxyID  string
	DBDir    string
	DBKeyEnc []byte
}

func (s *Store) SetSessionDBDir(ctx context.Context, id, dbDir string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE session SET db_dir=$2, updated_at=now() WHERE id=$1`, id, dbDir)
	return err
}

func (s *Store) UpdateSessionStatus(ctx context.Context, id, status string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE session SET status=$2, updated_at=now() WHERE id=$1`, id, status)
	return err
}

// --- management reads (registry listings for the UI / ops) ---
// These never expose secrets: api_hash, bot tokens, proxy secrets, and db keys
// are omitted by construction.

type AppInfo struct {
	ID        string    `json:"id"`
	APIID     int32     `json:"api_id"`
	Label     string    `json:"label,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Store) ListApps(ctx context.Context) ([]AppInfo, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, api_id, COALESCE(label,''), created_at FROM tg_app ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AppInfo{}
	for rows.Next() {
		var a AppInfo
		if err := rows.Scan(&a.ID, &a.APIID, &a.Label, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

type ProxyInfo struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Host      string    `json:"host"`
	Port      int32     `json:"port"`
	Username  string    `json:"username,omitempty"`
	Label     string    `json:"label,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Store) ListProxies(ctx context.Context) ([]ProxyInfo, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, type, host, port, COALESCE(username,''), COALESCE(label,''), created_at
		 FROM proxy ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProxyInfo{}
	for rows.Next() {
		var p ProxyInfo
		if err := rows.Scan(&p.ID, &p.Type, &p.Host, &p.Port, &p.Username, &p.Label, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SessionInfo is a registry row enriched with its app label and a human proxy
// string, so the UI can render sessions without extra lookups.
type SessionInfo struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Status     string     `json:"status"`
	Phone      string     `json:"phone,omitempty"`
	Label      string     `json:"label,omitempty"`
	WorkerID   string     `json:"worker_id,omitempty"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	AppID      string     `json:"app_id"`
	AppLabel   string     `json:"app_label,omitempty"`
	ProxyID    string     `json:"proxy_id,omitempty"`
	Proxy      string     `json:"proxy,omitempty"` // "socks5 host:port"
}

// sessionListSQL is the shared projection behind the session listings; callers
// append their own WHERE and ORDER BY.
const sessionListSQL = `SELECT s.id, s.kind, s.status, COALESCE(s.phone,''), COALESCE(s.label,''),
	        COALESCE(s.worker_id,''), s.last_seen_at, s.created_at,
	        s.app_id::text, COALESCE(a.label,''),
	        COALESCE(s.proxy_id::text,''), p.type, p.host, p.port
	 FROM session s
	 LEFT JOIN tg_app a ON a.id = s.app_id
	 LEFT JOIN proxy  p ON p.id = s.proxy_id`

func (s *Store) ListSessions(ctx context.Context) ([]SessionInfo, error) {
	return s.querySessions(ctx, sessionListSQL+` ORDER BY s.created_at`)
}

// ListSessionsForToken returns only the sessions a scoped token's grants cover,
// so its holder can discover the full ids (and kinds) it may address without
// needing the master token. The predicate mirrors TokenGrantsSession — keep the
// two in step.
func (s *Store) ListSessionsForToken(ctx context.Context, tokenID string) ([]SessionInfo, error) {
	return s.querySessions(ctx, sessionListSQL+`
	 WHERE EXISTS(
	   SELECT 1 FROM api_token t
	   WHERE t.id=$1 AND t.enabled AND (
	     t.all_sessions
	     OR EXISTS(SELECT 1 FROM api_token_session ts WHERE ts.token_id=t.id AND ts.session_id=s.id)
	     OR EXISTS(SELECT 1 FROM api_token_app ta WHERE ta.token_id=t.id AND ta.app_id=s.app_id)
	   )
	 )
	 ORDER BY s.created_at`, tokenID)
}

func (s *Store) querySessions(ctx context.Context, sql string, args ...any) ([]SessionInfo, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionInfo{}
	for rows.Next() {
		var si SessionInfo
		var ptype, phost *string
		var pport *int32
		if err := rows.Scan(&si.ID, &si.Kind, &si.Status, &si.Phone, &si.Label,
			&si.WorkerID, &si.LastSeenAt, &si.CreatedAt,
			&si.AppID, &si.AppLabel,
			&si.ProxyID, &ptype, &phost, &pport); err != nil {
			return nil, err
		}
		if ptype != nil && phost != nil && pport != nil {
			si.Proxy = *ptype + " " + *phost + ":" + strconv.Itoa(int(*pport))
		}
		out = append(out, si)
	}
	return out, rows.Err()
}

type WorkerInfo struct {
	ID         string    `json:"id"`
	Addr       string    `json:"addr"`
	LastSeenAt time.Time `json:"last_seen_at"`
	Alive      bool      `json:"alive"`
	Sessions   int       `json:"sessions"` // authorized sessions it owns
}

func (s *Store) ListWorkers(ctx context.Context, aliveCutoff time.Time) ([]WorkerInfo, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT w.id, w.addr, w.last_seen_at, (w.last_seen_at > $1) AS alive,
		        (SELECT count(*) FROM session s WHERE s.worker_id=w.id AND s.status='authorized')
		 FROM worker w ORDER BY w.id`, aliveCutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkerInfo{}
	for rows.Next() {
		var wi WorkerInfo
		if err := rows.Scan(&wi.ID, &wi.Addr, &wi.LastSeenAt, &wi.Alive, &wi.Sessions); err != nil {
			return nil, err
		}
		out = append(out, wi)
	}
	return out, rows.Err()
}

// UpdateSessionMeta updates a session's mutable fields. Only non-nil fields are
// written; an empty *proxyID ("") clears the proxy (NULL). Returns whether a row
// matched (false = no such session).
func (s *Store) UpdateSessionMeta(ctx context.Context, id string, label, proxyID *string) (bool, error) {
	sets := []string{"updated_at=now()"}
	args := []any{id}
	n := 2
	if label != nil {
		sets = append(sets, fmt.Sprintf("label=$%d", n))
		args = append(args, nullStr(*label))
		n++
	}
	if proxyID != nil {
		sets = append(sets, fmt.Sprintf("proxy_id=$%d", n))
		args = append(args, nullStr(*proxyID))
		n++
	}
	if len(sets) == 1 {
		return true, nil // nothing to change
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE session SET `+strings.Join(sets, ", ")+` WHERE id=$1`, args...)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// UpdateAppLabel renames an app (api_id/api_hash are immutable).
func (s *Store) UpdateAppLabel(ctx context.Context, id, label string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE tg_app SET label=$2 WHERE id=$1`, id, nullStr(label))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// UpdateProxyLabel renames a proxy.
func (s *Store) UpdateProxyLabel(ctx context.Context, id, label string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE proxy SET label=$2 WHERE id=$1`, id, nullStr(label))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// --- scoped API tokens ---

// TokenInfo is a scoped token without its secret (never returned after creation).
type TokenInfo struct {
	ID          string    `json:"id"`
	Name        string    `json:"name,omitempty"`
	Enabled     bool      `json:"enabled"`
	AllSessions bool      `json:"all_sessions"`
	AppIDs      []string  `json:"app_ids"`
	SessionIDs  []string  `json:"session_ids"`
	CreatedAt   time.Time `json:"created_at"`
}

// CreateToken inserts a scoped token and its app/session grants atomically.
func (s *Store) CreateToken(ctx context.Context, name string, secretHash []byte, enabled, allSessions bool, appIDs, sessionIDs []string) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var id string
	if err := tx.QueryRow(ctx,
		`INSERT INTO api_token (name, secret_hash, enabled, all_sessions)
		 VALUES ($1,$2,$3,$4) RETURNING id`,
		nullStr(name), secretHash, enabled, allSessions).Scan(&id); err != nil {
		return "", err
	}
	if err := replaceTokenGrants(ctx, tx, id, appIDs, sessionIDs); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return id, nil
}

// replaceTokenGrants clears and re-inserts a token's app/session grant rows.
func replaceTokenGrants(ctx context.Context, tx pgx.Tx, tokenID string, appIDs, sessionIDs []string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM api_token_app WHERE token_id=$1`, tokenID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM api_token_session WHERE token_id=$1`, tokenID); err != nil {
		return err
	}
	for _, a := range appIDs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO api_token_app (token_id, app_id) VALUES ($1,$2)
			 ON CONFLICT DO NOTHING`, tokenID, a); err != nil {
			return err
		}
	}
	for _, sid := range sessionIDs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO api_token_session (token_id, session_id) VALUES ($1,$2)
			 ON CONFLICT DO NOTHING`, tokenID, sid); err != nil {
			return err
		}
	}
	return nil
}

// ListTokens returns all scoped tokens with their grants (no secrets).
func (s *Store) ListTokens(ctx context.Context) ([]TokenInfo, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT t.id, COALESCE(t.name,''), t.enabled, t.all_sessions, t.created_at,
		        COALESCE((SELECT array_agg(app_id::text) FROM api_token_app WHERE token_id=t.id), ARRAY[]::text[]),
		        COALESCE((SELECT array_agg(session_id::text) FROM api_token_session WHERE token_id=t.id), ARRAY[]::text[])
		 FROM api_token t ORDER BY t.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TokenInfo{}
	for rows.Next() {
		var t TokenInfo
		if err := rows.Scan(&t.ID, &t.Name, &t.Enabled, &t.AllSessions, &t.CreatedAt, &t.AppIDs, &t.SessionIDs); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TokenPatch carries the fields to change on a token; nil fields are left as-is.
type TokenPatch struct {
	Name        *string
	Enabled     *bool
	AllSessions *bool
	AppIDs      *[]string
	SessionIDs  *[]string
}

// UpdateToken applies a partial update. Grant lists, when provided, replace the
// existing sets. Returns whether the token existed.
func (s *Store) UpdateToken(ctx context.Context, id string, p TokenPatch) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	sets := []string{}
	args := []any{id}
	n := 2
	if p.Name != nil {
		sets = append(sets, fmt.Sprintf("name=$%d", n))
		args = append(args, nullStr(*p.Name))
		n++
	}
	if p.Enabled != nil {
		sets = append(sets, fmt.Sprintf("enabled=$%d", n))
		args = append(args, *p.Enabled)
		n++
	}
	if p.AllSessions != nil {
		sets = append(sets, fmt.Sprintf("all_sessions=$%d", n))
		args = append(args, *p.AllSessions)
		n++
	}
	exists := true
	if len(sets) > 0 {
		tag, err := tx.Exec(ctx, `UPDATE api_token SET `+strings.Join(sets, ", ")+` WHERE id=$1`, args...)
		if err != nil {
			return false, err
		}
		exists = tag.RowsAffected() > 0
	} else {
		if err := tx.QueryRow(ctx, `SELECT true FROM api_token WHERE id=$1`, id).Scan(&exists); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			return false, err
		}
	}
	if !exists {
		return false, nil
	}
	if p.AppIDs != nil || p.SessionIDs != nil {
		apps := []string{}
		if p.AppIDs != nil {
			apps = *p.AppIDs
		}
		sess := []string{}
		if p.SessionIDs != nil {
			sess = *p.SessionIDs
		}
		// Only touch a list that was provided; fetch the other from current state.
		if p.AppIDs == nil {
			apps = nil // sentinel: leave apps untouched below
		}
		if p.SessionIDs == nil {
			sess = nil
		}
		if err := patchTokenGrants(ctx, tx, id, apps, sess, p.AppIDs != nil, p.SessionIDs != nil); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// patchTokenGrants replaces only the grant lists that were explicitly provided.
func patchTokenGrants(ctx context.Context, tx pgx.Tx, tokenID string, appIDs, sessionIDs []string, setApps, setSessions bool) error {
	if setApps {
		if _, err := tx.Exec(ctx, `DELETE FROM api_token_app WHERE token_id=$1`, tokenID); err != nil {
			return err
		}
		for _, a := range appIDs {
			if _, err := tx.Exec(ctx,
				`INSERT INTO api_token_app (token_id, app_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
				tokenID, a); err != nil {
				return err
			}
		}
	}
	if setSessions {
		if _, err := tx.Exec(ctx, `DELETE FROM api_token_session WHERE token_id=$1`, tokenID); err != nil {
			return err
		}
		for _, sid := range sessionIDs {
			if _, err := tx.Exec(ctx,
				`INSERT INTO api_token_session (token_id, session_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
				tokenID, sid); err != nil {
				return err
			}
		}
	}
	return nil
}

// DeleteToken removes a token and its grants (cascade). Returns whether it existed.
func (s *Store) DeleteToken(ctx context.Context, id string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM api_token WHERE id=$1`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ResolveToken looks up a token by its secret hash. found is false when unknown.
func (s *Store) ResolveToken(ctx context.Context, secretHash []byte) (id string, enabled, found bool, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT id, enabled FROM api_token WHERE secret_hash=$1`, secretHash).Scan(&id, &enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, false, nil
	}
	if err != nil {
		return "", false, false, err
	}
	return id, enabled, true, nil
}

// TokenGrantsSession reports whether an enabled token's scope covers a session:
// all_sessions, an explicit session grant, or the session's app being granted.
func (s *Store) TokenGrantsSession(ctx context.Context, tokenID, sessionID string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(
		   SELECT 1 FROM api_token t
		   WHERE t.id=$1 AND t.enabled AND (
		     t.all_sessions
		     OR EXISTS(SELECT 1 FROM api_token_session ts WHERE ts.token_id=t.id AND ts.session_id=$2)
		     OR EXISTS(SELECT 1 FROM api_token_app ta JOIN session s ON s.id=$2
		               WHERE ta.token_id=t.id AND ta.app_id=s.app_id)
		   )
		 )`, tokenID, sessionID).Scan(&ok)
	return ok, err
}

// DeleteSession removes a session row and returns its on-disk db_dir so the
// caller can delete the directory. The delete cascades to the session's webhook
// subscription and queued deliveries (ON DELETE CASCADE). found is false when no
// such session exists.
func (s *Store) DeleteSession(ctx context.Context, id string) (dbDir string, found bool, err error) {
	err = s.pool.QueryRow(ctx,
		`DELETE FROM session WHERE id=$1 RETURNING COALESCE(db_dir,'')`, id).Scan(&dbDir)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return dbDir, true, nil
}

// --- worker coordination (Postgres-based ownership lease) ---

// RegisterWorker upserts a worker's advertised address and refreshes its
// heartbeat. Called at startup and periodically; a worker is considered alive
// while last_seen_at stays within the caller's staleness window.
func (s *Store) RegisterWorker(ctx context.Context, id, addr string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO worker (id, addr, last_seen_at) VALUES ($1,$2,now())
		 ON CONFLICT (id) DO UPDATE SET addr=EXCLUDED.addr, last_seen_at=now()`,
		id, addr)
	return err
}

// SetSessionOwner assigns a session to a worker. Used at creation time so the
// gateway can route follow-up requests (login code, /call) to the same worker.
func (s *Store) SetSessionOwner(ctx context.Context, sessionID, workerID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE session SET worker_id=$2, updated_at=now() WHERE id=$1`, sessionID, workerID)
	return err
}

// ClaimOrphans atomically assigns up to limit authorized sessions that are
// unowned or whose owner is no longer alive (last_seen_at <= aliveCutoff) to
// workerID, returning the newly-claimed rows to rehydrate. FOR UPDATE SKIP
// LOCKED lets multiple workers claim disjoint batches without contention.
func (s *Store) ClaimOrphans(ctx context.Context, workerID string, aliveCutoff time.Time, limit int) ([]Rehydratable, error) {
	rows, err := s.pool.Query(ctx,
		`WITH claim AS (
		   SELECT s.id FROM session s
		   WHERE s.status='authorized' AND s.db_dir IS NOT NULL AND s.db_key_enc IS NOT NULL
		     AND (s.worker_id IS NULL
		          OR s.worker_id NOT IN (SELECT id FROM worker WHERE last_seen_at > $2))
		   ORDER BY s.updated_at
		   LIMIT $3
		   FOR UPDATE SKIP LOCKED
		 )
		 UPDATE session SET worker_id=$1, updated_at=now()
		 FROM claim WHERE session.id = claim.id
		 RETURNING session.id, session.kind, session.app_id,
		           COALESCE(session.proxy_id::text,''), session.db_dir, session.db_key_enc`,
		workerID, aliveCutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rehydratable
	for rows.Next() {
		var r Rehydratable
		if err := rows.Scan(&r.ID, &r.Kind, &r.AppID, &r.ProxyID, &r.DBDir, &r.DBKeyEnc); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SessionRoute returns the advertised address of the worker currently owning
// the session, or "" if it is unowned or the owner is not alive.
func (s *Store) SessionRoute(ctx context.Context, sessionID string, aliveCutoff time.Time) (string, error) {
	var addr string
	err := s.pool.QueryRow(ctx,
		`SELECT w.addr FROM session s JOIN worker w ON w.id = s.worker_id
		 WHERE s.id=$1 AND w.last_seen_at > $2`, sessionID, aliveCutoff).Scan(&addr)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return addr, err
}

// ReleaseSessions clears this worker's ownership so peers immediately reclaim
// its authorized sessions (worker_id NULL -> orphan on the next sweep). Used on
// graceful shutdown to avoid waiting out the staleness window.
func (s *Store) ReleaseSessions(ctx context.Context, workerID string) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE session SET worker_id=NULL, updated_at=now()
		 WHERE worker_id=$1 AND status='authorized'`, workerID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DeregisterWorker removes this worker's registry row on graceful shutdown.
func (s *Store) DeregisterWorker(ctx context.Context, workerID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM worker WHERE id=$1`, workerID)
	return err
}

// WorkerLoad returns this worker's authorized-session count, the fleet total,
// and the number of live workers — the inputs the reconciler uses to compute
// its fair share (ceil(total/live)) when balancing.
func (s *Store) WorkerLoad(ctx context.Context, workerID string, aliveCutoff time.Time) (mine, total, liveWorkers int, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT
		   (SELECT count(*) FROM session WHERE worker_id=$1 AND status='authorized'),
		   (SELECT count(*) FROM session WHERE status='authorized'),
		   (SELECT count(*) FROM worker WHERE last_seen_at > $2)`,
		workerID, aliveCutoff).Scan(&mine, &total, &liveWorkers)
	return
}

// ListOwned returns authorized sessions the registry assigns to workerID, so
// the reconciler can (re)hydrate any it isn't holding live yet — handoffs and
// retries after a transient open failure.
func (s *Store) ListOwned(ctx context.Context, workerID string) ([]Rehydratable, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, kind, app_id, COALESCE(proxy_id::text,''), db_dir, db_key_enc
		 FROM session
		 WHERE worker_id=$1 AND status='authorized' AND db_dir IS NOT NULL AND db_key_enc IS NOT NULL`,
		workerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rehydratable
	for rows.Next() {
		var r Rehydratable
		if err := rows.Scan(&r.ID, &r.Kind, &r.AppID, &r.ProxyID, &r.DBDir, &r.DBKeyEnc); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ForeignOwned returns the subset of ids that the registry now assigns to a
// DIFFERENT live worker — sessions this worker still holds but must yield.
func (s *Store) ForeignOwned(ctx context.Context, workerID string, ids []string, aliveCutoff time.Time) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT s.id FROM session s JOIN worker w ON w.id = s.worker_id
		 WHERE s.id = ANY($1) AND s.worker_id <> $2 AND w.last_seen_at > $3`,
		ids, workerID, aliveCutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ReleaseSession clears ownership of a single session (worker_id -> NULL) so a
// lighter worker can claim it. Used when shedding load to rebalance.
func (s *Store) ReleaseSession(ctx context.Context, sessionID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE session SET worker_id=NULL, updated_at=now() WHERE id=$1`, sessionID)
	return err
}

// PurgeWorkers removes registry rows for workers not seen since before (a
// window far larger than the liveness threshold), so permanently-dead workers
// don't accumulate. Returns how many were deleted.
func (s *Store) PurgeWorkers(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM worker WHERE last_seen_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// PickWorker returns the least-loaded live worker's address for placing a new
// session (or a stateless call), or "" if none are alive.
func (s *Store) PickWorker(ctx context.Context, aliveCutoff time.Time) (string, error) {
	var addr string
	err := s.pool.QueryRow(ctx,
		`SELECT w.addr FROM worker w
		 WHERE w.last_seen_at > $1
		 ORDER BY (SELECT count(*) FROM session s
		           WHERE s.worker_id = w.id AND s.status='authorized') ASC,
		          w.last_seen_at DESC
		 LIMIT 1`, aliveCutoff).Scan(&addr)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return addr, err
}

// Webhook is a session's update-delivery subscription (kind='webhook').
// Filters is a raw jsonb object ({"types":[...]}), or nil for "all updates".
type Webhook struct {
	URL       string
	SecretEnc []byte
	Filters   []byte
}

// SetWebhook upserts the session's webhook subscription.
func (s *Store) SetWebhook(ctx context.Context, sessionID, url string, secretEnc, filters []byte) error {
	var f any
	if len(filters) > 0 {
		f = string(filters)
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO update_subscription (session_id, kind, url, secret_enc, filters)
		 VALUES ($1,'webhook',$2,$3,$4)
		 ON CONFLICT (session_id, kind)
		 DO UPDATE SET url=EXCLUDED.url, secret_enc=EXCLUDED.secret_enc, filters=EXCLUDED.filters`,
		sessionID, url, secretEnc, f)
	return err
}

// GetWebhook returns the session's webhook subscription, or nil if none.
func (s *Store) GetWebhook(ctx context.Context, sessionID string) (*Webhook, error) {
	w := &Webhook{}
	var filters *string
	err := s.pool.QueryRow(ctx,
		`SELECT url, secret_enc, filters FROM update_subscription WHERE session_id=$1 AND kind='webhook'`,
		sessionID).Scan(&w.URL, &w.SecretEnc, &filters)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if filters != nil {
		w.Filters = []byte(*filters)
	}
	return w, nil
}

// DeleteWebhook removes the session's webhook subscription (idempotent).
func (s *Store) DeleteWebhook(ctx context.Context, sessionID string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM update_subscription WHERE session_id=$1 AND kind='webhook'`, sessionID)
	return err
}

// CancelPendingDeliveries drops still-pending deliveries for a session, so
// removing a subscription doesn't leave undeliverable rows behind.
func (s *Store) CancelPendingDeliveries(ctx context.Context, sessionID string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM webhook_delivery WHERE session_id=$1 AND status='pending'`, sessionID)
	return err
}

// PurgeDeliveries removes terminal (delivered/failed) rows older than before,
// returning how many were deleted.
func (s *Store) PurgeDeliveries(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM webhook_delivery
		 WHERE status IN ('delivered','failed') AND created_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// Delivery is one queued webhook payload plus its current subscription target.
type Delivery struct {
	ID        string
	SessionID string
	Payload   []byte
	Attempts  int
	URL       string
	SecretEnc []byte
}

// EnqueueDelivery persists an update payload for at-least-once delivery.
func (s *Store) EnqueueDelivery(ctx context.Context, sessionID string, payload []byte) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO webhook_delivery (session_id, payload) VALUES ($1, $2::jsonb)`,
		sessionID, string(payload))
	return err
}

// ClaimDeliveries returns due pending deliveries for sessions owned by
// workerID, joined with their current subscription. Delivery is partitioned by
// owner: each worker delivers only its own sessions' updates, so one dispatcher
// per worker means no two workers claim the same row (no locking needed). Rows
// whose subscription was removed are not returned (they linger until cleanup).
func (s *Store) ClaimDeliveries(ctx context.Context, workerID string, limit int) ([]Delivery, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT d.id, d.session_id, d.payload::text, d.attempts, sub.url, sub.secret_enc
		 FROM webhook_delivery d
		 JOIN update_subscription sub ON sub.session_id = d.session_id AND sub.kind='webhook'
		 JOIN session ss ON ss.id = d.session_id
		 WHERE d.status='pending' AND d.next_try_at <= now() AND ss.worker_id = $1
		 ORDER BY d.next_try_at
		 LIMIT $2`, workerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		var d Delivery
		if err := rows.Scan(&d.ID, &d.SessionID, &d.Payload, &d.Attempts, &d.URL, &d.SecretEnc); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// MarkDelivered marks a delivery as successfully sent.
func (s *Store) MarkDelivered(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE webhook_delivery SET status='delivered', attempts=attempts+1 WHERE id=$1`, id)
	return err
}

// FailDelivery records a failed attempt: either reschedule with backoff or, if
// giving up, mark the delivery failed.
func (s *Store) FailDelivery(ctx context.Context, id string, nextTry time.Time, giveUp bool) error {
	if giveUp {
		_, err := s.pool.Exec(ctx,
			`UPDATE webhook_delivery SET status='failed', attempts=attempts+1 WHERE id=$1`, id)
		return err
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE webhook_delivery SET attempts=attempts+1, next_try_at=$2 WHERE id=$1`, id, nextTry)
	return err
}
