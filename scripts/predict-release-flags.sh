#!/usr/bin/env bash
#
# Predict what release.yml will derive, BEFORE the tag exists.
#
# Prints the release range, and the two flags that go into versions.json:
# `breaking` and `requires_host_update`. Run it before tagging, compare, and
# only then tag.
#
# Why this exists: both flags are DERIVED from the commit range, nobody writes
# them by hand, and a wrong one is only visible after the tag is pushed — at
# which point the manifest PR is already open with the wrong value in it.
# `breaking` has shipped wrong twice (v0.1.12 and v0.1.20, both a squash subject
# that lost its `!`), and `requires_host_update` has never been wrong for a
# reason worth knowing: a release with rcs AND an infra change has never
# happened, so the one scenario that can break it has never been exercised.
# "It has always been right" is luck, not evidence. 0.2.0 is the first release
# where both conditions hold at once.
#
# The range matters as much as the flags, because the flags are computed FROM
# it. A plain `git describe` on a release line that had rcs resolves to the last
# rc and truncates the range to the handful of commits since it — which makes
# the notes nearly empty and both flags derive against almost no diff. That is
# what the --exclude below prevents, and this script prints both forms so a
# divergence is visible rather than assumed.
#
# ⛔ The paths and the pattern below are copied from release.yml and must stay
# identical to it. Do not "tidy" them: a predictor that disagrees with the
# workflow is worse than no predictor, because it is believed.
#
# Usage:
#   ./scripts/predict-release-flags.sh                 # HEAD against the last stable tag
#   ./scripts/predict-release-flags.sh v0.2.0          # as if this tag existed at HEAD
#   ./scripts/predict-release-flags.sh v0.2.0 abc1234  # ...at a specific commit
set -euo pipefail

TIP="${2:-HEAD}"
TAG="${1:-}"

# release.yml:141 and :272, verbatim. --match keeps non-release tags out (there
# is a `belune-hoist-archive`); --exclude drops both the `-rc` and the archived
# `-alpha` lines, because a hyphen is what every prerelease here has in common.
# ⚠️ The caret is load-bearing and is why this mirrors "${TAG}^" rather than
# "${TAG}". Without it, pointing this at an EXISTING tag describes that tag as
# itself (v0.1.21 -> v0.1.21), yielding an empty range and two meaningless
# flags. With it, both modes work: an untagged HEAD still resolves back to the
# last stable, and an existing tag resolves to its predecessor.
PREV=$(git describe --tags --abbrev=0 --match 'v[0-9]*' --exclude '*-*' "${TIP}^" 2>/dev/null || true)
# The pre-fix form, shown only so a divergence is visible.
PREV_NAIVE=$(git describe --tags --abbrev=0 --match 'v[0-9]*' "${TIP}^" 2>/dev/null || true)

if [ -z "${PREV}" ]; then
  echo "No stable tag is an ancestor of ${TIP}."
  echo "release.yml would exit 1 here rather than compute against the whole of history."
  exit 1
fi

RANGE="${PREV}..${TIP}"
echo "tag being predicted : ${TAG:-<none given>}"
echo "previous stable     : ${PREV}"
if [ "${PREV_NAIVE}" != "${PREV}" ]; then
  echo "  ⚠️  a plain git describe would say '${PREV_NAIVE}' — the --exclude IS load-bearing here"
fi
echo "range               : ${RANGE}  ($(git rev-list --count "${RANGE}") commits)"
echo

# ── requires_host_update ──────────────────────────────────────────────────────
# release.yml:292. Means an in-place `docker compose up -d` is not enough —
# the compose file or a systemd unit itself moved.
if git diff --quiet "${RANGE}" -- infra/docker-compose.prod.yml infra/systemd/; then
  echo "requires_host_update = false"
else
  echo "requires_host_update = TRUE   changed:"
  git diff --name-only "${RANGE}" -- infra/docker-compose.prod.yml infra/systemd/ | sed 's/^/    /'
fi

# ── breaking ──────────────────────────────────────────────────────────────────
# A Conventional Commits `!` before the colon, in a subject line. ⚠️ Only as
# good as the subject it reads: a breaking change whose squash subject lost its
# `!` derives false no matter how correct the range is. That is how v0.1.12 and
# v0.1.20 both shipped the wrong flag. If this prints false on a release that
# moves or removes a documented path, the subject is wrong, not this check.
BREAKING=$(git log --format='%s' "${RANGE}" | grep -E '^[a-z]+(\([^)]*\))?!:' || true)
if [ -n "${BREAKING}" ]; then
  echo "breaking             = TRUE   from:"
  echo "${BREAKING}" | sed 's/^/    /'
else
  echo "breaking             = false  (no subject carries a ! before the colon)"
  echo "    ⚠️  if this release moves or removes a documented path, the subject lost its ! —"
  echo "        fix the flag by hand in the manifest PR, as v0.1.12 and v0.1.20 needed."
fi
