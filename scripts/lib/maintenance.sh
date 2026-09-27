# shellcheck shell=bash
# Offline Compose commands; caller sources common.sh first (DESIGN.md §11.4).
# Never restart after a failed destructive operation: its marker blocks boot.
maintenance_run() {
  local was_running=0 status=0
  local running
  running=$(compose --profile app ps --status running -q app) || die 'cannot inspect app status; maintenance was not started'
  [[ -z "$running" ]] || was_running=1
  info 'stopping app (PostgreSQL remains up)'
  compose --profile app stop app || die 'cannot stop app; maintenance was not started'
  info "running offline $1"
  if compose --profile app run --rm --no-deps app "$@"; then
    status=0
  else
    status=$?
  fi
  if (( (status == 0 || (status == 1 && ${MAINTENANCE_DOCTOR:-0} == 1)) && was_running == 1 )); then
    info 'starting app again'
    compose --profile app start app || die 'maintenance succeeded but app did not restart; start it manually'
  elif (( status != 0 )); then
    info "offline command exited $status; app remains stopped; inspect the logs before starting it"
  fi
  return "$status"
}
