#!/usr/bin/env bash
# Belune — Updater launcher
#
# Usage, from anywhere:
#   bash update.sh              # update to the latest release
#   bash update.sh v0.2.0       # update to a specific version
#
# This does NOT do the update. It pulls the target image and runs that image's
# /usr/local/bin/belune-update in a helper container — the same thing the
# dashboard does (SpawnUpdateHelper). Two reasons it is only a launcher:
#   - The updater that runs is the TARGET version's, not whatever copy is lying
#     on this host, so a fix to it protects the update that installs it.
#   - "Is an update running?" has one answer: a container labelled
#     belune-update. An update done by this script itself would be invisible
#     to the dashboard.
#
# Keep this file as dumb as possible: it is replaced during the update it
# launches, and an older copy of it runs on every future update.
set -euo pipefail

# ⚠️ Everything lives inside main() and the file ends with `main "$@"; exit $?`
# on ONE line. Do not "tidy" either. The updater replaces this very file
# (scripts/update.sh is an infra file) while this bash is still waiting on it;
# bash reads a script incrementally by byte offset, so an overwrite would make
# it resume mid-content and run garbage. A function body is parsed in full
# before it runs, and `exit` on the same line means the file is never read again.
main() {
  INSTALL_DIR="${BELUNE_DIR:-/opt/belune}"
  # IMAGE_REPO is the GHCR image path; SOURCE_REPO is the GitHub repo release
  # metadata comes from. Same string today, separate because they move
  # independently: a GHCR package belongs to the ACCOUNT, not the repository, so
  # an org transfer relocates the repo and leaves the package behind. Keep them
  # in step with imageRepo/sourceRepo in cmd/update/update.go.
  IMAGE_REPO="weiliang79/belune"
  SOURCE_REPO="weiliang79/belune"

  die() { echo "  [err]   $*" >&2; exit 1; }

  [[ -f "${INSTALL_DIR}/docker-compose.yml" ]] || \
    die "No docker-compose.yml found at ${INSTALL_DIR}. Is Belune installed?"

  TARGET_VERSION="${1:-}"
  if [[ -z "${TARGET_VERSION}" ]]; then
    echo "  [info]  Resolving the latest release..."
    TARGET_VERSION=$(curl -sSfL "https://api.github.com/repos/${SOURCE_REPO}/releases/latest" \
      | grep '"tag_name"' | head -1 | cut -d'"' -f4 || true)
    [[ -n "${TARGET_VERSION}" ]] || die "Could not resolve the latest release. Pass a version: bash update.sh v0.2.0"
  fi
  # Git tags carry a leading v, image tags do not.
  TARGET_VERSION="${TARGET_VERSION#v}"
  TARGET_IMAGE="ghcr.io/${IMAGE_REPO}:${TARGET_VERSION}"

  echo "  [info]  Pulling ${TARGET_IMAGE}..."
  docker pull "${TARGET_IMAGE}" || die "Could not pull ${TARGET_IMAGE}. Does that version exist?"

  # Attached, unlike the dashboard's detached helper, so the operator sees the
  # output; -i/-t only on a terminal so the no-backup prompt can be answered.
  # Everything else mirrors SpawnUpdateHelper (update_helper.go): root, host
  # network, the install dir at the same path, the Docker socket — and the
  # labels. ⚠️ The empty compose labels are load-bearing: an image built by
  # `docker compose build` carries project/service labels, and without
  # clearing them `docker compose up -d` may remove this helper mid-update.
  TTY_FLAGS=()
  [[ -t 0 ]] && TTY_FLAGS=(-i -t)
  docker run --rm ${TTY_FLAGS[@]+"${TTY_FLAGS[@]}"} \
    --user 0:0 \
    --network host \
    --workdir "${INSTALL_DIR}" \
    -e "BELUNE_DIR=${INSTALL_DIR}" \
    -v "${INSTALL_DIR}:${INSTALL_DIR}" \
    -v /var/run/docker.sock:/var/run/docker.sock \
    --label managed-by=belune \
    --label belune-helper=true \
    --label belune-update=true \
    --label com.docker.compose.project= \
    --label com.docker.compose.service= \
    --entrypoint /usr/local/bin/belune-update \
    "${TARGET_IMAGE}" "${TARGET_VERSION}"
}

main "$@"; exit $?
