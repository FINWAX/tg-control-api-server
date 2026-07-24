#!/usr/bin/env bash
# footprint.sh — measure the runtime footprint of a running TgApi stack:
# per-container memory, per-session disk on the shared TDLib volume, and a
# rough per-account estimate. Re-run as the account count grows to track
# density. Requires the stack to be up (docker compose up -d).
#
# Usage:  ./scripts/footprint.sh
set -eu

echo "== container memory (RSS) =="
docker stats --no-stream --format '{{.Name}}\t{{.MemUsage}}' \
  | grep -E 'gateway|worker|postgres|ui' || true

echo
echo "== per-session disk (shared tdlibdata volume) =="
docker compose exec -T --index 1 worker sh -c '
  cd /data 2>/dev/null || exit 0
  du -sh */ 2>/dev/null | sort -h
  echo "---- total ----"
  du -sh /data 2>/dev/null
'

echo
echo "== session ownership per worker =="
# Reads the registry directly so no API token is needed.
docker compose exec -T postgres psql -U tgapi -tA -c "
  SELECT COALESCE(worker_id,'(unowned)') AS worker,
         count(*)                        AS sessions,
         count(*) FILTER (WHERE kind='bot')  AS bots,
         count(*) FILTER (WHERE kind='user') AS users
  FROM session
  WHERE status='authorized'
  GROUP BY worker
  ORDER BY worker;"

echo
echo "Note: memory is not the binding constraint at <50 accounts. Bots are"
echo "light (~2-4 MiB RSS, ~100 KiB disk); user sessions are heavier (~10-15"
echo "MiB RSS) and their disk grows with the file cache. Tune via the TDLib"
echo "parameters in internal/session/manager.go (buildParams)."
