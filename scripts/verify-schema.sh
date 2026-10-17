#!/usr/bin/env bash
#
# Compare a live database against what the migrations actually produce.
#
# Why this exists: `cmd/migrate-check` proves every migration applies to a
# FRESH database. It cannot see the other failure — a migration file edited
# AFTER it was already applied somewhere. golang-migrate records only the
# version number, so an edited file never re-runs, `schema_migrations` reads
# clean, and the database silently lacks whatever the edit added.
#
# That is not hypothetical. A dev database applied 000068 before `picked_up_at`
# was added to it; `schema_migrations` said `68, dirty=f`; and every request to
# the scheduled-tasks endpoint answered 500, because sqlc expands `SELECT r.*`
# into explicit column names at generation time and Postgres rejected the
# column that was missing. Nothing in the stack could report that: the only
# honest way to find it is to build the schema the migrations describe and
# compare, which is what this does.
#
# ⚠️ Editing an already-applied migration is only safe BEFORE release. Once a
# version ships, its migration is immutable — see "Dropping or restructuring
# existing data" in CLAUDE.md. This script is how you find out that the rule
# was broken, on your own machine, rather than from an operator's 500.
#
# Usage:
#   ./scripts/verify-schema.sh              # check $DATABASE_URL
#   ./scripts/verify-schema.sh "postgres://user:pass@host:5432/db?sslmode=disable"
#
# Exit 0 when the live schema matches the migrations, 1 when it does not.
set -euo pipefail

LIVE_URL="${1:-${DATABASE_URL:-}}"
if [ -z "${LIVE_URL}" ]; then
  echo "No database to check: pass one as an argument or set DATABASE_URL." >&2
  exit 1
fi

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REF_CONTAINER="belune-schema-ref-$$"
# A port nobody is likely to be serving on, so this never collides with a dev
# stack's Postgres on 5432.
REF_PORT="${BELUNE_SCHEMA_REF_PORT:-55433}"
WORK="$(mktemp -d)"

cleanup() {
  docker rm -f "${REF_CONTAINER}" >/dev/null 2>&1 || true
  rm -rf "${WORK}"
}
trap cleanup EXIT

# The reference image must match what Belune actually runs; a different major
# version would report differences that are Postgres's, not ours.
REF_IMAGE="${BELUNE_POSTGRES_IMAGE:-postgres:16-alpine}"

echo "Building a reference schema from the migrations (${REF_IMAGE})…"
docker run -d --rm --name "${REF_CONTAINER}" \
  -e POSTGRES_USER=belune -e POSTGRES_PASSWORD=belune -e POSTGRES_DB=belune \
  -p "127.0.0.1:${REF_PORT}:5432" "${REF_IMAGE}" >/dev/null

for _ in $(seq 1 60); do
  docker exec "${REF_CONTAINER}" pg_isready -U belune -q 2>/dev/null && break
  sleep 1
done
docker exec "${REF_CONTAINER}" pg_isready -U belune -q 2>/dev/null || {
  echo "The reference database never became ready." >&2
  exit 1
}

( cd "${HERE}/apps/api" && \
  DATABASE_URL="postgres://belune:belune@localhost:${REF_PORT}/belune?sslmode=disable" \
  go run ./cmd/migrate-check >/dev/null )

# Columns and indexes are what actually break a query. Compared as sorted sets,
# so physical column ORDER is deliberately ignored: sqlc names every column
# explicitly, so a column appended by ALTER TABLE is as good as one declared in
# the middle.
COLUMNS_Q="SELECT table_name||'.'||column_name||' '||data_type
           FROM information_schema.columns
           WHERE table_schema='public' ORDER BY 1;"
INDEXES_Q="SELECT indexdef FROM pg_indexes WHERE schemaname='public' ORDER BY 1;"

dump_ref() { docker exec "${REF_CONTAINER}" psql -U belune -d belune -At -c "$1"; }
dump_live() { psql "${LIVE_URL}" -At -c "$1"; }

if ! command -v psql >/dev/null 2>&1; then
  # No local psql: borrow the reference container's client, which is the right
  # major version by construction.
  #
  # ⚠️⚠️ But rewrite the host first. Inside that container `localhost` is the
  # REFERENCE Postgres, so a URL pointing at localhost made this script compare
  # the reference against itself — reporting a perfect match for a database it
  # had never looked at. A verification tool that cannot fail is worse than
  # none, because it is believed.
  CONTAINER_URL="${LIVE_URL}"
  case "${LIVE_URL}" in
    *@localhost:*|*@127.0.0.1:*|*@[::1]:*)
      CONTAINER_URL="$(printf '%s' "${LIVE_URL}" \
        | sed -e 's|@localhost:|@host.docker.internal:|' \
              -e 's|@127\.0\.0\.1:|@host.docker.internal:|' \
              -e 's|@\[::1\]:|@host.docker.internal:|')"
      echo "note: no local psql; reaching the live database as ${CONTAINER_URL##*@}"
      ;;
  esac
  dump_live() { docker exec -i "${REF_CONTAINER}" psql "${CONTAINER_URL}" -At -c "$1"; }
fi

dump_ref  "${COLUMNS_Q}" | sort > "${WORK}/ref-cols"
dump_live "${COLUMNS_Q}" | sort > "${WORK}/live-cols"
dump_ref  "${INDEXES_Q}" | sort > "${WORK}/ref-idx"
dump_live "${INDEXES_Q}" | sort > "${WORK}/live-idx"

status=0
report() {
  local label="$1" ref="$2" live="$3"
  local missing extra
  missing="$(comm -23 "${ref}" "${live}")"
  extra="$(comm -13 "${ref}" "${live}")"
  if [ -n "${missing}" ]; then
    status=1
    echo
    echo "❌ ${label} the migrations create but your database does NOT have:"
    echo "${missing}" | sed 's/^/     /'
  fi
  if [ -n "${extra}" ]; then
    status=1
    echo
    echo "⚠️  ${label} your database has but the migrations do NOT create:"
    echo "${extra}" | sed 's/^/     /'
  fi
}

echo
echo "reference: $(wc -l < "${WORK}/ref-cols" | tr -d ' ') columns, $(wc -l < "${WORK}/ref-idx" | tr -d ' ') indexes"
echo "live:      $(wc -l < "${WORK}/live-cols" | tr -d ' ') columns, $(wc -l < "${WORK}/live-idx" | tr -d ' ') indexes"

report "Columns" "${WORK}/ref-cols" "${WORK}/live-cols"
report "Indexes" "${WORK}/ref-idx" "${WORK}/live-idx"

echo
if [ "${status}" -eq 0 ]; then
  echo "✅ The live schema matches the migrations."
else
  cat <<'EOF'
The usual cause is a migration edited after it had already been applied here.
golang-migrate records only the version number, so the edit never re-runs.

To repair a DEV database, apply the difference by hand (ADD COLUMN IF NOT
EXISTS / CREATE INDEX IF NOT EXISTS), or recreate it from scratch.

⛔ If the migration in question has already SHIPPED, do not edit it further —
every install that ran the old version has the same gap, and the fix belongs in
a NEW forward-only migration.
EOF
fi
exit "${status}"
