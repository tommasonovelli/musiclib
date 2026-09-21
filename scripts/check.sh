#!/usr/bin/env bash
# Full quality gate, in Docker: go build, go vet, gofmt, go test -race.
#
# Usage: scripts/check.sh [package-pattern...]     default: ./...
#   scripts/check.sh                       everything
#   scripts/check.sh ./internal/names/...  one package tree
#
# The sources are copied into the `test` image (a snapshot of the working
# tree at build time), the container has no network and a read-only root
# filesystem, and TMPDIR is on the ext4 `testdata` volume. See docs/docker.md.
set -euo pipefail

# shellcheck source=scripts/lib/common.sh
source "$(dirname -- "${BASH_SOURCE[0]}")/lib/common.sh"

info "building the test image (uid ${MUSICLIB_DEV_UID}:${MUSICLIB_DEV_GID})"
compose build test || die "building the test image failed"

info "running the gate"
compose run --rm test /src/docker/gate.sh "$@"
