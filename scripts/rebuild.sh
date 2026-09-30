#!/usr/bin/env bash
set -euo pipefail
# shellcheck source=scripts/lib/common.sh
source "$(dirname -- "${BASH_SOURCE[0]}")/lib/common.sh"
# shellcheck source=scripts/lib/maintenance.sh
source "$(dirname -- "${BASH_SOURCE[0]}")/lib/maintenance.sh"
[[ $# == 1 && -n "$1" ]] || die 'usage: scripts/rebuild.sh STORE_ID (read /data/.musiclib-store first)'
# The rule of `musiclibd rebuild`: a canonical lowercase, non-nil UUID. Checked
# here too, so that a mistyped id does not leave the app stopped. The digits
# are listed, not a range: a range such as a-f can match capitals in some locales.
hex='[0123456789abcdef]'
uuid="^${hex}{8}-${hex}{4}-${hex}{4}-${hex}{4}-${hex}{12}\$"
[[ $1 =~ $uuid && $1 != 00000000-0000-0000-0000-000000000000 ]] \
  || die "STORE_ID must be the UUID after store_id= in /data/.musiclib-store, exactly as printed, in lowercase: nothing was stopped"
maintenance_run rebuild --store-id "$1"
