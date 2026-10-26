# CrowdSec Runbook

How bans work in Belune, what to do when one locks someone out, and what to check
after an upgrade that adds the CrowdSec service.

---

## How it fits together

```
dashboard / API ── cscli (docker exec) ──▶ crowdsec container (local API, SQLite)
                                                     ▲
                                  ban list, every 5s │ (bouncer key)
                                                     │
visitor ──▶ Caddy ── http.handlers.crowdsec ─────────┘ ──▶ 403, or on to the app
```

- **One container, `crowdsec`**, running the local API only. The log-parsing agent
  (`DISABLE_AGENT`) and the central API / community blocklist
  (`DISABLE_ONLINE_API`) are off, so nothing is banned that an admin or an admin's
  own policy did not ban, and nothing leaves the host.
- **Belune writes bans by running `cscli` in that container**, found by its
  `belune-system=crowdsec` label. It holds no machine password; the only secret it
  stores is the bouncer key (`crowdsec_config.bouncer_key_encrypted`, rewrapped by
  `cmd/rewrap`), which can read the ban list and nothing else.
- **Caddy's bouncer** is the `crowdsec` app plus one route (`route-crowdsec`) in
  front of every application route, matching application hostnames only. **The
  dashboard's hostname is not in it**, so a ban can always be lifted from the
  dashboard.
- It is the HTTP bouncer, never the firewall bouncer: a ban cannot touch SSH.

## What cannot be banned

| | Why |
|---|---|
| A public address an admin has logged in from, or turned CrowdSec on from, while its allowlist entry stands (30 days, restarted by each login) | CrowdSec refuses to ban an allowlisted address or a range containing one, and adding an address to the allowlist deletes a ban already on it |
| The address of whoever is placing the ban | Refused by Belune before it reaches CrowdSec |
| Private, loopback, link-local, multicast and reserved ranges | Behind Docker the address Caddy sees may be a gateway or proxy that every visitor shares |
| Ranges wider than /16 (IPv4) or /48 (IPv6) | A typo should not ban a continent |

Every ban expires (1 minute to 365 days). Belune never passes `--bypass-allowlist`.

## When a ban locks someone out

If you can reach the dashboard: **Proxy → CrowdSec → Bans → Lift.** Lifting never
requires the feature to be on, and takes effect within about five seconds.

If you cannot reach the dashboard at all (for example a proxy or CDN in front of
Belune shares one address with every visitor and it got banned), connect to the
server over SSH. **The host shell in the dashboard does not help**: reaching it
needs the login you cannot reach.

```bash
cd /opt/belune                                  # the install directory

# lift every ban
docker compose exec crowdsec cscli decisions delete --all

# or one address / range
docker compose exec crowdsec cscli decisions delete --ip 203.0.113.9
docker compose exec crowdsec cscli decisions delete --range 203.0.113.0/24

# see what is banned, and what is allowlisted
docker compose exec crowdsec cscli decisions list
docker compose exec crowdsec cscli allowlists inspect belune-admins

# allowlist an address so it cannot be banned again (also lifts a ban on it)
docker compose exec crowdsec cscli allowlists add belune-admins 203.0.113.9 -d "me" -e 720h
```

To take the bouncer out of Caddy without the dashboard (Belune removes it within 30
seconds):

```bash
docker compose exec postgres psql -U belune -d belune \
  -c "UPDATE crowdsec_config SET enabled = false"
```

Stopping the `crowdsec` container does **not** end enforcement: Caddy keeps the
bans it already holds in memory.

## What the page means

| Pill | Meaning |
|---|---|
| **Off** | The switch is off. Bans already in CrowdSec are not enforced. |
| **Enforcing** | The route is in Caddy and the bouncer fetched the ban list in the last 90 seconds. |
| **Not enforcing** | The route is in Caddy, but the bouncer is not registered or has not fetched the list for over 90 seconds. Caddy **fails open**: it is admitting everyone. Use **Rotate the bouncer key**. |
| **Local API down** | On, but the `crowdsec` container is not answering. Nothing is written to Caddy until it is. After an upgrade, run the host update so the service exists. |
| **Waiting for an application** | On, and no application has a domain yet. |
| **Cannot tell yet** | Belune could not list Caddy's modules. The switch is saved; it applies once Belune can ask. This is not "your image lacks the module". |

## Failure behaviour, as measured

Measured against Caddy v2.11.7 with `caddy-crowdsec-bouncer` v0.14.1 and CrowdSec
v1.8.1:

- **Local API down, Caddy untouched:** the bouncer keeps enforcing the bans it holds
  and admits everyone else. No request fails.
- **Local API down, Caddy's config changes** (any deploy or domain change does this):
  the bouncer restarts **empty**, so no ban is enforced until the local API answers.
- **Wrong or missing key:** Caddy accepts the config, logs errors, and enforces
  nothing.
- **`enable_hard_fails` is written as `false` explicitly.** If it were true, an
  unreachable local API would make Caddy quit.
- A ban takes effect at the bouncer's next poll, at most 5 seconds.
- `X-Forwarded-For` from a visitor does not change the address a ban applies to.

## Automatic bans from the WAF

Off by default and independent of the CrowdSec switch. A WAF detection is only ever
a ban when the operator has turned the policy on. The policy counts **distinct
requests** (not rule matches) in the attack categories (`attack-sqli`,
`attack-xss`, `attack-rce`, `attack-lfi`, `attack-rfi`, `attack-injection-php`,
`attack-injection-java`, and optionally `attack-ssrf`, `attack-ssti`) over a window,
bans for a fixed time, and stops at a cap of automatic bans in force.

`attack-protocol` is never eligible: its rules are CRITICAL like real attacks, and
CRS rule 920420 ("request content type is not allowed by policy") fires on ordinary
`text/plain` POSTs. The category list is enforced by a CHECK constraint on
`crowdsec_config.autoban_categories` as well as in code.

An address an admin lifts **from the dashboard** is not banned again for the length of the window (the audit log is how Belune knows). A ban lifted with `cscli` on the host is not recorded, so allowlist the address too. Every
automatic ban is in the audit log as `crowdsec_autoban`, with no user.

## After upgrading to a release that adds the service

The release changes `docker-compose.yml`, so `update.sh` runs a full
`docker compose up -d` and the stack restarts briefly. Check, on the host:

```bash
docker compose ps crowdsec                      # healthy
docker compose exec crowdsec cscli lapi status
docker compose exec crowdsec cscli bouncers list   # belune-caddy, once turned on
docker compose exec crowdsec wget -qO- http://127.0.0.1:8080/health
```

and in the dashboard, with CrowdSec on: the pill reads **Enforcing**, and a ban on a
test address (for example `203.0.113.9`) shows up in the table and, from a machine
with that address, returns 403 on an application hostname within about five seconds.
The same address reaches the dashboard.
