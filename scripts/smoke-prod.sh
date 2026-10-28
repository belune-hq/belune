#!/usr/bin/env bash
#
# Production smoke drill.
#
# Boots the real production stack — the real image, the real Caddy, a non-root
# user in a container — and asserts that features are actually ALIVE, not merely
# that the code compiles.
#
# Why this exists: every bug found during the v0.0.29 VPS trial was invisible in
# development and only appeared in production, because dev runs the API on the
# host against a Vite dev server while production serves one binary as a non-root
# user in a container behind Caddy. Two of them — request logging, and reporting
# why a certificate failed — were entire features that had been silently dead in
# production since the day they shipped. Nothing failed. Nothing logged an error
# a human would see. They just quietly did nothing.
#
# So every assertion below is a bug that actually happened. Each one is annotated
# with what it caught. Do not "simplify" an assertion into checking that a
# container is running: a running container is exactly what all of these bugs
# looked like.
#
# Usage:
#   ./scripts/smoke-prod.sh              # builds the image, runs the drill
#   BELUNE_IMAGE=belune:trial ./scripts/smoke-prod.sh   # reuse an existing image
#   KEEP=1 ./scripts/smoke-prod.sh       # leave the stack up for poking at
#
# KEEP=1 is what scripts/smoke-browser.sh builds on: it runs this drill, then
# drives the surviving stack in a real browser.
set -euo pipefail

cd "$(dirname "$0")/.."

PROJECT=belunesmoke
IMAGE="${BELUNE_IMAGE:-belune:smoke}"
# Fixed path, not a temp file: infra/docker-compose.smoke.yml has to reference
# it statically in env_file, which cannot interpolate a random name.
ENV_FILE=".env.smoke"
# --project-directory pins relative paths in the compose files to the repo root.
# Without it Compose infers it from the first -f, i.e. infra/, and the drill stops
# testing the layout a real install actually has.
COMPOSE=(docker compose
  -p "$PROJECT"
  --project-directory .
  -f infra/docker-compose.prod.yml
  -f infra/docker-compose.smoke.yml
  --env-file "$ENV_FILE")

# The dashboard is reached through Caddy by Host header, so it needs a name. It
# never resolves in DNS and never should.
DASHBOARD_HOST=smoke.belune.internal
# An application domain, which is the only kind the access-log tailer records.
APP_HOST=app.smoke.internal
CADDY_URL=http://127.0.0.1:18080
ADMIN_URL=http://127.0.0.1:12019
# Belune reached directly, bypassing Caddy. Some checks must not go through the
# proxy: an app or dashboard hostname force-redirects HTTP→HTTPS (correct for a
# login form), which would mask what the check is actually about.
BELUNE_URL=http://127.0.0.1:18081

# The install's first admin, created through the real setup endpoint below.
#
# The password is generated per run and never leaves this process: the stack is
# bound to 127.0.0.1, torn down on the way out, and exported only to whatever
# this script hands off to (scripts/smoke-browser.sh). Overridable so a caller
# that needs to sign in can choose it.
ADMIN_EMAIL="${SMOKE_ADMIN_EMAIL:-admin@belune.invalid}"
ADMIN_PASSWORD="${SMOKE_ADMIN_PASSWORD:-$(openssl rand -hex 16)}"

# ⚠️ The version the image reports, and it has to be real semver.
#
# The Dockerfile defaults VERSION to "dev", and "dev" is not a version the
# dashboard can compare against — the Updates card hides itself entirely rather
# than offer an update it cannot reason about, and /api/version's consumers all
# treat it as "unstamped, do nothing". So a drill that wants to exercise
# anything version-shaped must stamp the build. Deliberately LOW, so a seeded
# "latest" is always newer whatever it is. The leading "v" matches what the
# release workflow stamps (`v0.1.16`, as a published image reports); both sides
# strip it before comparing, so it is convention rather than requirement.
#
# Only applies when this script does the build. A caller passing BELUNE_IMAGE
# owns its own build args — see the prod-smoke job in .github/workflows/ci.yml.
SMOKE_VERSION="${SMOKE_VERSION:-v0.1.0}"

PASS=0
FAIL=0

pass() { printf '  \033[32m✓\033[0m %s\n' "$1"; PASS=$((PASS + 1)); }
fail() { printf '  \033[31m✗\033[0m %s\n' "$1"; printf '      %s\n' "${2:-}"; FAIL=$((FAIL + 1)); }
info() { printf '\n\033[1m%s\033[0m\n' "$1"; }

cleanup() {
  local code=$?
  if [[ "${KEEP:-0}" == "1" ]]; then
    info "KEEP=1 — leaving the stack up. Tear down with:"
    echo "  docker compose -p $PROJECT --project-directory . -f infra/docker-compose.prod.yml -f infra/docker-compose.smoke.yml --env-file $ENV_FILE down -v"
    exit $code
  fi
  info "Tearing down"
  "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  rm -f "$ENV_FILE"
  exit $code
}
trap cleanup EXIT

# --- environment -------------------------------------------------------------

# The container must be in the group that owns the Docker socket, or it starts,
# serves the UI, and can do nothing at all. The GID differs per host, which is
# precisely why hardcoding 999 broke the first VPS deploy (it was 988).
if [[ "$(uname)" == "Darwin" ]]; then
  # Docker Desktop exposes the VM's socket as root:root inside containers; the
  # host-side path is a user-owned symlink and tells us nothing.
  DOCKER_GID=0
else
  DOCKER_GID="$(stat -c '%g' /var/run/docker.sock)"
fi

cat >"$ENV_FILE" <<EOF
BELUNE_IMAGE=$IMAGE
DOCKER_GID=$DOCKER_GID

POSTGRES_USER=belune
POSTGRES_PASSWORD=smoke
POSTGRES_DB=belune

DATABASE_URL=postgres://belune:smoke@postgres:5432/belune?sslmode=disable
REDIS_URL=redis://redis:6379
PORT=8080

JWT_SECRET=$(openssl rand -hex 32)
ENCRYPTION_KEY=$(openssl rand -hex 32)

CADDY_ADMIN_URL=http://caddy:2019
BUILDKIT_HOST=tcp://buildkit:1234
SECURE_COOKIES=false
EOF

# --- boot --------------------------------------------------------------------

if [[ -z "${BELUNE_IMAGE:-}" ]]; then
  info "Building $IMAGE (pass BELUNE_IMAGE=... to skip)"
  docker build --build-arg "VERSION=$SMOKE_VERSION" -t "$IMAGE" . >/dev/null
fi

# ⚠️ Clear anything a previous KEEP=1 run left behind, volumes included. Compose
# would happily reuse the containers, and a surviving database already has an
# admin — so the setup call below returns 409 and the drill fails on its own
# leftovers, several runs after the one that caused it.
"${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true

info "Starting the production stack"
# stderr is NOT suppressed: a compose failure here (a container that cannot even
# be created) is silent otherwise, and "it printed nothing and did nothing" is
# the exact failure mode this drill exists to catch.
"${COMPOSE[@]}" up -d --wait --wait-timeout 240 >/dev/null || {
  echo "stack did not come up:"
  "${COMPOSE[@]}" ps
  "${COMPOSE[@]}" logs --tail 40 belune 2>&1 || true
  exit 1
}

psql() { "${COMPOSE[@]}" exec -T postgres psql -U belune -d belune -tAc "$1" 2>/dev/null | tr -d '[:space:]'; }
belune_logs() { "${COMPOSE[@]}" logs belune 2>&1; }

# Point the dashboard at a hostname so Caddy has a route to serve.
#
# Also seed one application with a domain. This is not decoration: the access-log
# tailer deliberately skips any hostname that is not a known *application* domain
# ("requests to the PaaS app itself"), so hitting the dashboard alone can never
# produce a request_logs row and an assertion built on it would be testing
# nothing. ssl_mode=off keeps Caddy from chasing a certificate for a name that
# does not resolve.
#
# The admin comes first, and through the REAL setup endpoint rather than an
# INSERT. Two reasons. It exercises the first-admin bootstrap, which every
# install in existence runs exactly once and nothing else here covers — a broken
# setup wizard bricks a new install while every other assertion stays green. And
# it produces a usable password: an INSERT would have to carry a hash, and a
# fabricated one (this drill used to seed 'x') cannot be signed in with, so
# nothing could drive the dashboard.
info "Creating the first admin through POST /api/auth/setup"
setup_body="$(curl -fsS -X POST "$BELUNE_URL/api/auth/setup" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASSWORD\",\"username\":\"admin\",\"instance_name\":\"Smoke\"}" 2>&1)" || {
  echo "setup failed: $setup_body"
  echo "POST /api/auth/setup did not create the first admin — a new install cannot be bootstrapped."
  exit 1
}

"${COMPOSE[@]}" exec -T postgres psql -U belune -d belune -v ON_ERROR_STOP=1 -q <<SQL
INSERT INTO settings (key, value) VALUES ('dashboard_domain', '$DASHBOARD_HOST')
  ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value;

INSERT INTO projects (id, name, slug, user_id, server_id)
  VALUES ('22222222-2222-2222-2222-222222222222', 'smoke', 'smoke',
          (SELECT id FROM users WHERE email = '$ADMIN_EMAIL'),
          (SELECT id FROM servers WHERE is_local));
INSERT INTO applications (id, project_id, name, slug, type, build_type)
  VALUES ('33333333-3333-3333-3333-333333333333',
          '22222222-2222-2222-2222-222222222222', 'smoke', 'smoke', 'image', 'image');
INSERT INTO domains (id, application_id, hostname, ssl_mode, ssl_enabled)
  VALUES ('44444444-4444-4444-4444-444444444444',
          '33333333-3333-3333-3333-333333333333', '$APP_HOST', 'off', false);
SQL

# --- assertions --------------------------------------------------------------

info "1. The container is healthy"
# Caught: the Dockerfile HEALTHCHECK probed /health, but the route is /healthz —
# so the container was permanently "unhealthy" while working perfectly.
health="$("${COMPOSE[@]}" ps --format '{{.Service}} {{.Health}}' | grep '^belune ' | awk '{print $2}')"
[[ "$health" == "healthy" ]] \
  && pass "belune reports healthy (HEALTHCHECK hits a route that exists)" \
  || fail "belune health is '$health'" "the Dockerfile HEALTHCHECK is probing a route that does not exist"

info "2. Belune can reach the Docker daemon"
# Caught: the socket was never mounted in production, and later the group_add GID
# was wrong. Belune's entire job is managing containers; without this it serves
# the UI and can do nothing.
hz="$(curl -fsS "http://127.0.0.1:18081/healthz" 2>/dev/null || echo '{}')"
if grep -q '"docker"' <<<"$hz" && ! grep -qi '"docker":[^,}]*\(unhealthy\|error\|fail\)' <<<"$hz"; then
  pass "/healthz reports the Docker check passing"
else
  fail "/healthz does not show a healthy Docker check" "$hz"
fi

info "3. Caddy's catch-all reaches Belune"
# Caught: the catch-all route dialled localhost:8080 — which is Caddy itself, not
# Belune — so the dashboard was a connection refused behind a working proxy.
#
# An UNKNOWN host, so the request lands on the catch-all rather than a named
# route. The dashboard's own hostname now force-redirects HTTP→HTTPS (a login
# form has no business on plain HTTP once it has a certificate), which is a 301,
# not the proxied page this check is about. The catch-all carries no such
# redirect, so it still exercises the reverse_proxy to belune:8080.
body="$(curl -fsS -H "Host: unrouted.smoke.internal" "$CADDY_URL/" 2>/dev/null || true)"
grep -q 'id="root"' <<<"$body" \
  && pass "the catch-all reaches belune:8080 and serves the SPA" \
  || fail "Caddy's catch-all did not serve the SPA" "the catch-all upstream is probably wrong"

info "4. The served page has no inline script"
# Caught: an inline <script> in index.html was blocked outright by the API's own
# Content-Security-Policy (script-src 'self'), so it never ran in production —
# silently, with only a console violation no one was reading.
if grep -qE '<script[^>]*>[^<]+</script>' <<<"$body"; then
  fail "index.html contains an inline script" "the CSP (script-src 'self') will block it in production"
else
  pass "no inline script (nothing for the CSP to block)"
fi

info "5. Caddy listens on both :80 and :443"
# Caught: the Caddyfile adapter emits one server per port, so ':80, :443' became
# srv0=:443 + srv1=:80 — stranding every route added later, all of which address
# srv0 by name.
listen="$(curl -fsS "$ADMIN_URL/config/apps/http/servers/srv0/listen" 2>/dev/null || true)"
[[ "$listen" == *':80'* && "$listen" == *':443'* ]] \
  && pass "srv0 holds both listeners ($listen)" \
  || fail "srv0 does not listen on :80 and :443" "got: ${listen:-<nothing>}"

info "6. Certificate sync works against a stock Caddy"
# Caught: a production Caddy has no tls app at all, so listing certificates
# returned 400 rather than the empty set dev returned. SyncCertificates bailed
# before the write — custom certificates were never pushed, i.e. the whole
# feature was dead in production. Dev hid it, because local_certs creates a tls app.
if belune_logs | grep -q "failed to sync certificates"; then
  fail "the proxy reconciler cannot sync certificates" "$(belune_logs | grep -m1 'failed to sync certificates')"
else
  pass "no certificate-sync errors against a stock Caddy"
fi

info "7. Caddy is watched so certificate errors can be captured"
# Caught: the log collector only watched containers labelled managed-by=belune,
# and Caddy is not one (that label marks containers the cleanup worker may reap).
# So Caddy was never watched — and Caddy's log is where ACME failure reasons live.
# A domain whose certificate failed showed no reason at all.
#
# The proxy's lines are deliberately NOT persisted (they were 94% of
# container_logs with no reader). They feed the TLS status pipeline live through
# the collector's line hook instead. So the contract is two-sided: the collector
# attaches to the Caddy container as a system source, and stores nothing for it.
if belune_logs | grep 'source_type=system' | grep -qi caddy; then
  pass "the collector is watching Caddy (system source attached) — ACME reasons can reach the TLS pipeline"
else
  fail "the collector is not watching the Caddy container" "$(belune_logs | grep -m1 -i 'collector' || echo 'no collector log line')"
fi
sys="$(psql "SELECT count(*) FROM container_logs WHERE source_type = 'system'")"
[[ "${sys:-0}" -eq 0 ]] \
  && pass "proxy logs are not persisted — they feed the TLS pipeline, not the busiest table" \
  || fail "system logs are being persisted ($sys rows)" "perf(logs): stop persisting proxy logs looks reverted"

info "8. Request logging is alive"
# Caught twice. First, ACCESS_LOG_PATH defaulted to a repo-relative dev path that
# does not exist in the container. Then, once mounted, Caddy writes the log 0600
# root-owned and Belune runs as non-root — so every read failed with "permission
# denied", the tailer retried forever, and request_logs stayed empty. Both were
# silent: zero rows, no error a user would ever see.
# Hit the APPLICATION host: the tailer skips the dashboard's own hostname by
# design, so requests to it would never be recorded.
for _ in 1 2 3 4 5; do curl -sS -o /dev/null -H "Host: $APP_HOST" "$CADDY_URL/" 2>/dev/null || true; done
deadline=$((SECONDS + 60))
reqs=0
while [[ $SECONDS -lt $deadline ]]; do
  reqs="$(psql 'SELECT count(*) FROM request_logs')"
  [[ "${reqs:-0}" -gt 0 ]] && break
  sleep 3
done
if [[ "${reqs:-0}" -gt 0 ]]; then
  pass "requests reach request_logs ($reqs rows)"
else
  perm="$(belune_logs | grep -m1 'permission denied' || true)"
  fail "request_logs is empty after 5 requests" "${perm:-the access-log tailer is reading nothing}"
fi

# Caught by reading the file, not the tailer: a named Caddy log with no `include`
# receives EVERY logger, so ACME and TLS lines — and any module's own log line,
# such as a WAF rule match carrying the request URI and matched payload — sat in
# this world-readable, week-long file beside entries whose URI is redacted. The
# tailer skips them (no request.host), so request_logs looked perfectly healthy.
#
# An access entry is recognised by its LOGGER, not by having a "request" object:
# Caddy's error-log entries (http.log.error) carry one too, so keying on it would
# count a stray line as an access entry and report clean.
#
# The first count is the anti-vacuity guard: an empty or missing file has no stray
# lines either, and must not pass for a clean one.
ACCESS_ENTRY='"logger":"http.log.access.'
# grep FLAGS PATTERN over the file inside the proxy container, as a bare number.
# -F throughout: the patterns below contain [ ] ? and are meant literally.
count_in_access_log() {
  "${COMPOSE[@]}" exec -T caddy sh -c "grep $1 -- '$2' /var/log/caddy/access.log || true" 2>/dev/null | tr -d '[:space:]' || true
}
entries="$(count_in_access_log -cF "$ACCESS_ENTRY")"
stray="$(count_in_access_log -vcF "$ACCESS_ENTRY")"
if [[ "${entries:-0}" -eq 0 ]]; then
  fail "the access log holds no access entries" "request_logs has rows, so the file should too — is the check reading the right path?"
elif [[ "${stray:-0}" -eq 0 ]]; then
  pass "the access log holds only access entries ($entries), no other Caddy logger"
else
  fail "$stray non-access lines in the world-readable access log" \
    "$("${COMPOSE[@]}" exec -T caddy sh -c "grep -vF -m1 -- '$ACCESS_ENTRY' /var/log/caddy/access.log" 2>/dev/null | cut -c1-200)"
fi

# A request IS an access entry, so scoping the file cannot help here. The dashboard
# serves single-use secrets IN THE URL — the reset and invitation links carry
# ?token=<plaintext>, and the invitation lookup is a GET with it too — and the
# deploy-hook token is a path segment. Caddy records the request line verbatim, and
# a browser repeats the page URL as Referer on every request that page makes. The
# file is world-readable and kept for a week, and Caddy's own log (the container's
# stderr) held a second, unfiltered copy of every entry.
#
# A unique marker is sent through each shape, and must appear nowhere. Separately,
# the probes must be PRESENT in the file — redacted or not — which is the
# anti-vacuity guard: a request that never reached Caddy leaves no marker behind
# either, and would pass for a fix. It counts the probe by its path and not by
# its redacted form, so a regression is reported as a leak, not as a missing probe.
LEAK="smoke-secret-${RANDOM}${RANDOM}"
for path in "/reset-password?token=$LEAK" "/accept-invite?token=$LEAK" \
            "/api/auth/invitation?token=$LEAK" "/api/webhooks/deploy/$LEAK"; do
  curl -sS -o /dev/null -H "Host: $DASHBOARD_HOST" "$CADDY_URL$path" 2>/dev/null || true
done
curl -sS -o /dev/null -H "Host: $DASHBOARD_HOST" \
  -H "Referer: http://$DASHBOARD_HOST/reset-password?token=$LEAK" "$CADDY_URL/" 2>/dev/null || true

deadline=$((SECONDS + 30))
probed=0
hooked=0
while [[ $SECONDS -lt $deadline ]]; do
  probed="$(count_in_access_log -cF '?token=')"           # reset, accept-invite, invitation lookup, Referer
  hooked="$(count_in_access_log -cF '/api/webhooks/deploy/')"
  [[ "${probed:-0}" -ge 3 && "${hooked:-0}" -ge 1 ]] && break
  sleep 1
done
leaked_file="$(count_in_access_log -cF "$LEAK")"
caddy_log="$("${COMPOSE[@]}" logs --no-log-prefix caddy 2>&1 || true)"
leaked_stderr="$(printf '%s\n' "$caddy_log" | grep -cF -- "$LEAK" || true)"
access_in_stderr="$(printf '%s\n' "$caddy_log" | grep -cF -- "$ACCESS_ENTRY" || true)"
if [[ "${probed:-0}" -lt 3 || "${hooked:-0}" -lt 1 ]]; then
  fail "the probe requests never reached the access log (${probed:-0} ?token= entries, ${hooked:-0} deploy-hook entries; expected 3 and 1)" \
    "the redaction check would pass vacuously, so it was not trusted"
elif [[ -z "$caddy_log" ]]; then
  fail "could not read the proxy container's log" "the stderr check would pass vacuously"
else
  [[ "${leaked_file:-0}" -eq 0 ]] \
    && pass "no ?token= or deploy-hook secret reaches the world-readable access log ($probed probe entries, all redacted)" \
    || fail "$leaked_file access-log lines still carry a plaintext secret" \
       "$("${COMPOSE[@]}" exec -T caddy sh -c "grep -F -m1 -- '$LEAK' /var/log/caddy/access.log" 2>/dev/null | cut -c1-240)"
  [[ "${leaked_stderr:-0}" -eq 0 && "${access_in_stderr:-0}" -eq 0 ]] \
    && pass "the proxy's own log carries no access entries, so no second unfiltered copy of those URLs" \
    || fail "the proxy's own log holds ${access_in_stderr:-0} access entries, ${leaked_stderr:-0} with the plaintext secret" \
       "the default log must exclude the access logger"
fi

info "9. Nothing is silently failing in a retry loop"
# The failure mode this whole drill is about: a warning, on a loop, that no one
# reads, while a feature does nothing.
noisy="$(belune_logs | grep -ciE 'permission denied|no such file or directory|tailer error' || true)"
[[ "${noisy:-0}" -eq 0 ]] \
  && pass "no tailer/permission/missing-file errors in the log" \
  || fail "$noisy tailer/permission/missing-file errors in the log" "$(belune_logs | grep -m1 -iE 'permission denied|no such file or directory|tailer error')"

info "10. Deep links and refreshes work"
# Caught: the SPA fallback rewrote the request path to /index.html and handed it
# to http.FileServer — which redirects anything ending in /index.html to "./".
# A refresh on /server bounced to / (and the router then sent the user to
# /projects); a deep path bounced to its own parent, forever:
#   /projects/x/applications/y → ./ → /projects/x/applications/ → ./ → …
# which the browser reports as TOO_MANY_REDIRECTS. Vite hides this in dev by
# doing the fallback itself.
# Probed directly against Belune, not through Caddy. Through the proxy the
# dashboard hostname force-redirects HTTP→HTTPS (a 301 that is correct, and a
# different one than this guards against). The fallback bug lives in Belune's own
# handler, so that is where the shell must appear.
for route in /server /projects/abc-123/applications/def-456 /docker; do
  code="$(curl -s -o /dev/null -w '%{http_code}' "$BELUNE_URL$route")"
  if [[ "$code" == "200" ]]; then
    pass "GET $route → 200 (no redirect)"
  else
    fail "GET $route → $code" "the SPA fallback is redirecting instead of serving the shell"
  fi
done
# ...and a real asset must still be served as itself, not swallowed by the fallback.
asset="$(curl -s "$BELUNE_URL/" | grep -o '/assets/[^"]*\.js' | head -1)"
ctype="$(curl -s -o /dev/null -w '%{content_type}' "$BELUNE_URL$asset")"
[[ "$ctype" == *javascript* ]] \
  && pass "assets are still served as assets ($asset)" \
  || fail "asset served as '$ctype'" "the fallback is swallowing real files"

info "11. Belune finds the Caddy container without being told its name"
# Caught on the first real install. Compose names containers after the project,
# which is the directory holding the compose file: infra/ in this repo, but the
# install directory (belune-caddy-1) on a real install. CADDY_CONTAINER_NAME was
# baked to the repo's guess, so every real install missed — and missing is silent,
# because failing to attach Caddy to a project network only logs a warning. The
# symptom is every app domain answering 502 on a stack that looks perfectly
# healthy. This drill used to set the variable itself, which is precisely why it
# never caught it; the variable is now deliberately absent from the drill's env.
# The console log format puts the message and its structured fields on separate
# lines, so the name= field lands one line below "discovered Caddy container".
# grep -A1 spans the wrap; matching them on one line never can.
expected="${PROJECT}-caddy-1"
if belune_logs | grep -A1 "discovered Caddy container" | grep -q "${expected}"; then
  pass "Caddy resolved by the belune-system label → ${expected}"
elif belune_logs | grep -q "no container carries belune-system=caddy"; then
  fail "Caddy was not discovered" "the belune-system=caddy label is missing from the proxy — app networks will never be joined"
else
  fail "no Caddy discovery log line" "$(belune_logs | grep -m1 -i caddy || echo 'nothing mentioning caddy')"
fi

info "12. An admin can actually sign in"
# New coverage rather than a past bug: everything above probes the API and the
# proxy, and none of it would notice that the dashboard is unreachable to a
# human. It is also the precondition the browser drill depends on, so a broken
# session or cookie fails attributably here instead of as "the login form did
# not go away".
#
# SECURE_COOKIES=false is in the drill's env, which is what makes a session over
# plain HTTP work at all.
login="$(curl -fsS -X POST "$BELUNE_URL/api/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASSWORD\"}" 2>&1 || true)"
if grep -q '"email"' <<<"$login"; then
  pass "the first admin can sign in (the dashboard is reachable to a human)"
else
  fail "POST /api/auth/login did not return a session" "$login"
fi

info "13. The build reports a real version"
# ⚠️ Not cosmetic. "dev" is the Dockerfile's default and means "unstamped", and
# the dashboard treats it as "nothing to compare against" — the Updates card
# hides itself, so an unstamped image makes every update-shaped drill silently
# pass by having nothing to click. Attributable here, once, rather than as a
# missing button in a browser test.
# A released image reports "v0.1.16", WITH the prefix — matched optionally
# rather than demanded, since the prefix is the release workflow's convention
# and every consumer strips it.
ver="$(curl -fsS "$BELUNE_URL/api/version" 2>/dev/null || echo '{}')"
if grep -qE '"version":"v?[0-9]' <<<"$ver"; then
  pass "/api/version reports a stamped version ($(sed -n 's/.*"version":"\([^"]*\)".*/\1/p' <<<"$ver"))"
else
  fail "the image reports no stamped version" "got: $ver — pass --build-arg VERSION=<semver> when building it"
fi

info "14. CrowdSec: the local API, the bouncer key and a ban round-trip"
# New coverage rather than a past bug, and the only place it can be had: everything
# Belune does with CrowdSec is `cscli` run inside the crowdsec container, found by
# its belune-system label, against a data volume the image's entrypoint refuses to
# start without. A mock proves none of that, and a compose service that exits 0
# quietly looks exactly like a working one.
#
# The ban goes to a documentation address (TEST-NET-3), which is public as far as
# Belune's guard is concerned, so the guard is not what is being tested here.
cs_health="$("${COMPOSE[@]}" ps --format '{{.Service}} {{.Health}}' | grep '^crowdsec ' | awk '{print $2}')"
[[ "$cs_health" == "healthy" ]] \
  && pass "crowdsec reports healthy (the entrypoint started the local API on its data volume)" \
  || fail "crowdsec health is '${cs_health:-<no such service>}'" \
       "$("${COMPOSE[@]}" logs --tail 15 crowdsec 2>&1 | cut -c1-200)"

token="$(sed -n 's/.*"token":"\([^"]*\)".*/\1/p' <<<"${login:-}")"
cs_api() { curl -sS -H "Authorization: Bearer $token" -H 'Content-Type: application/json' -H 'X-Forwarded-For: 198.51.100.77' "$@"; }
cs_exec() { "${COMPOSE[@]}" exec -T crowdsec cscli "$@" 2>/dev/null; }

# Turning it on is what registers the bouncer, so this is the key flow in a real
# container: the key is generated by CrowdSec, stored sealed, and handed to Caddy.
cs_on="$(cs_api -X PUT "$BELUNE_URL/api/maintenance/proxy/crowdsec" -d '{"enabled":true}' || true)"
if grep -q '"enabled":true' <<<"$cs_on"; then
  pass "the CrowdSec switch can be turned on"
else
  fail "PUT /api/maintenance/proxy/crowdsec did not enable it" "${cs_on:-<no response>}"
fi

# The module is a property of the published Caddy image, not of this repo: if it
# were missing, every other CrowdSec assertion could pass and nothing would ever
# be enforced.
if grep -q '"module":"http.handlers.crowdsec","state":"available"' <<<"$cs_on"; then
  pass "the Caddy image carries the CrowdSec bouncer module"
else
  fail "Caddy does not report http.handlers.crowdsec as available" "$(grep -o '"capability":{[^}]*}' <<<"$cs_on")"
fi

registered=""
for _ in $(seq 1 10); do
  registered="$(cs_exec bouncers list -o json | grep -c '"name": "belune-caddy"' || true)"
  [[ "${registered:-0}" -gt 0 ]] && break
  sleep 2
done
[[ "${registered:-0}" -gt 0 ]] \
  && pass "Belune registered its bouncer in the local API (the key never passed through a command line)" \
  || fail "no belune-caddy bouncer in the local API" "$(cs_exec bouncers list -o json | cut -c1-200)"

ban_code="$(cs_api -o /dev/null -w '%{http_code}' -X POST "$BELUNE_URL/api/maintenance/proxy/crowdsec/decisions" \
  -d '{"target":"203.0.113.9","duration":"5m","reason":"smoke drill"}' || true)"
if [[ "$ban_code" == "201" ]] && cs_exec decisions list -o json | grep -q '203.0.113.9'; then
  pass "a ban placed through the API is in the local API"
else
  fail "ban round-trip failed (HTTP $ban_code)" "$(cs_exec decisions list -o json | cut -c1-200)"
fi

unban_code="$(cs_api -o /dev/null -w '%{http_code}' -X DELETE "$BELUNE_URL/api/maintenance/proxy/crowdsec/decisions?target=203.0.113.9" || true)"
if [[ "$unban_code" == "204" ]] && ! cs_exec decisions list -o json | grep -q '203.0.113.9'; then
  pass "lifting the ban removes it from the local API"
else
  fail "unban round-trip failed (HTTP $unban_code)" "$(cs_exec decisions list -o json | cut -c1-200)"
fi

# A forged X-Forwarded-For on a connection that BYPASSES Caddy must not reach the
# allowlist.
#
# This used to assert the opposite: that the forged address landed in the allowlist.
# BELUNE_URL is the API's published port, not Caddy, so the request has no proxy in
# front of it, and it passed only because Belune believed a header the client wrote.
# That was the bug, recorded as expected behaviour: the sink is the allowlist, which
# decides who cannot be banned. The fix made it stop being true, so the assertion is
# inverted and is now a regression test for the fix on a real stack.
#
# What is NOT asserted here, deliberately: "an admin login allowlists the address it
# came from". The runner's addresses are all private or loopback, which CheckBannable
# refuses on purpose, so the property cannot honestly be tested here; it is drilled on
# a public host, with the autoban drill. And the header is not "fixed" by sending
# X-Belune-Client-IP: this request comes from the host and not from Caddy, so it would
# not be believed, believing it would mean asserting that a client chooses its own
# address, and it would train the only end-to-end drill on the forgeable path.
login_code="$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$BELUNE_URL/api/auth/login" \
  -H 'Content-Type: application/json' -H 'X-Forwarded-For: 198.51.100.88' \
  -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASSWORD\"}" || true)"
# Allowlisting happens after the response, so give it the time it would have needed
# to appear; an absence checked at once would pass whatever the code did.
sleep 5
allowed="$(cs_exec allowlists list -o json | grep -c '198.51.100.88' || true)"
if [[ "$login_code" == "200" && "${allowed:-0}" -eq 0 ]]; then
  pass "a forged X-Forwarded-For on a connection that bypasses Caddy does not reach the CrowdSec allowlist"
else
  fail "the forged address reached the allowlist, or the login did not succeed (HTTP $login_code)" \
    "$(cs_exec allowlists list -o json | cut -c1-300)"
fi

# --- result ------------------------------------------------------------------

info "Result"
printf '  %d passed, %d failed\n' "$PASS" "$FAIL"

if [[ "$FAIL" -ne 0 ]]; then
  # Dump the logs here rather than from CI: the cleanup trap tears the stack down
  # on the way out, so anything that wants to look at it has to look now.
  info "belune logs (last 60 lines)"
  belune_logs | tail -60
  exit 1
fi
echo
