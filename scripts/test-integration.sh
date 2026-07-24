#!/usr/bin/env bash
# test-integration.sh — run the Postgres-backed integration tests (build tag
# `integration`) against a throwaway database, isolated from the dev stack.
# Spins an ephemeral Postgres on its own network, runs the tests in the build
# image, and tears everything down.
#
# Usage:  ./scripts/test-integration.sh
set -eu

NET=tgc-itest-net
PG=tgc-itest-pg

cleanup() {
  docker rm -f "$PG" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker network create "$NET" >/dev/null 2>&1 || true

docker run -d --rm --name "$PG" --network "$NET" \
  -e POSTGRES_USER=tgapi -e POSTGRES_PASSWORD=tgapi -e POSTGRES_DB=tgapi \
  postgres:16-alpine >/dev/null

echo "waiting for postgres..."
until docker exec "$PG" pg_isready -U tgapi >/dev/null 2>&1; do sleep 1; done

docker build --target go-builder -t tg-control-api-server-build . >/dev/null

docker run --rm --network "$NET" \
  -e DATABASE_URL="postgres://tgapi:tgapi@${PG}:5432/tgapi?sslmode=disable" \
  tg-control-api-server-build \
  go test -tags=integration -count=1 ./internal/store
