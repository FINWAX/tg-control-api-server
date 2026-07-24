# Telegram Control API Server

[![CI](https://github.com/FINWAX/tg-control-api-server/actions/workflows/ci.yml/badge.svg)](https://github.com/FINWAX/tg-control-api-server/actions/workflows/ci.yml)

A containerized internal service that exposes an HTTP interface to Telegram
**user accounts** (like TelegramApiServer) and **bots** (like the tdlight Bot
API) over a single engine — TDLib. Credentials, session persistence, and
authorization are all handled server-side. Aimed at self-hosted deployments of
**under ~50 accounts**.

The public surface is native td_api JSON: you send a method name plus params and
the service dispatches it on the right session. User and bot sessions never mix.

## Management console

An optional web console (Next.js served by Caddy) manages sessions, apps,
proxies, workers, and scoped API tokens — no curl required. Enable it with the
`ui` compose profile (see [Running](#running)).

![Console dashboard](docs/assets/console-dashboard.png)

Scoped API tokens grant per-session or per-app access, separate from the master
token:

![API tokens](docs/assets/console-tokens.png)

## Architecture

Two planes, split into separate containers:

- **gateway** (`cmd/gateway`) — stateless control plane. Holds no TDLib clients.
  Terminates auth, serves registry reads/writes straight from Postgres, and
  reverse-proxies session-scoped requests to the worker that owns the session.
- **worker** (`cmd/worker`) — data plane. Owns live TDLib clients (one per
  account), drives login, delivers update webhooks, and claims orphaned sessions
  from the registry. Horizontally scalable; see [Scaling workers](#scaling-workers).

Supporting pieces: **Postgres** is the registry and ownership lease (apps,
proxies, sessions, workers, webhook deliveries, API tokens). Secrets
(api_hash, bot tokens, proxy passwords, session DB keys) are encrypted with
AES-256-GCM using a master key from the environment and never leave the service
in plaintext. Each account gets its own TDLib database directory, and a per-session
proxy is mandatory by design.

```
client ──HTTP──> gateway ──reverse-proxy──> worker ──CGO──> libtdjson ──> Telegram
                    │                          │
                    └────────── Postgres ──────┘   (registry + ownership lease)
```

Internals: `internal/api` (worker HTTP surface), `internal/router` (gateway
routing + auth), `internal/session` (worker manager + authorizer + reconciler),
`internal/store` (Postgres), `internal/secret` (AES-GCM), `internal/tdjson`
(raw td_api JSON dispatch).

## Prerequisites

- Docker + Docker Compose.
- No local Go/Node toolchain needed — everything builds in containers.

Pinned versions (do not drift): go-tdlib `v1.0.0-beta1` against image
`ghcr.io/zelenin/tdlib-docker:b498497-alpine` (TDLib `b498497`).

## Setup

```sh
cp .env.example .env
# Fill in .env — both values are `openssl rand -base64 32`:
#   MASTER_KEY  — AES-256 key for the secret store
#   API_TOKEN   — master bearer token for the gateway API
```

`.env` holds infra config only and is gitignored. Tenant credentials
(api_id/api_hash, bot tokens, phones, proxies) are **never** put in env — they
are sent to the API and stored encrypted in Postgres.

## Running

```sh
docker compose up -d --build          # gateway + postgres + workers
docker compose logs -f gateway        # follow logs
docker compose down                   # stop (volumes kept)
docker compose down -v                # stop and wipe data
```

Optional management console (Next.js served by Caddy, reverse-proxying the API):

```sh
docker compose --profile ui up -d     # console on http://localhost:3080
```

Sign in with the master `API_TOKEN`. The console manages sessions, apps, proxies,
workers, and scoped tokens without hand-rolling curl.

Toolchain smoke test:

```sh
docker build -t tg-control-api-server . && docker run --rm tg-control-api-server ./smoke
```

## Development

Unit tests link libtdjson (CGO), so they run inside the build image rather than
on the host:

```sh
./scripts/test.sh              # unit tests (go test ./...) in the toolchain image
./scripts/test-integration.sh  # Postgres-backed store tests against a throwaway DB
./scripts/footprint.sh         # measure per-account memory / disk of a running stack
```

## API

Every request except `GET /healthz` must carry `Authorization: Bearer <token>`.

| Method & path | Purpose |
| --- | --- |
| `GET  /healthz` | Liveness (no auth) |
| `POST /v1/apps` `{api_id, api_hash, label}` | Register a Telegram app → `{app_id}` |
| `POST /v1/proxies` `{type, host, port, username?, password?, secret?, label?}` | Register a proxy → `{proxy_id}` |
| `POST /v1/bot` `{token, app_id, proxy_id?, label?}` | Create + log in a bot → `{id, status, me}` |
| `POST /v1/user` `{app_id, phone, proxy_id?, label?}` | Start a user login → `{id, status}` |
| `POST /v1/user/{id}/login/code` `{code}` | Submit the login code |
| `POST /v1/user/{id}/login/password` `{password}` | Submit the 2FA password |
| `POST /v1/{user\|bot}/{id}/call` `{method, params}` | Async td_api call on the session |
| `POST /v1/execute` `{method, params}` | Synchronous td_api call (no session) |
| `PUT/DELETE /v1/{user\|bot}/{id}/updates/webhook` | Manage update delivery |
| `GET  /v1/{user\|bot}/{id}` | Session status |
| `PATCH /v1/{user\|bot}/{id}` `{label?, proxy_id?}` | Rename / change proxy (applied live) |
| `DELETE /v1/{user\|bot}/{id}` | Close and remove a session |
| `GET  /v1/{sessions\|apps\|proxies\|workers}` | Registry listings (no secrets) |
| `PATCH /v1/{apps\|proxies}/{id}` `{label}` | Rename |
| `GET/POST/PATCH/DELETE /v1/tokens` | Scoped API token CRUD |

Responses use an envelope: `{"ok": true, "result": ...}` or
`{"ok": false, "error": {"message": ...}}`.

## API tokens

The `API_TOKEN` from env is the **master** token: full admin access.

Additional **scoped** tokens can be created (via `/v1/tokens` or the console).
A scoped token may only invoke a session's `/call` and read its status, and only
for sessions its scope covers. Scope is the union of:

- `all_sessions` — every session, or
- an explicit `session_ids` list, or
- `app_ids` — all sessions belonging to those apps.

Each token has an `enabled` flag. Only the SHA-256 of a token's secret is stored;
the plaintext is shown once at creation.

## Scaling workers

Workers are stateless with respect to identity — each registers itself and the
reconciler rebalances sessions to a fair share (`ceil(total / live workers)`).
Set the count in `docker-compose.yml` (`deploy.replicas`) or override at runtime:

```sh
docker compose up -d --scale worker=4
```

Minimum one worker. Killing a worker (graceful or hard) fails its sessions over
to peers within seconds. All replicas share the `tdlibdata` volume, so this
assumes a single host; spreading across hosts would need shared storage.

## Observability

OpenTelemetry is built in and opt-in. Set `OTEL_EXPORTER_OTLP_ENDPOINT` (plus any
standard `OTEL_*` variables) on the gateway and workers to export **traces,
metrics, and logs** over OTLP to any collector:

- **Traces** span each request end to end, from the gateway into the owning
  worker (context is propagated on the reverse-proxy hop).
- **Metrics** are HTTP server/client metrics from `otelhttp`.
- **Logs** — every `log` line is teed to OTLP while still printing to stdout
  (Docker logs), so nothing is lost.

With no endpoint configured, telemetry is disabled and the service just logs to
stdout.

## License

Apache License 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
Contributions welcome; see [CONTRIBUTING.md](CONTRIBUTING.md).
