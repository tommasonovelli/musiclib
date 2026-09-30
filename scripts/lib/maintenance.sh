# shellcheck shell=bash
# The offline commands (doctor, rebuild, backup, restore) through Compose;
# the caller sources common.sh first. They act on the installation's Compose
# file: compose.yaml, or COMPOSE_FILE (environment or .env) for a source
# build (compose_app), and only when its containers belong to this clone.
# Never restart after a failed destructive operation: its marker blocks boot.
maintenance_run() {
  local was_running=0 status=0
  local running
  maintenance_check_folder "$@"
  running=$(compose_app ps --status running -q app) || die 'cannot inspect app status; maintenance was not started'
  [[ -z "$running" ]] || was_running=1
  info 'stopping app (PostgreSQL remains up)'
  compose_app stop app || die 'cannot stop app; maintenance was not started'
  info "running offline $1"
  if compose_app run --rm --no-deps app "$@"; then
    status=0
  else
    status=$?
  fi
  if (( (status == 0 || (status == 1 && ${MAINTENANCE_DOCTOR:-0} == 1)) && was_running == 1 )); then
    info 'starting app again'
    compose_app start app || die 'maintenance succeeded but app did not restart; start it manually'
  elif (( status != 0 )); then
    info "offline command exited $status; app remains stopped; inspect the logs before starting it"
  fi
  return "$status"
}

# maintenance_check_folder <command...>: refuses, before anything is stopped,
# when the app or postgres container of this clone's Compose project was
# created in another folder. A clone next to an installation (~/musiclib) has
# the same project name, `musiclib`, but not that installation's .env: it
# would stop the installation's app and run the command with the clone's
# settings. Compose records the folder in a label of every container; `-ef`
# checks that it is this clone's folder, whatever its form (C:\... or /c/...
# in Git Bash, a symlink, a trailing slash). A folder this shell cannot see,
# or no label (-ef is false for both), is refused too. No container yet (a
# first start) passes.
maintenance_check_folder() {
  local ids folders folder
  # -a: a stopped installation counts too. Docker Desktop may end lines with CR.
  ids=$(compose_app ps -a -q app postgres) || die 'cannot inspect the Compose project; maintenance was not started'
  ids=${ids//$'\r'/}
  [[ -n "$ids" ]] || return 0
  # One container id per word.
  # shellcheck disable=SC2086
  folders=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project.working_dir"}}' $ids) \
    || die 'cannot inspect the Compose project; maintenance was not started'
  while IFS= read -r folder; do
    folder=${folder%$'\r'}
    [[ "$folder" -ef "$REPO_ROOT" ]] || maintenance_refuse_folder "$folder" "$@"
  done <<<"$folders"
}

# maintenance_refuse_folder <folder> <command...>: the refusal, with the
# commands to run by hand in the installation's folder instead.
maintenance_refuse_folder() {
  local folder="$1" why cd_line args start_line
  shift
  if [[ -n "$folder" ]]; then
    why="the Compose project of this clone (${REPO_ROOT}) already has containers created from ${folder}, which has its own compose.yaml and .env (a folder this shell cannot see counts as another one)"
    printf -v cd_line 'cd %q' "$folder"
  else
    why="a container of the Compose project of this clone (${REPO_ROOT}) does not record the folder it was created in: it may belong to another installation"
    cd_line='cd ~/musiclib    # the folder of your compose.yaml and .env'
  fi
  printf -v args '%q ' "$@"
  case "$1" in
    doctor) start_line='docker compose start app    # after exit 0 or 1' ;;
    restore) start_line='docker compose up -d --wait    # after exit 0' ;;
    *) start_line='docker compose start app    # after exit 0' ;;
  esac
  die "${why}. Nothing was stopped. Run the commands by hand in the installation's folder (docs/operations.md, \"Maintenance\"):
  ${cd_line}
  docker compose stop app
  docker compose run --rm --no-deps app ${args% }
  ${start_line}"
}
