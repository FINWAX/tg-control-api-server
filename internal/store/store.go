// Package store is the Postgres registry: apps, proxies, and sessions. It is
// the source of truth for creds (encrypted) and session metadata.
package store

import (
	"context"
	"encoding/json"
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

// PendingUserLogin returns the id of an unfinished login for this phone — one
// still connecting or waiting for a code or password. Creating another session
// for that number would ask Telegram for a second code, so the caller is
// pointed back at the attempt already in flight. An authorized number is not
// reported: a number may legitimately hold several sessions.
func (s *Store) PendingUserLogin(ctx context.Context, phone string) (id string, found bool, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT id::text FROM session
		 WHERE kind='user' AND phone=$1
		   AND status IN ('connecting','awaiting_code','awaiting_password')
		 ORDER BY created_at DESC LIMIT 1`, phone).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

// ExpireStaleLogins writes off logins whose owning worker is gone. The waiting
// state (connecting / awaiting_code / awaiting_password) exists only in that
// worker's memory, so once it is dead no code can ever be delivered and the row
// would otherwise keep offering a login that cannot be resumed. The grace period
// covers a session in the middle of being created, before ownership is set.
func (s *Store) ExpireStaleLogins(ctx context.Context, aliveCutoff time.Time, grace time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE session s SET status='expired', updated_at=now()
		 WHERE s.status IN ('connecting','awaiting_code','awaiting_password')
		   AND s.updated_at < now() - $2::interval
		   AND NOT EXISTS (
		     SELECT 1 FROM worker w WHERE w.id = s.worker_id AND w.last_seen_at >= $1
		   )`, aliveCutoff, grace)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
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
	// What Telegram has said about the account; see SessionLimits.
	IsPremium   *bool `json:"is_premium,omitempty"` // unknown until TDLib reports it
	Frozen      bool  `json:"frozen"`
	SpamLimited bool  `json:"spam_limited"`
}

// sessionListSQL is the shared projection behind the session listings; callers
// append their own WHERE and ORDER BY.
const sessionListSQL = `SELECT s.id, s.kind, s.status, COALESCE(s.phone,''), COALESCE(s.label,''),
	        COALESCE(s.worker_id,''), s.last_seen_at, s.created_at,
	        s.app_id::text, COALESCE(a.label,''),
	        COALESCE(s.proxy_id::text,''), p.type, p.host, p.port,
	        s.is_premium, s.frozen_at IS NOT NULL,
	        EXISTS(SELECT 1 FROM cooldown c
	               WHERE c.session_id = s.id AND c.scope = 'spam_limit' AND c.ends_at > now())
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
			&si.ProxyID, &ptype, &phost, &pport,
			&si.IsPremium, &si.Frozen, &si.SpamLimited); err != nil {
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
// subscriptions and queued deliveries (ON DELETE CASCADE). found is false when no
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

// SessionRoute returns the session's kind ("user"/"bot") and the advertised
// address of the worker currently owning it — "" when the session is unowned or
// its owner is not alive. found is false when no such session exists; the LEFT
// JOIN keeps those two cases distinct, so a caller can answer 404 for an unknown
// id and 503 only for a genuinely unhosted one. The kind lets the caller check
// the {user|bot} path segment against reality without a second query.
func (s *Store) SessionRoute(ctx context.Context, sessionID string, aliveCutoff time.Time) (kind, addr string, found bool, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT s.kind, COALESCE(w.addr,'')
		 FROM session s
		 LEFT JOIN worker w ON w.id = s.worker_id AND w.last_seen_at > $2
		 WHERE s.id=$1`, sessionID, aliveCutoff).Scan(&kind, &addr)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return kind, addr, true, nil
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

// Subscription is one update subscription of a session, with its delivery
// status. TokenID names the scoped token that owns it; nil means it was created
// with the master token. Dropped counts updates the session lost before they
// could be queued (see AddDropped) while this subscription existed.
type Subscription struct {
	ID              string          `json:"id"`
	SessionID       string          `json:"session_id"`
	TokenID         *string         `json:"token_id"`
	URL             string          `json:"url"`
	Filters         json.RawMessage `json:"filters,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	LastDeliveredAt *time.Time      `json:"last_delivered_at"`
	LastError       *string         `json:"last_error"`
	LastErrorAt     *time.Time      `json:"last_error_at"`
	Pending         int64           `json:"pending"`
	Failed          int64           `json:"failed"`
	Dropped         int64           `json:"dropped"`
}

// CreateSubscription adds a subscription to a session. tokenID is the owning
// scoped token, or "" for the master token. filters is a raw jsonb object, or
// nil for "every update".
func (s *Store) CreateSubscription(ctx context.Context, sessionID, tokenID, url string, secretEnc, filters []byte) (string, error) {
	var f any
	if len(filters) > 0 {
		f = string(filters)
	}
	var id string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO update_subscription (session_id, token_id, kind, url, secret_enc, filters)
		 VALUES ($1, $2, 'webhook', $3, $4, $5) RETURNING id`,
		sessionID, nullStr(tokenID), url, secretEnc, f).Scan(&id)
	return id, err
}

// ListSubscriptions returns a session's subscriptions with their delivery
// status. A non-empty tokenID narrows the list to that token's own; "" (the
// master token) sees them all. subID, when set, selects a single one.
func (s *Store) ListSubscriptions(ctx context.Context, sessionID, tokenID, subID string) ([]Subscription, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT sub.id, sub.session_id, sub.token_id::text, sub.url, sub.filters::text,
		        sub.created_at, sub.last_delivered_at, sub.last_error, sub.last_error_at,
		        (SELECT count(*) FROM webhook_delivery d WHERE d.subscription_id=sub.id AND d.status='pending'),
		        (SELECT count(*) FROM webhook_delivery d WHERE d.subscription_id=sub.id AND d.status='failed'),
		        sub.dropped
		 FROM update_subscription sub
		 WHERE sub.session_id=$1
		   AND ($2 = '' OR sub.token_id::text = $2)
		   AND ($3 = '' OR sub.id::text = $3)
		 ORDER BY sub.created_at`, sessionID, tokenID, subID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Subscription{}
	for rows.Next() {
		var (
			sub     Subscription
			filters *string
		)
		if err := rows.Scan(&sub.ID, &sub.SessionID, &sub.TokenID, &sub.URL, &filters,
			&sub.CreatedAt, &sub.LastDeliveredAt, &sub.LastError, &sub.LastErrorAt,
			&sub.Pending, &sub.Failed, &sub.Dropped); err != nil {
			return nil, err
		}
		if filters != nil {
			sub.Filters = json.RawMessage(*filters)
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// DeleteSubscription removes one subscription of a session, and with it every
// delivery still queued for it (cascade). A non-empty tokenID only matches that
// token's own subscriptions, so a token cannot remove another's — it sees the
// same "not found" as for an id that never existed.
func (s *Store) DeleteSubscription(ctx context.Context, sessionID, tokenID, subID string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM update_subscription
		 WHERE session_id=$1 AND id::text=$3 AND ($2 = '' OR token_id::text = $2)`,
		sessionID, tokenID, subID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// SubscriptionFilter is what a worker needs to route updates: which
// subscription, and its raw filter (nil = every update).
type SubscriptionFilter struct {
	ID        string
	SessionID string
	Filters   []byte
}

// SubscriptionFilters returns the subscriptions of the given sessions. A worker
// reloads these for the sessions it holds, so subscriptions created or removed
// through any gateway take effect without a restart.
func (s *Store) SubscriptionFilters(ctx context.Context, sessionIDs []string) ([]SubscriptionFilter, error) {
	if len(sessionIDs) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, session_id, filters::text FROM update_subscription
		 WHERE session_id = ANY($1) ORDER BY created_at`, sessionIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SubscriptionFilter
	for rows.Next() {
		var (
			f       SubscriptionFilter
			filters *string
		)
		if err := rows.Scan(&f.ID, &f.SessionID, &filters); err != nil {
			return nil, err
		}
		if filters != nil {
			f.Filters = []byte(*filters)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// AddDropped records n updates the session lost before they reached the queue.
// The loss happens before an update is matched to subscriptions, so every
// subscription the session has is charged with it.
func (s *Store) AddDropped(ctx context.Context, sessionID string, n int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE update_subscription SET dropped = dropped + $2 WHERE session_id=$1`,
		sessionID, n)
	return err
}

// PurgeDeliveries removes rows older than before, returning how many were
// deleted. That includes rows still pending: a retry schedule spans well under
// an hour, so a pending row this old is one that can no longer be sent — its
// token disabled, or its session unhosted all along — and would otherwise
// linger forever.
func (s *Store) PurgeDeliveries(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM webhook_delivery WHERE created_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// Delivery is one queued webhook payload plus its subscription's target.
type Delivery struct {
	ID             string
	SessionID      string
	Kind           string // the session's kind: user or bot
	SubscriptionID string
	Payload        []byte
	Attempts       int
	URL            string
	SecretEnc      []byte
}

// EnqueueDeliveries queues one payload for each of the given subscriptions. A
// subscription removed since the caller looked it up is skipped rather than
// failing the rest.
func (s *Store) EnqueueDeliveries(ctx context.Context, sessionID string, subIDs []string, payload []byte) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO webhook_delivery (session_id, subscription_id, payload)
		 SELECT $1, sub.id, $3::jsonb FROM update_subscription sub
		 WHERE sub.id = ANY($2) AND sub.session_id = $1`,
		sessionID, subIDs, string(payload))
	return err
}

// ClaimDeliveries returns due pending deliveries for sessions owned by
// workerID. Delivery is partitioned by owner: each worker delivers only its own
// sessions' updates, so one dispatcher per worker means no two workers claim
// the same row (no locking needed).
//
// Three things keep one receiver from holding up the rest. busy names the
// subscriptions whose deliveries are already in flight; they are skipped so a
// slow receiver is never handed its next row while the last one is still
// waiting. perSub caps how many rows one subscription contributes, so a backlog
// at one receiver cannot fill the batch. And a subscription whose token is
// disabled, or no longer covers the session, is not served at all — checked
// here rather than when the subscription was made, so revoking access takes
// effect at once. A deleted token takes its subscriptions with it (cascade).
func (s *Store) ClaimDeliveries(ctx context.Context, workerID string, busy []string, perSub, limit int) ([]Delivery, error) {
	if busy == nil {
		busy = []string{}
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, session_id, kind, subscription_id, payload, attempts, url, secret_enc FROM (
		   SELECT d.id, d.session_id, ss.kind, d.subscription_id, d.payload::text AS payload,
		          d.attempts, sub.url, sub.secret_enc, d.next_try_at,
		          row_number() OVER (PARTITION BY d.subscription_id
		                             ORDER BY d.next_try_at, d.created_at) AS rn
		   FROM webhook_delivery d
		   JOIN update_subscription sub ON sub.id = d.subscription_id
		   JOIN session ss ON ss.id = d.session_id
		   LEFT JOIN api_token t ON t.id = sub.token_id
		   WHERE d.status='pending' AND d.next_try_at <= now() AND ss.worker_id = $1
		     AND NOT (d.subscription_id::text = ANY($2))
		     AND (sub.token_id IS NULL OR (t.enabled AND (
		           t.all_sessions
		           OR EXISTS(SELECT 1 FROM api_token_session ts
		                     WHERE ts.token_id=t.id AND ts.session_id=d.session_id)
		           OR EXISTS(SELECT 1 FROM api_token_app ta
		                     WHERE ta.token_id=t.id AND ta.app_id=ss.app_id))))
		 ) due
		 WHERE rn <= $3
		 ORDER BY next_try_at
		 LIMIT $4`, workerID, busy, perSub, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		var d Delivery
		if err := rows.Scan(&d.ID, &d.SessionID, &d.Kind, &d.SubscriptionID, &d.Payload,
			&d.Attempts, &d.URL, &d.SecretEnc); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// MarkDelivered marks a delivery as sent and stamps its subscription's last
// success.
func (s *Store) MarkDelivered(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx,
		`WITH d AS (
		   UPDATE webhook_delivery SET status='delivered', attempts=attempts+1
		   WHERE id=$1 RETURNING subscription_id)
		 UPDATE update_subscription SET last_delivered_at=now()
		 WHERE id IN (SELECT subscription_id FROM d)`, id)
	return err
}

// FailDelivery records a failed attempt — either rescheduled with backoff or,
// when giving up, marked failed — and keeps the error on the subscription so a
// consumer can see why its receiver is not being reached.
func (s *Store) FailDelivery(ctx context.Context, id string, nextTry time.Time, giveUp bool, reason string) error {
	_, err := s.pool.Exec(ctx,
		`WITH d AS (
		   UPDATE webhook_delivery
		   SET attempts=attempts+1,
		       status=CASE WHEN $3 THEN 'failed' ELSE status END,
		       next_try_at=CASE WHEN $3 THEN next_try_at ELSE $2 END
		   WHERE id=$1 RETURNING subscription_id)
		 UPDATE update_subscription SET last_error=$4, last_error_at=now()
		 WHERE id IN (SELECT subscription_id FROM d)`, id, nextTry, giveUp, reason)
	return err
}

// SentMessage is one tracked send. Status is "pending" until the update stream
// resolves it, then "sent" (FinalID holds the server id) or "failed" (Error
// holds the rendered td_api error).
type SentMessage struct {
	Status  string
	FinalID int64
	Error   []byte
}

// RegisterSend records a dispatched send as pending. The outcome update may be
// processed before this insert lands, so an existing row always wins: the
// registration is the weaker fact.
func (s *Store) RegisterSend(ctx context.Context, sessionID string, chatID, tempID int64) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO sent_message (session_id, chat_id, temp_id, status)
		 VALUES ($1,$2,$3,'pending')
		 ON CONFLICT (session_id, chat_id, temp_id) DO NOTHING`,
		sessionID, chatID, tempID)
	return err
}

// RecordSendOutcome upserts a send's terminal state. It neither requires nor
// preserves a prior pending row, so registration and outcome may arrive in
// either order. A non-nil tdErr marks the send failed.
func (s *Store) RecordSendOutcome(ctx context.Context, sessionID string, chatID, tempID, finalID int64, tdErr []byte) error {
	status := "sent"
	var e any
	if len(tdErr) > 0 {
		status = "failed"
		e = string(tdErr)
		finalID = 0
	}
	var final any
	if finalID != 0 {
		final = finalID
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO sent_message (session_id, chat_id, temp_id, status, final_id, error)
		 VALUES ($1,$2,$3,$4,$5,$6)
		 ON CONFLICT (session_id, chat_id, temp_id) DO UPDATE
		   SET status=EXCLUDED.status, final_id=EXCLUDED.final_id,
		       error=EXCLUDED.error, updated_at=now()`,
		sessionID, chatID, tempID, status, final, e)
	return err
}

// GetSend returns a tracked send. found=false means it was never registered
// (unknown or wrong id) or has already passed retention — which the caller must
// distinguish from a still-pending send.
func (s *Store) GetSend(ctx context.Context, sessionID string, chatID, tempID int64) (SentMessage, bool, error) {
	var m SentMessage
	var final *int64
	err := s.pool.QueryRow(ctx,
		`SELECT status, final_id, error FROM sent_message
		 WHERE session_id=$1 AND chat_id=$2 AND temp_id=$3`,
		sessionID, chatID, tempID).Scan(&m.Status, &final, &m.Error)
	if errors.Is(err, pgx.ErrNoRows) {
		return SentMessage{}, false, nil
	}
	if err != nil {
		return SentMessage{}, false, err
	}
	if final != nil {
		m.FinalID = *final
	}
	return m, true, nil
}

// PurgeSends removes send-registry rows created before the cutoff, returning
// how many were deleted. Pending rows go too: a send unresolved for that long
// is never going to resolve.
func (s *Store) PurgeSends(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM sent_message WHERE created_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// --- account limits ---

// SpamLimitScope is the cooldown Telegram's PEER_FLOOD imposes: the account
// may only write to mutual contacts, invite nobody and import no contacts.
const SpamLimitScope = "spam_limit"

// Cooldown is a scope of calls the gateway refuses on a session until a time,
// because Telegram asked it to wait or restricted the account. The scope is a
// method group ("@send"), a method name, or SpamLimitScope; the reason is
// Telegram's own error text.
type Cooldown struct {
	Scope  string    `json:"scope"`
	Until  time.Time `json:"until"`
	Reason string    `json:"reason"`
}

// Limits is what Telegram has said about a session's account.
type Limits struct {
	IsPremium   *bool      `json:"is_premium,omitempty"`
	Frozen      bool       `json:"frozen"`
	FrozenSince *time.Time `json:"frozen_since,omitempty"`
	// DeletionDate and AppealLink come with Telegram's own report of a freeze:
	// the account is deleted then unless the freeze is appealed at the link.
	DeletionDate *time.Time `json:"deletion_date,omitempty"`
	AppealLink   string     `json:"appeal_link,omitempty"`
	SpamLimited  bool       `json:"spam_limited"`
	Cooldowns    []Cooldown `json:"cooldowns"` // active ones, soonest to end first
}

// SessionLimits returns a session's account state and active cooldowns, or
// found=false for an unknown session.
func (s *Store) SessionLimits(ctx context.Context, id string) (l Limits, found bool, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT is_premium, frozen_at, freeze_deletion_at, COALESCE(freeze_appeal_link, '')
		 FROM session WHERE id=$1`, id).Scan(&l.IsPremium, &l.FrozenSince, &l.DeletionDate, &l.AppealLink)
	if errors.Is(err, pgx.ErrNoRows) {
		return l, false, nil
	}
	if err != nil {
		return l, false, err
	}
	l.Frozen = l.FrozenSince != nil
	rows, err := s.pool.Query(ctx,
		`SELECT scope, ends_at, reason FROM cooldown
		 WHERE session_id=$1 AND ends_at > now() ORDER BY ends_at`, id)
	if err != nil {
		return l, false, err
	}
	defer rows.Close()
	l.Cooldowns = []Cooldown{}
	for rows.Next() {
		var c Cooldown
		if err := rows.Scan(&c.Scope, &c.Until, &c.Reason); err != nil {
			return l, false, err
		}
		l.SpamLimited = l.SpamLimited || c.Scope == SpamLimitScope
		l.Cooldowns = append(l.Cooldowns, c)
	}
	return l, true, rows.Err()
}

// SetCooldown records a cooldown, keeping the later end if one is already
// running on that scope.
func (s *Store) SetCooldown(ctx context.Context, sessionID string, c Cooldown) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO cooldown (session_id, scope, ends_at, reason) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (session_id, scope) DO UPDATE
		   SET ends_at = excluded.ends_at, reason = excluded.reason, created_at = now()
		   WHERE cooldown.ends_at < excluded.ends_at`,
		sessionID, c.Scope, c.Until, c.Reason)
	return err
}

// PurgeCooldowns removes cooldowns that ended before the cutoff.
func (s *Store) PurgeCooldowns(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM cooldown WHERE ends_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// SetSessionPremium records the account's Premium flag.
func (s *Store) SetSessionPremium(ctx context.Context, id string, premium bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE session SET is_premium=$2 WHERE id=$1`, id, premium)
	return err
}

// SetSessionFrozen marks the account frozen (keeping when that was first
// seen) or clears the mark.
func (s *Store) SetSessionFrozen(ctx context.Context, id string, frozen bool) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE session SET frozen_at = CASE WHEN $2 THEN COALESCE(frozen_at, now()) END,
		   freeze_deletion_at = CASE WHEN $2 THEN freeze_deletion_at END,
		   freeze_appeal_link = CASE WHEN $2 THEN freeze_appeal_link END
		 WHERE id=$1`,
		id, frozen)
	return err
}

// SetSessionFreeze records a freeze as Telegram reports it: since when, when
// the account will be deleted (zero: not said), and where to appeal.
func (s *Store) SetSessionFreeze(ctx context.Context, id string, since, deletion time.Time, appeal string) error {
	var del *time.Time
	if !deletion.IsZero() {
		del = &deletion
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE session SET frozen_at=$2, freeze_deletion_at=$3, freeze_appeal_link=NULLIF($4, '') WHERE id=$1`,
		id, since, del, appeal)
	return err
}

// DeleteCooldown lifts a cooldown, reporting whether one was recorded.
func (s *Store) DeleteCooldown(ctx context.Context, sessionID, scope string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM cooldown WHERE session_id=$1 AND scope=$2`, sessionID, scope)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
