#!/usr/bin/env bash
# Interactive shell (or one command) in the Go toolchain container, on the
# live sources, with TMPDIR on the ext4 test volume.
#
# Usage: scripts/dev.sh                 bash
#        scripts/dev.sh go test ./internal/fsops/ -run TestLock -v
set -euo pipefail

# shellcheck source=scripts/lib/common.sh
source "$(dirname -- "${BASH_SOURCE[0]}")/lib/common.sh"

compose build dev || die "building the dev image failed"

if [[ $# -eq 0 ]]; then
  compose run --rm dev
else
  compose run --rm dev "$@"
fi
