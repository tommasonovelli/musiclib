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
# Git Bash (MSYS) on Windows rewrites every argument that looks like a POSIX
# path before it reaches docker.exe (`--workdir /src` becomes
# `C:/Program Files/Git/src`). Turn that off, and give Docker the native
# Windows form of the repository path instead (NOTES.md N-059).
case "${OSTYPE:-}" in
  msys* | cygwin*)
    export MSYS_NO_PATHCONV=1
    REPO_ROOT="$(cd -- "${REPO_ROOT}" && pwd -W)"
    ;;
esac
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

# sqlc (DESIGN.md §2.1), pinned like every other image; see docs/docker.md.
readonly SQLC_IMAGE='sqlc/sqlc:1.31.1@sha256:70f53171d27b2424e9358869975455a6e955a5aa8e58a998a270a6e34e525537'

# run_sqlc <ro|rw> <sqlc args...>: sqlc on the repository, as the host user,
# without network. `ro` mounts the sources read-only (diff); `rw` lets
# `generate` write internal/store.
run_sqlc() {
  local mode="$1"
  shift
  local mount="type=bind,source=${REPO_ROOT},target=/src"
  [[ "${mode}" == ro ]] && mount+=",readonly"
  docker run --rm --network none --read-only --tmpfs /tmp --env HOME=/tmp \
    --user "${MUSICLIB_DEV_UID}:${MUSICLIB_DEV_GID}" --cap-drop ALL \
    --security-opt no-new-privileges:true \
    --mount "${mount}" --workdir /src "${SQLC_IMAGE}" "$@"
}

# Starts the throwaway PostgreSQL of the tests (NOTES.md N-024) and waits
# until it is healthy. It keeps running for the next runs; its data is tmpfs.
start_test_db() {
  info "starting postgres-test"
  compose --profile tools up --detach --wait postgres-test \
    || die "postgres-test did not become healthy (docker compose logs postgres-test)"
}
