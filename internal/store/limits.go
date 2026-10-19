package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Limit is an operator's rate limit: at most Max calls of Methods per window
// Per. Level "session" with SessionID is one session's own rule; with Kind
// instead, the default for every session of that kind. Level "token" is a
// consumer token's rule on each session it works with.
type Limit struct {
	ID        string    `json:"id"`
	Level     string    `json:"level"`
	Kind      *string   `json:"kind,omitempty"`
	SessionID *string   `json:"session_id,omitempty"`
	TokenID   *string   `json:"token_id,omitempty"`
	Methods   []string  `json:"methods"`
	Max       int32     `json:"max"`
	Per       string    `json:"per"`
	Label     *string   `json:"label,omitempty"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

// ErrLimitTarget is a rule naming a session or token that does not exist.
var ErrLimitTarget = errors.New("the limit's session or token does not exist")

const limitCols = `id, level, kind, session_id, token_id, methods, max_calls, per, label, enabled, created_at`

func scanLimit(row pgx.Row) (*Limit, error) {
	var l Limit
	err := row.Scan(&l.ID, &l.Level, &l.Kind, &l.SessionID, &l.TokenID, &l.Methods,
		&l.Max, &l.Per, &l.Label, &l.Enabled, &l.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &l, nil
}

func (s *Store) CreateLimit(ctx context.Context, l Limit) (*Limit, error) {
	out, err := scanLimit(s.pool.QueryRow(ctx,
		`INSERT INTO rate_limit (level, kind, session_id, token_id, methods, max_calls, per, label, enabled)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING `+limitCols,
		l.Level, l.Kind, l.SessionID, l.TokenID, l.Methods, l.Max, l.Per, l.Label, l.Enabled))
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23503" { // foreign_key_violation
		return nil, ErrLimitTarget
	}
	return out, err
}

// GetLimit returns one rule, or nil when there is none.
func (s *Store) GetLimit(ctx context.Context, id string) (*Limit, error) {
	l, err := scanLimit(s.pool.QueryRow(ctx, `SELECT `+limitCols+` FROM rate_limit WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return l, err
}

func (s *Store) ListLimits(ctx context.Context) ([]*Limit, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+limitCols+` FROM rate_limit ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Limit{}
	for rows.Next() {
		l, err := scanLimit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// UpdateLimit writes what may change: methods, max, per, label, enabled.
func (s *Store) UpdateLimit(ctx context.Context, l Limit) (*Limit, error) {
	return scanLimit(s.pool.QueryRow(ctx,
		`UPDATE rate_limit SET methods=$2, max_calls=$3, per=$4, label=$5, enabled=$6
		 WHERE id=$1 RETURNING `+limitCols,
		l.ID, l.Methods, l.Max, l.Per, l.Label, l.Enabled))
}

func (s *Store) DeleteLimit(ctx context.Context, id string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM rate_limit WHERE id=$1`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// CounterKey names one window of a rule on one session (the bucket).
type CounterKey struct {
	LimitID     string
	Bucket      string
	WindowStart time.Time
}

// CounterUse is a call counted in a window that may hold at most Max.
type CounterUse struct {
	Key CounterKey
	N   int
	Max int32
}

// TakeCounters counts every use if each still fits its window, and none
// otherwise, returning the index of the first use that does not fit (-1:
// all counted). It is one transaction, so concurrent calls cannot both take
// the last slot.
func (s *Store) TakeCounters(ctx context.Context, uses []CounterUse) (refused int, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return -1, err
	}
	defer tx.Rollback(ctx)
	for i, u := range uses {
		tag, err := tx.Exec(ctx,
			`INSERT INTO rate_counter (limit_id, bucket, window_start, calls)
			 SELECT $1::uuid, $2::text, $3::timestamptz, $4::int WHERE $4::int <= $5::int
			 ON CONFLICT (limit_id, bucket, window_start) DO UPDATE
			   SET calls = rate_counter.calls + excluded.calls
			   WHERE rate_counter.calls + excluded.calls <= $5::int`,
			u.Key.LimitID, u.Key.Bucket, u.Key.WindowStart, u.N, u.Max)
		if err != nil {
			return -1, err
		}
		if tag.RowsAffected() == 0 {
			return i, nil
		}
	}
	return -1, tx.Commit(ctx)
}

// GiveBackCounters takes back uses counted for a call that never went out.
func (s *Store) GiveBackCounters(ctx context.Context, uses []CounterUse) error {
	for _, u := range uses {
		if _, err := s.pool.Exec(ctx,
			`UPDATE rate_counter SET calls = GREATEST(calls - $4, 0)
			 WHERE limit_id=$1 AND bucket=$2 AND window_start=$3`,
			u.Key.LimitID, u.Key.Bucket, u.Key.WindowStart, u.N); err != nil {
			return err
		}
	}
	return nil
}

// Counters reads the calls counted in the given windows (0 when none yet).
func (s *Store) Counters(ctx context.Context, keys []CounterKey) (map[CounterKey]int, error) {
	out := make(map[CounterKey]int, len(keys))
	for _, k := range keys {
		var n int
		err := s.pool.QueryRow(ctx,
			`SELECT calls FROM rate_counter WHERE limit_id=$1 AND bucket=$2 AND window_start=$3`,
			k.LimitID, k.Bucket, k.WindowStart).Scan(&n)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		out[k] = n
	}
	return out, nil
}

// MaxCounter is the highest count among a rule's sessions in one window.
func (s *Store) MaxCounter(ctx context.Context, limitID string, windowStart time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(calls), 0) FROM rate_counter WHERE limit_id=$1 AND window_start=$2`,
		limitID, windowStart).Scan(&n)
	return n, err
}

// PurgeCounters drops windows that started before t.
func (s *Store) PurgeCounters(ctx context.Context, t time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM rate_counter WHERE window_start < $1`, t)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
