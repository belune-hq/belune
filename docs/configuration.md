# Configuration reference

Every environment variable Belune reads. This page is the canonical list — the
`.env.example` files are seeds, not references, and a test asserts that
everything below stays in step with the code.

Belune reads configuration once at startup. Values that can also be changed from
the dashboard (SMTP, server IP, retention, daily cleanup, host shell) are stored
in the database and **take precedence over the environment**; the variable is the
install-time seed.

- **Required** — no safe default. `scripts/install.sh` generates these.
- **Internal** — set by Docker Compose or derived from another value. Overriding
  them usually breaks something; they are listed for completeness, not for use.

---

## Core server

| Variable | Default | Notes |
|---|---|---|
| `PORT` | `8080` | HTTP listener port |
| `DATABASE_URL` | `postgres://belune:belune@localhost:5432/belune?sslmode=disable` | **Required** in production |
| `REDIS_URL` | `redis://localhost:6379` | Job broker (asynq) |
| `CORS_ORIGINS` | `http://localhost:5173` | Comma-separated. Add your dashboard hostname |
| `SECURE_COOKIES` | `false` | Only needed behind a proxy that terminates TLS without setting `X-Forwarded-Proto` — cookies are marked Secure automatically over HTTPS |
| `TLS_ENABLED` | `false` | Sends HSTS headers; also gates custom-certificate routing in Caddy |
| `PUBLIC_BASE_URL` | *empty* | Public dashboard URL. Required once SMTP is configured, so links in email resolve |
| `WEBHOOK_PUBLIC_URL` | *empty* | Public URL git providers call back on |
| `BELUNE_PUBLIC_IP` | *auto-detect* | Address domains must point at. Overridden by the Server IP setting in the dashboard |
| `BELUNE_SKIP_MIGRATIONS` | `false` | **Development only.** Booting against an unmigrated schema fails in confusing ways |

## Logging

| Variable | Default | Notes |
|---|---|---|
| `LOG_LEVEL` | `info` | `debug` · `info` · `warn` · `error` |
| `LOG_FORMAT` | `console` | `console` for humans, `json` for anything that parses the stream |
| `LOG_COLOR` | `auto` | `auto` colours only when stdout is a terminal. Set `always` if your runner pipes output (`air` does); `never` disables it |
| `NO_COLOR` | *unset* | Cross-tool convention, honoured under `LOG_COLOR=auto` |

Console output looks like this, with attributes on an indented continuation line
and the module derived from the calling package:

```
2026-07-22 03:14:10 INFO  [worker.deploy] deploy finished
                          app_id=10d7ec5f dur=4.2s
```

Colour is off whenever the stream is captured rather than displayed. Docker gives
the container a pipe, so escape codes would otherwise be stored verbatim and
break the level detection the log viewer relies on.

## Authentication and encryption

| Variable | Default | Notes |
|---|---|---|
| `JWT_SECRET` | *empty* | **Required.** Generated at install |
| `JWT_EXPIRY_HOURS` | `1` | Access token lifetime. Short on purpose — the refresh token covers the session |
| `JWT_REFRESH_HOURS` | `168` | Refresh token lifetime (7 days) |
| `ENCRYPTION_KEY` | *empty* | **Required.** Generated at install. Encrypts stored secrets |
| `ENCRYPTION_KEYS` | *empty* | Multi-key form for rotation, `v1:hex,v2:hex` |
| `ENCRYPTION_KEY_CURRENT` | *empty* | Which key ID new writes use |

Installs created before v0.1.0 carry an explicit `JWT_EXPIRY_HOURS=24` in their
`.env`. The installer no longer writes it, but `update.sh` deliberately does not
edit an existing `.env` — silently rewriting an operator's config during an
upgrade is a worse precedent than the setting itself. The API warns at startup
and on the Projects page whenever the value exceeds 12 hours; remove the line and
restart to take the 1-hour default. Sessions are unaffected, since refresh tokens
already cover them.

Access tokens cannot be revoked, which is what makes a long lifetime costly: a
stolen one stays usable for the whole window.

Rotation is a procedure, not a setting — see
[runbooks/key-rotation.md](runbooks/key-rotation.md).

## Proxy and networking

| Variable | Default | Notes |
|---|---|---|
| `CADDY_ADMIN_URL` | `http://localhost:2019` | Caddy admin API |
| `CADDY_CONTAINER_NAME` | *auto-detect* | *Internal* — found by the `belune-system=caddy` label; only set it if you run Caddy outside the Compose stack |
| `CADDY_TLS_PROBE_ADDR` | `caddy:443` | *Internal* — where the TLS status probe dials |
| `DASHBOARD_UPSTREAM` | `belune:8080` | *Internal* — set by Compose and the Caddyfile template |
| `API_CONTAINER_NAME` | *self-detect* | *Internal* — overriding risks attaching to the wrong container |
| `PROXY_PEER_CIDRS` | *empty* | Extra TCP peers to treat as the Caddy proxy, comma-separated addresses or CIDRs. Belune finds its own Caddy container; set this only when it cannot (the dev stack, where the API runs on the host, or a Caddy that is not a container). ⚠️ Every entry may claim any client IP, so never list a network you do not control |
| `ACCESS_LOG_PATH` | `../../infra/caddy/logs/access.log` | *Internal* — Caddy access log the request-log tailer reads |

## Build and deploy

| Variable | Default | Notes |
|---|---|---|
| `BUILDKIT_HOST` | *empty* | *Internal* — set by Compose to the BuildKit service |
| `BUILD_TIMEOUT_MINUTES` | `30` | |
| `IMAGE_PULL_TIMEOUT_MINUTES` | `10` | |
| `TASK_TIMEOUT_MINUTES` | `45` | Overall worker task ceiling |
| `PREVIEW_IDLE_DAYS` | `7` | Idle preview environments are reaped after this. The preview UI is unlinked until the feature ships |

## Paths

| Variable | Default | Notes |
|---|---|---|
| `BELUNE_DIR` | `/opt/belune` | Install root. Owned by the install, backup, restore and update scripts |
| `FILE_MOUNTS_DIR` | *derived* | *Internal* — from `BELUNE_DIR` |
| `CONTROL_PLANE_BACKUP_DIR` | *derived* | *Internal* — from `BELUNE_DIR`. Where the worker writes control-plane archives; same directory `scripts/backup.sh`/`restore.sh` use |
| `ENV_FILE_PATH` | *derived* | *Internal* — from `BELUNE_DIR`. Where the worker reads `.env` to copy into a control-plane archive |
| `BACKUP_REMOTE_CONFIG_PATH` | *derived* | *Internal* — from `BELUNE_DIR` (`backup-remote.env`). Dashboard-managed remote-storage config — see below |
| `DATABASE_BACKUP_DIR` | *derived* | *Internal* — from `BELUNE_DIR` |

## Platform backups

Control-plane backups: the Belune database and Caddy certificates. Both the
in-app schedule/manual trigger (Server → Backups) and `scripts/backup.sh`
(host CLI, used for manual/DR runs and by `update.sh` before a version move)
produce the same archive format and record every run — there is no longer a
systemd timer; the daily trigger is the in-app schedule (`control_plane_backup_schedule`
setting, default `0 2 * * *`). Distinct from per-database backups below.

**Schedule, remote storage, and retention are dashboard-managed** — Server →
Backups → **Configure**. Remote storage edits save to `BACKUP_REMOTE_CONFIG_PATH`
(`backup-remote.env`, mode `0600`), a flat `KEY=value` file separate from
`.env` so the dashboard never touches bootstrap secrets like `ENCRYPTION_KEY`;
it's read fresh on every backup — no API restart needed — and both
`scripts/backup.sh` and the `belune-backup-upload` helper read the same file.
Retention (`control_plane_backup_retain_days`/`...retain_count`) lives in the
settings table instead — no bash reader needs it, since only the worker ever
prunes. The variables below are all fallbacks: unset in their dashboard-managed
source, they fall back per-key to `.env`, so setting these still works on a
stock install that has never touched the Configure sheet.

| Variable | Default | Notes |
|---|---|---|
| `BACKUP_REMOTE_ENABLED` | `false` | Fallback only — prefer the dashboard |
| `BACKUP_RETAIN_COUNT` | `14` | Applies to local archives too, not just the remote bucket. Fallback only |
| `BACKUP_RETAIN_DAYS` | `30` | Fallback only |
| `BACKUP_S3_BUCKET` | *empty* | Fallback only — prefer the dashboard |
| `BACKUP_S3_ENDPOINT` | *empty* | For S3-compatible providers. Fallback only |
| `BACKUP_S3_REGION` | `us-east-1` | Fallback only |
| `BACKUP_S3_PREFIX` | `belune/` | Fallback only |
| `BACKUP_S3_ACCESS_KEY` | *empty* | Secret. Fallback only |
| `BACKUP_S3_SECRET_KEY` | *empty* | Secret. Fallback only |
| `BACKUP_S3_USE_SSL` | `true` | Fallback only |
| `BACKUP_ENCRYPTION_KEY` | *empty* | age public key (or path to a file containing one); encrypts the archive as `.tar.gz.age`. Deliberately stays `.env`/host-only, unlike every other setting on this page — the archive contains `.env` itself (every platform secret), so changing what it encrypts to requires host access, same boundary as restore. Each run in **Recent Runs** shows a lock icon when its own archive was encrypted |

## Database backups

| Variable | Default | Notes |
|---|---|---|
| `DATABASE_BACKUP_HELPER_IMAGE` | `alpine:3.20` | *Internal* |
| `DATABASE_BACKUP_RETAIN_COUNT` | `7` | *Internal* — per-database local retention |

Destinations and schedules are configured per project in the dashboard, not here.

## Scheduled tasks

| Variable | Default | Notes |
|---|---|---|
| `SCHEDULED_TASK_MAX_CONCURRENT` | `2` | Most scheduled-task runs in flight at once; further due runs are recorded as skipped. The worker pool is shared with deploys, so keep this well under it |

## Email

Configurable from **Server → Configuration**; those values are stored in the
database and win over the environment. See [runbooks/smtp.md](runbooks/smtp.md).

| Variable | Default |
|---|---|
| `SMTP_HOST` | *empty* |
| `SMTP_PORT` | `587` |
| `SMTP_USER` | *empty* |
| `SMTP_PASSWORD` | *empty* (secret) |
| `SMTP_FROM_EMAIL` | *empty* |
| `SMTP_FROM_NAME` | `Belune` |
| `SMTP_TLS_MODE` | `starttls` — `none` · `starttls` · `tls` |

## Observability

| Variable | Default | Notes |
|---|---|---|
| `METRICS_BIND` | *empty* | Empty serves `/metrics` on the main router behind admin auth. Setting it (e.g. `127.0.0.1:9090`) starts a separate listener with **no authentication** — bind it to loopback |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | *empty* | Empty installs a no-op tracer. Accepts either `host:port` (`jaeger:4318`) or a full URL (`https://api.honeycomb.io`). OTLP/**HTTP**, so a bare host:port means port 4318 |
| `OTEL_EXPORTER_OTLP_INSECURE` | `true` | Disables TLS — but only for the bare `host:port` form. Give a URL and its scheme decides, so `https://` stays encrypted whatever this is set to |

The exporter also honours the standard `OTEL_EXPORTER_OTLP_HEADERS`,
`_TIMEOUT`, `_COMPRESSION` and `_CERTIFICATE` variables, so authenticating to a
hosted collector needs no extra configuration from Belune.

Point it at a hosted collector with a URL and an API key header:

```
OTEL_EXPORTER_OTLP_ENDPOINT=https://api.honeycomb.io
OTEL_EXPORTER_OTLP_HEADERS=x-honeycomb-team=YOUR_KEY
```

The bundled Prometheus, Grafana and Jaeger stack is opt-in via
`infra/compose.observability.yml` and is not started by the main stack. Point
`OTEL_EXPORTER_OTLP_ENDPOINT` at it only when it is running — an endpoint that
does not resolve makes the exporter retry every ten seconds.

## Limits

| Variable | Default |
|---|---|
| `MAX_WEBSOCKET_CONNS_PER_USER` | `20` |
| `MAX_TERMINAL_SESSIONS_PER_USER` | `5` |

## Host shell

| Variable | Default | Notes |
|---|---|---|
| `SERVER_SSH_HOST` | *resolved server IP* | Shown in the database tunnel command. Set it when SSH lands on a different box, such as a bastion |
| `SERVER_SSH_USER` | *empty* | Shown in the same command |

The host shell itself is enabled from **Server → Configuration**, not here.

## Compose and scripts

Not read by the Go binary — consumed by Docker Compose and the install scripts.

| Variable | Notes |
|---|---|
| `BELUNE_IMAGE` | Image tag the stack runs |
| `BELUNE_CADDY_IMAGE` | Caddy image. Defaults to `ghcr.io/belune-hq/belune-caddy:2` (upstream Caddy plus optional modules); set `caddy:2-alpine` to run stock Caddy, where module-backed features show as "not configured" |
| `DOCKER_GID` | Host docker group ID, so the container can reach the socket |
| `POSTGRES_USER` · `POSTGRES_DB` | Database bootstrap |
| `POSTGRES_PASSWORD` | Secret. Generated at install |
