#!/usr/bin/env bash
# Regenerates internal/store from sql/ and migrations/ with the pinned sqlc
# image (sqlc.yaml). Commit the generated files together with their sources;
# scripts/check.sh fails while they are out of date.
#
# Usage: scripts/sqlc.sh            generate
#        scripts/sqlc.sh diff       only report differences
set -euo pipefail

# shellcheck source=scripts/lib/common.sh
source "$(dirname -- "${BASH_SOURCE[0]}")/lib/common.sh"

case "${1:-generate}" in
  generate) run_sqlc rw generate ;;
  diff) run_sqlc ro diff ;;
  *) die "usage: scripts/sqlc.sh [generate|diff]" ;;
esac
