# shellcheck shell=bash
# Shared helpers for the host-side scripts. Source it; do not execute it.
#
# Sets:  REPO_ROOT, and exports MUSICLIB_DEV_UID / MUSICLIB_DEV_GID for Compose.
# Defines: die, info, compose.

die() {
  printf '%s: error: %s\n' "${0##*/}" "$*" >&2
  exit 1
}

info() {
  printf '%s: %s\n' "${0##*/}" "$*" >&2
}

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)"
readonly REPO_ROOT

command -v docker >/dev/null 2>&1 \
  || die "docker not found in PATH: Docker is the only prerequisite (see docs/docker.md)"
docker compose version >/dev/null 2>&1 \
  || die "'docker compose' (Compose v2 plugin) is not available"
docker info >/dev/null 2>&1 \
  || die "cannot reach the Docker daemon (is it running? is your user allowed to use it?)"

# The toolchain containers run as the host user, so that files written into
# the bind-mounted sources (dev, fuzz) belong to you. Root is refused as the
# test user: root bypasses permission checks and would hide bugs.
if [[ -z "${MUSICLIB_DEV_UID:-}" ]]; then
  MUSICLIB_DEV_UID="$(id -u)"
  MUSICLIB_DEV_GID="$(id -g)"
  if [[ "${MUSICLIB_DEV_UID}" == 0 ]]; then
    MUSICLIB_DEV_UID=10001
    MUSICLIB_DEV_GID=10001
  fi
fi
[[ "${MUSICLIB_DEV_UID}" != 0 ]] || die "MUSICLIB_DEV_UID=0: the tests must not run as root"
export MUSICLIB_DEV_UID MUSICLIB_DEV_GID="${MUSICLIB_DEV_GID:-${MUSICLIB_DEV_UID}}"

# docker compose, always on this repository's compose.yaml whatever the cwd.
compose() {
  docker compose --project-directory "${REPO_ROOT}" -f "${REPO_ROOT}/compose.yaml" "$@"
}
