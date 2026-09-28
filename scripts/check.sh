#!/usr/bin/env bash
# Full quality gate, in Docker: generated sqlc code up to date, then go build,
# go vet, gofmt, go test -race (PostgreSQL tests included, never skipped).
#
# Usage: scripts/check.sh [package-pattern...]     default: ./...
#   scripts/check.sh                       everything
#   scripts/check.sh ./internal/names/...  one package tree
#
# The sources are copied into the `test` image (a snapshot of the working
# tree at build time), the container has a read-only root filesystem, TMPDIR
# on the ext4 `testdata` volume and no network but the internal one of
# postgres-test. See docs/docker.md.
set -euo pipefail

# shellcheck source=scripts/lib/common.sh
source "$(dirname -- "${BASH_SOURCE[0]}")/lib/common.sh"

info "sqlc diff: generated code in internal/store is up to date"
run_sqlc ro diff || die "internal/store is out of date with sql/ or migrations/: run scripts/sqlc.sh"

info "building the test image (uid ${MUSICLIB_DEV_UID}:${MUSICLIB_DEV_GID})"
compose_dev build test || die "building the test image failed"

start_test_db

info "running the gate"
compose_dev --profile tools run --rm test /src/docker/gate.sh "$@"
