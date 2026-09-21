#!/usr/bin/env bash
# Lints every shell script of the repository with shellcheck, in Docker.
#
# Usage: scripts/lint-shell.sh
set -euo pipefail

# shellcheck source=scripts/lib/common.sh
source "$(dirname -- "${BASH_SOURCE[0]}")/lib/common.sh"

# Pinned like every other image (DESIGN.md §2.1); see docs/docker.md.
readonly SHELLCHECK_IMAGE='koalaman/shellcheck:v0.11.0@sha256:61862eba1fcf09a484ebcc6feea46f1782532571a34ed51fedf90dd25f925a8d'

cd -- "${REPO_ROOT}"
mapfile -t files < <(find scripts docker -type f -name '*.sh' | LC_ALL=C sort)
[[ ${#files[@]} -gt 0 ]] || die "no shell scripts found"

info "shellcheck ${#files[@]} files"
docker run --rm --network none --read-only --user "${MUSICLIB_DEV_UID}:${MUSICLIB_DEV_GID}" \
  --mount "type=bind,source=${REPO_ROOT},target=/mnt,readonly" --workdir /mnt \
  "${SHELLCHECK_IMAGE}" --external-sources --source-path=SCRIPTDIR "${files[@]}"
info "clean"
