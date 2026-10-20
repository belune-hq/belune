# Install & First-Run Checklist

End-to-end runbook for bringing a fresh Belune install online: DNS,
TLS, first deploy, persistence, and updates. Pair with `scripts/install.sh`,
which handles the boring parts (compose, secrets, image pull) automatically.

---

## 1. Prerequisites

- Linux host with Docker Engine 24+ and Docker Compose v2.
- Root or sudo access on the host (for the systemd unit and port 80/443).
- A domain you control (for TLS via Let's Encrypt). A bare IP works for
  evaluation but not for Caddy automatic TLS.
- Outbound HTTPS to `ghcr.io`, your git host, and Let's Encrypt's ACME
  endpoints.

---

## 2. Run the installer

```sh
curl -sSL https://raw.githubusercontent.com/belune-hq/belune/refs/heads/main/scripts/install.sh | bash
```

This:

- Creates `/opt/belune` (override with `BELUNE_DIR=/path bash install.sh`).
- Downloads `docker-compose.yml` and the Caddyfile template.
- Generates a `.env` containing fresh `JWT_SECRET`, `ENCRYPTION_KEY`, and
  Postgres password — keep this file out of version control.
- Pulls `ghcr.io/weiliang79/belune:latest` and runs `docker compose up -d`.
- Waits for `GET /healthz` to return 200.

When it finishes, the panel is reachable at `http://<host>` — the server's bare
IP address, over plain HTTP. That is expected: you set a domain from inside the
dashboard in step 5, and HTTPS follows. The API is not published on a public port;
it is reached through Caddy.

> Reference for every other config option: [`docs/configuration.md`](../configuration.md).

---

## 3. Install the systemd unit (recommended)

The compose stack uses `restart: unless-stopped`, so individual containers
respawn after Docker restarts. The systemd unit's job is to bring the stack
up on host reboot before anyone needs to log in.

```sh
sudo cp /opt/belune/infra/systemd/belune.service /etc/systemd/system/belune.service
sudo systemctl daemon-reload
sudo systemctl enable --now belune.service
```

Verify:

```sh
systemctl status belune.service        # should be 'active (exited)'
docker compose -f /opt/belune/docker-compose.yml ps  # all healthy
```

If you keep the install in a non-default directory, edit
`WorkingDirectory=` in the unit before installing.

---

## 4. DNS

You need at least two records. Replace `belune.example.com` with your
chosen panel hostname.

| Record           | Type  | Target              | Purpose                        |
| ---------------- | ----- | ------------------- | ------------------------------ |
| `belune.example.com` | A   | `<host IP>`         | Dashboard + API                |
| `*.example.com`  | A     | `<host IP>`         | Apps deployed via the platform |

If you plan to use **preview environments**, you also need a wildcard for
the preview subdomain template you configure on each app — typically
`*.preview.example.com → <host IP>`.

**TTL** can be low (300s) while you bootstrap; raise it once stable.

---

## 5. HTTPS for the dashboard

Out of the box the dashboard answers on the server's IP over plain HTTP. To serve
it on your own hostname with a Let's Encrypt certificate:

1. Open `http://<host>` and create the admin account (step 6 below) — the first
   login happens over HTTP on the IP, before a certificate can exist.
2. Go to **Server → Configuration → Dashboard domain**.
3. Enter your panel hostname (`belune.example.com`) and **Save**.

Belune publishes that hostname to Caddy, which requests a certificate from Let's
Encrypt automatically. The badge under the field goes from *Waiting for
certificate* to **HTTPS active**, usually within a minute. If it does not, the
badge tells you why — see [`tls.md`](./tls.md).

This needs the DNS record from step 4 to already resolve, and ports 80 and 443
open to the internet. **Port 80 is required even though the site runs on 443** —
it is how Let's Encrypt validates that you control the domain.

Session cookies become `Secure` on their own as soon as the panel is served over
HTTPS — there is nothing to toggle. Two settings are still worth having in
`/opt/belune/.env` so generated links and HSTS are right:

```env
PUBLIC_BASE_URL=https://belune.example.com
TLS_ENABLED=true          # adds the HSTS header
```

```sh
sudo systemctl restart belune.service
```

Verify:

```sh
curl -sSI https://belune.example.com/healthz | head -1   # 200 OK
```

**Wildcard certificates** (`*.example.com`) cannot be issued this way — Let's
Encrypt only issues wildcards over a DNS challenge, which Belune does not support.
Each app domain you add gets its own certificate automatically, which covers the
normal case. If you genuinely need a wildcard, issue it elsewhere and upload it
under **Settings → Certificates**, then set the domain's SSL mode to *Custom*.

---

## 6. Bootstrap the admin account

Visit `http://<host>` in a browser — the server's IP address. The first-run page
asks you to create an admin account. After that, login is required for everything.

Do this before step 5: the dashboard domain is configured from inside the
dashboard, so the very first login is over plain HTTP on the IP. Once HTTPS is
active you reach it at `https://belune.example.com` instead.

Useful CLI sanity checks:

```sh
docker compose -f /opt/belune/docker-compose.yml logs --tail 50 belune
docker compose -f /opt/belune/docker-compose.yml exec postgres \
    psql -U belune -d belune -c 'SELECT count(*) FROM users;'
```

---

## 6a. Configure SMTP (recommended)

SMTP unlocks three features: password-reset emails, user invitations, and alert
notifications. Without it the platform works but team onboarding requires the
admin to set passwords manually.

See [`smtp.md`](./smtp.md) for the full setup guide. Minimum required vars:

```env
PUBLIC_BASE_URL=https://belune.example.com
SMTP_HOST=smtp.example.com
SMTP_PORT=587
SMTP_USER=...
SMTP_PASSWORD=...
```

---

## 6b. Invite additional team members

Once SMTP is configured, invite team members from **Settings → Team → Invite
by Email**. They receive a link to set their own password — no manual password
distribution needed. Invitations expire after 7 days; resend from the same
Pending Invitations table if needed.

---

## 7. Sample deploy (smoke test)

Confirm the platform end-to-end with a static container:

1. Create a project named `demo`.
2. Add an application of type **Docker Image** with image `nginx:alpine`.
3. Add a domain on that app: `demo.example.com`.
4. Wait for the deployment to flip to **running** (green badge).
5. `curl -I https://demo.example.com` — expect `200 OK` from nginx and a
   valid TLS cert.

If anything sticks in `pending` or `building`, see
[`troubleshooting.md`](./troubleshooting.md).

---

## 8. Backups

The platform includes a built-in backup scheduler (in-app cron, configurable
under **Server → Backups**; default daily at 02:00) that runs natively in the
worker and enforces a rotation policy. No external cron job or systemd timer
is required. `scripts/backup.sh` remains available as a host/CLI equivalent
for manual runs and pre-update backups, producing the same archive format.

**Defaults** (active with no configuration):
- Daily local backup to `BELUNE_DIR/backups/` (i.e. `/opt/belune/backups/`).
- Keeps the last 14 backups **and** any backup newer than 30 days — whichever
  retains more.

**Enable remote (S3-compatible) upload** for off-host durability:

```env
BACKUP_REMOTE_ENABLED=true
BACKUP_S3_ENDPOINT=            # empty = AWS S3; or e.g. s3.us-west-004.backblazeb2.com
BACKUP_S3_REGION=us-east-1
BACKUP_S3_BUCKET=my-belune-backups
BACKUP_S3_ACCESS_KEY=...
BACKUP_S3_SECRET_KEY=...
```

The status of the last backup run — and a "Run Backup Now" trigger — is
visible in the dashboard under **Settings → Backups** (admin only).

Restore drill (do this at least once before you need it for real):
[`disaster-recovery.md`](./disaster-recovery.md).

---

## 9. Updating

```sh
cd /opt/belune
sudo bash scripts/update.sh
```

This pulls the latest pinned image (read from `BELUNE_IMAGE` in `.env`),
re-runs migrations, and waits for `/healthz`. The systemd unit doesn't need
to be restarted — `update.sh` only bounces the `belune` container.

---

## 10. Where to go next

- [`smtp.md`](./smtp.md) — configure outbound email for password-reset, invitations, and alerts.
- [`alerts.md`](./alerts.md) — per-user alert preferences (deploy/build failures, quota thresholds).
- [`key-rotation.md`](./key-rotation.md) — rotate `ENCRYPTION_KEY` without losing data.
- [`scaling.md`](./scaling.md) — vertical / horizontal headroom and resource caps.
- [`troubleshooting.md`](./troubleshooting.md) — first stop when something is wrong.
- [`disaster-recovery.md`](./disaster-recovery.md) — restore from backup.

## Upgrading from a release before request logs worked

Caddy creates its access log `0600` root-owned by default. Belune runs as a
non-root user and tails that file from its own container, so it could not read
it: request logging silently collected nothing. Belune now asks Caddy for mode
`0644`, but **Caddy only applies the mode when it creates the file** — an
existing log keeps its old permissions, so a one-time fix is needed on an
existing install:

```bash
docker exec belune-caddy-1 chmod 644 /var/log/caddy/access.log
```

Fresh installs need nothing. Verify with:

```bash
docker exec belune-postgres-1 psql -U belune -d belune -tAc "select count(*) from request_logs"
```

A count that climbs after a few page loads means the tailer is reading.
