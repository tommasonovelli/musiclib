#!/usr/bin/env bash
set -euo pipefail
# shellcheck source=scripts/lib/common.sh
source "$(dirname -- "${BASH_SOURCE[0]}")/lib/common.sh"
# shellcheck source=scripts/lib/maintenance.sh
source "$(dirname -- "${BASH_SOURCE[0]}")/lib/maintenance.sh"
[[ $# == 1 && -n "$1" ]] || die 'usage: scripts/rebuild.sh STORE_ID (read /data/.musiclib-store first)'
maintenance_run rebuild --store-id "$1"
