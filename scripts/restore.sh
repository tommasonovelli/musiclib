#!/usr/bin/env bash
set -euo pipefail
# shellcheck source=scripts/lib/common.sh
source "$(dirname -- "${BASH_SOURCE[0]}")/lib/common.sh"
# shellcheck source=scripts/lib/maintenance.sh
source "$(dirname -- "${BASH_SOURCE[0]}")/lib/maintenance.sh"
[[ $# == 1 && -n "$1" && "$1" != '.' && "$1" != '..' && "$1" != */* ]] || die 'usage: scripts/restore.sh BACKUP_NAME (new empty DB and /data required)'
maintenance_run restore --from "/backup/$1"
