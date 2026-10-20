# Security Policy

Belune deploys and runs other people's applications, holds database credentials,
and talks to the Docker daemon. Security reports are taken seriously, and
responsible disclosure is genuinely appreciated.

## Supported versions

Belune is pre-1.0. Only the **latest released version** receives security fixes.
There are no backports to older tags — please upgrade before reporting.

| Version | Supported |
|---|---|
| Latest release | ✅ |
| Anything older | ❌ |

## Reporting a vulnerability

**Please do not open a public issue for a security problem.**

Preferred: use GitHub's private vulnerability reporting —
[**Report a vulnerability**](https://github.com/belune-hq/belune/security/advisories/new)
under the repository's Security tab. This keeps the report private until a fix
is available and lets us coordinate a release together.

Alternative: email **liang2010@hotmail.my** with `[belune-security]` in the
subject line.

Helpful things to include:

- What the issue is, and what an attacker gains from it
- Affected version (shown on the Server page) and how Belune was installed
- Reproduction steps or a proof of concept
- Any logs or configuration relevant to the finding (**redact secrets** —
  tokens, API keys, certificates, and passwords)

## What to expect

Belune is maintained by one person, so these are best-effort commitments rather
than a contractual SLA:

- **Acknowledgement** within roughly 72 hours
- **An initial assessment** — including whether it is accepted as a
  vulnerability — within about a week
- **A fix released** as soon as practical, with severity driving urgency
- **Credit** in the release notes and advisory, unless you prefer to stay anonymous

There is no bug-bounty programme; this is an unfunded open-source project.

## Scope

In scope:

- The Belune control plane (API, workers, web UI)
- The installer and update scripts under `scripts/`
- Default configuration shipped in this repository (compose files, Caddy config)
- Privilege escalation, authentication or authorisation bypass, secret exposure,
  and tenant/project isolation failures

Out of scope:

- Vulnerabilities in applications *deployed by* Belune, or in third-party images
  referenced by app templates — report those upstream
- Issues that require an already-compromised host, or root access to the machine
  running Belune
- Findings that depend on deliberately insecure configuration documented as such
- Missing hardening headers or best-practice suggestions with no demonstrated
  impact — those are welcome as normal issues

## Known design decisions

These are deliberate and documented, not vulnerabilities:

- **Belune requires access to the Docker socket.** Docker socket access is
  equivalent to host root. Anyone who can administer Belune can, by design,
  affect the host it runs on. Treat administrator accounts accordingly.
- **The optional host shell** (Server → Maintenance, disabled by default) grants
  root on the host by design. It is admin-only, requires re-authentication, and
  every session is audit-logged.
- **Deploy hook URLs are bearer credentials.** Anyone holding the URL can trigger
  a deploy of that application. They are redacted from logs and can be
  regenerated or disabled at any time.
