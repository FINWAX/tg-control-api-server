package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// migration is one ordered, forward-only schema change. Version numbers are
// dense and start at 1; each is applied exactly once and recorded in
// schema_migration. SQL must be idempotent-safe for the first release (v1 uses
// IF NOT EXISTS) so pre-migration deployments — whose tables already exist from
// the old unversioned bootstrap — adopt the baseline without data loss.
type migration struct {
	Version int
	SQL     string
}

// migrations is the full, ordered history. Append new versions; never edit or
// reorder an already-released entry (that would diverge deployed databases from
// the code). A version is applied only if greater than the recorded maximum, so
// existing data is preserved and upgrades are additive.
var migrations = []migration{
	{Version: 1, SQL: schemaV1},
}

// schemaV1 is the baseline: every table from the original unversioned bootstrap,
// with IF NOT EXISTS so a database created before migrations existed simply
// records v1 without recreating (or dropping) anything.
const schemaV1 = `
CREATE TABLE IF NOT EXISTS tg_app (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  api_id       integer NOT NULL,
  api_hash_enc bytea   NOT NULL,
  label        text,
  created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS proxy (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  type       text NOT NULL CHECK (type IN ('socks5','http','mtproto')),
  host       text NOT NULL,
  port       integer NOT NULL,
  username   text,
  secret_enc bytea,
  label      text,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS session (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  kind          text NOT NULL CHECK (kind IN ('user','bot')),
  app_id        uuid NOT NULL REFERENCES tg_app(id),
  proxy_id      uuid REFERENCES proxy(id),
  phone         text,
  bot_token_enc bytea,
  label         text,
  status        text NOT NULL DEFAULT 'new',
  db_dir        text UNIQUE,
  db_key_enc    bytea,
  worker_id     text,
  last_seen_at  timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS session_status_idx ON session (status);
CREATE INDEX IF NOT EXISTS session_kind_idx   ON session (kind);

CREATE TABLE IF NOT EXISTS update_subscription (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  session_id uuid NOT NULL REFERENCES session(id) ON DELETE CASCADE,
  kind       text NOT NULL DEFAULT 'webhook' CHECK (kind IN ('webhook')),
  url        text NOT NULL,
  secret_enc bytea,
  filters    jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (session_id, kind)
);

CREATE TABLE IF NOT EXISTS webhook_delivery (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  session_id  uuid NOT NULL REFERENCES session(id) ON DELETE CASCADE,
  payload     jsonb NOT NULL,
  status      text NOT NULL DEFAULT 'pending'
              CHECK (status IN ('pending','delivered','failed')),
  attempts    integer NOT NULL DEFAULT 0,
  next_try_at timestamptz NOT NULL DEFAULT now(),
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS webhook_delivery_due_idx ON webhook_delivery (status, next_try_at);

CREATE TABLE IF NOT EXISTS worker (
  id           text PRIMARY KEY,
  addr         text NOT NULL,
  last_seen_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS api_token (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name         text,
  secret_hash  bytea NOT NULL UNIQUE,
  enabled      boolean NOT NULL DEFAULT true,
  all_sessions boolean NOT NULL DEFAULT false,
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS api_token_app (
  token_id uuid NOT NULL REFERENCES api_token(id) ON DELETE CASCADE,
  app_id   uuid NOT NULL REFERENCES tg_app(id) ON DELETE CASCADE,
  PRIMARY KEY (token_id, app_id)
);
CREATE TABLE IF NOT EXISTS api_token_session (
  token_id   uuid NOT NULL REFERENCES api_token(id) ON DELETE CASCADE,
  session_id uuid NOT NULL REFERENCES session(id) ON DELETE CASCADE,
  PRIMARY KEY (token_id, session_id)
);
`

// applyMigrations runs every migration newer than the recorded version, in
// order, within tx (the caller holds a transaction-scoped advisory lock, so
// only one process migrates at a time). Each is recorded on success; the whole
// batch commits atomically with the caller's transaction.
func applyMigrations(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migration (
		  version    integer PRIMARY KEY,
		  applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("migration table: %w", err)
	}
	var current int
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migration`).Scan(&current); err != nil {
		return fmt.Errorf("read migration version: %w", err)
	}
	for _, m := range migrations {
		if m.Version <= current {
			continue
		}
		if _, err := tx.Exec(ctx, m.SQL); err != nil {
			return fmt.Errorf("apply migration %d: %w", m.Version, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migration (version) VALUES ($1)`, m.Version); err != nil {
			return fmt.Errorf("record migration %d: %w", m.Version, err)
		}
	}
	return nil
}
