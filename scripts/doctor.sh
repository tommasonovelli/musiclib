#!/usr/bin/env bash
set -euo pipefail
# shellcheck source=scripts/lib/common.sh
source "$(dirname -- "${BASH_SOURCE[0]}")/lib/common.sh"
# shellcheck source=scripts/lib/maintenance.sh
source "$(dirname -- "${BASH_SOURCE[0]}")/lib/maintenance.sh"
if (( $# > 1 )) || { (( $# == 1 )) && [[ $1 != --deep ]]; }; then
  die 'usage: scripts/doctor.sh [--deep]'
fi
MAINTENANCE_DOCTOR=1 maintenance_run doctor "$@"
