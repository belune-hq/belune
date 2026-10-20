<div align="center">
  <img src="apps/web/public/favicon.svg" alt="Belune" width="72" height="72">
  <h1>Belune</h1>
  <p><strong>Deploy like the cloud — on servers you own.</strong></p>
  <p>
    An open-source, self-hosted Platform-as-a-Service in a single Go binary.
    Git push to deploy, managed databases, automatic HTTPS, and backups that
    actually restore.
  </p>
  <p>
    <a href="LICENSE"><img alt="License: Apache 2.0" src="https://img.shields.io/badge/license-Apache%202.0-blue.svg"></a>
    <a href="https://github.com/belune-hq/belune/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/belune-hq/belune/actions/workflows/ci.yml/badge.svg"></a>
    <!-- TODO(phase-b): release badge once the tag-driven release workflow publishes to GHCR
    <a href="https://github.com/belune-hq/belune/releases/latest"><img alt="Latest release" src="https://img.shields.io/github/v/release/belune-hq/belune"></a>
    -->
  </p>
</div>

<!-- TODO(assets): hero screenshot of the dashboard (dark theme) -->
<!-- TODO(assets): 90-second GIF — template catalog → running app with TLS -->

---

## Install

One command on a fresh Linux host with Docker:

```bash
curl -sSL https://raw.githubusercontent.com/belune-hq/belune/refs/heads/main/scripts/install.sh | bash
```

Then follow the [install runbook](docs/runbooks/install.md) for DNS, TLS, the
systemd unit, and a first deploy. Full configuration reference:
[`docs/configuration.md`](docs/configuration.md).

**Requirements:** a Linux host with Docker and Docker Compose, ports 80 and 443
free, and a domain pointed at the server for automatic HTTPS.

## What it does

- **Deploy from git** — push to deploy via webhooks, or trigger from CI with a
  per-app [deploy hook](docs/runbooks/auto-deploy.md). Builds with Railpack,
  Buildpacks, or your own Dockerfile.
- **One-click apps** — a catalog of templates (Uptime Kuma, Gitea, n8n, Ghost,
  Metabase, Vaultwarden and more) that provision as *native* Belune objects, so
  they get backups and upgrades like everything else.
- **Managed databases** — PostgreSQL, MySQL, MongoDB, Redis, or bring your own
  image. Scheduled backups to S3-compatible storage, in-app restore, and
  **guarded major-version upgrades** that roll back if they fail.
- **Automatic HTTPS** — Let's Encrypt via Caddy, custom certificate upload, and
  a status pipeline that tells you *why* issuance failed instead of spinning.
- **Volumes and file mounts** — persistent storage with its own scheduled
  snapshot backups and restore.
- **Notifications** — Discord, Telegram, Slack, ntfy, Gotify, generic webhook,
  or email when a deploy or backup fails.
- **Operations built in** — per-container logs and metrics, a browser terminal,
  audit log, per-user quotas, disk cleanup, and read-only Docker views.

## Why Belune

**Your databases are treated like they matter.** Most self-hosted PaaS tools
treat a database as a container they happened to start. Belune gives them
scheduled backups to S3, restore you can run from the UI, and major-version
upgrades that dump, verify, migrate, and roll back if anything goes wrong — the
destructive paths are covered by round-trip tests against real Postgres, MySQL,
and MongoDB containers, not mocks.

**You can always see what TLS is doing.** Certificate issuance elsewhere is a
spinner that either resolves or doesn't. Belune probes what is actually being
served, parses the proxy's ACME errors, and shows you the real reason —
"hostname resolves to 203.0.113.7, expected 198.51.100.2" — with status per
domain and expiry warnings before they bite.

## How it compares

| | Belune | Coolify | Dokploy | CapRover |
|---|---|---|---|---|
| Runtime | Single Go binary + Docker | PHP / Laravel stack | Node + Docker Swarm | Node + Docker Swarm |
| One-click app templates | ✅ | ✅ | ✅ | ✅ |
| Managed databases | ✅ | ✅ | ✅ | ✅ |
| Guarded DB major-version upgrades | ✅ | — | — | — |
| Scheduled DB **and volume** backups to S3 | ✅ | Partial | Partial | — |
| Per-domain TLS status with failure reasons | ✅ | — | — | — |
| **Multiple servers** | ❌ *(planned)* | ✅ | ✅ | ✅ |
| **Docker Compose apps** | ❌ *(planned)* | ✅ | ✅ | Limited |
| Maturity / community | Brand new | Large | Growing | Established |

Belune is younger and smaller in scope than the alternatives — if you need to
manage a fleet of servers or import compose stacks today, one of them is the
better answer. Comparisons reflect publicly documented features and are
maintained in good faith; corrections are welcome as an issue.

## Status

Belune is **pre-1.0 alpha** and under active development. It runs real
workloads, but the versioning contract is:

- **Patch releases** are always safe to apply.
- **Minor releases may contain breaking changes** until 1.0.
- **Always take a backup before upgrading** — Belune ships the tooling to do it.

## Documentation

- [Install](docs/runbooks/install.md) · [Configuration](docs/configuration.md) ·
  [Troubleshooting](docs/runbooks/troubleshooting.md)
- [Auto-deploy](docs/runbooks/auto-deploy.md) ·
  [TLS and certificates](docs/runbooks/tls.md) ·
  [Notifications](docs/runbooks/notifications.md)
- [Backups and disaster recovery](docs/runbooks/disaster-recovery.md) ·
  [Key rotation](docs/runbooks/key-rotation.md)
- [Architecture](docs/architecture.md) · [API](docs/api.md)

<!-- TODO(phase-d): point these at belune.dev once the docs site is live -->

## Contributing

Issues, documentation fixes, and new app templates are all welcome — adding a
template needs no Go or React. See [CONTRIBUTING.md](CONTRIBUTING.md) for the
development setup and the DCO sign-off every commit needs (`git commit -s`).

Security issues: please report them [privately](SECURITY.md), never as a public
issue.

## Licence

[Apache License 2.0](LICENSE) — see [NOTICE](NOTICE).

Everything an individual or homelab needs to run Belune is free and will remain
free. If a paid tier ever exists, it will target organisation-only needs such as
SAML/SCIM and compliance reporting — never the features you rely on today.
