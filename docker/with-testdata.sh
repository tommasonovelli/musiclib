#!/usr/bin/env bash
# Runs a command with TMPDIR on the test-data volume (DESIGN.md §3.1, §12.1).
#
# Runs INSIDE the container. /testdata must be a mount of a real ext4
# filesystem (a named volume: see compose.dev.yaml and docs/docker.md), so that
# t.TempDir() -- and therefore the internal/fsops tests -- exercise the real
# ext4 behaviour of openat2, renameat2, fsync and flock instead of overlayfs.
#
# Usage: with-testdata.sh <command> [args...]
# Env:   MUSICLIB_ALLOW_NON_EXT4=1  continue (with a warning) on a non-ext4 mount
set -euo pipefail

readonly TESTDATA_ROOT=/testdata
readonly EXT4_MAGIC=ef53 # EXT4_SUPER_MAGIC, shared with ext2/ext3

die() {
  printf 'with-testdata: error: %s\n' "$*" >&2
  exit 1
}

[[ $# -gt 0 ]] || die "usage: with-testdata.sh <command> [args...]"

mountpoint -q "${TESTDATA_ROOT}" \
  || die "${TESTDATA_ROOT} is not a mount point: the tests would run on the container's overlay filesystem. Mount the test-data volume (use scripts/check.sh or 'docker compose -f compose.dev.yaml run')."

[[ -w "${TESTDATA_ROOT}" ]] \
  || die "${TESTDATA_ROOT} is not writable by uid $(id -u). The volume was probably created for another uid; remove it with 'docker volume rm musiclib_testdata' and retry."

fs_type_hex="$(stat -f -c %t "${TESTDATA_ROOT}")"
fs_source="$(findmnt -n -o SOURCE,FSTYPE --target "${TESTDATA_ROOT}" || true)"
if [[ "${fs_type_hex}" != "${EXT4_MAGIC}" ]]; then
  if [[ "${MUSICLIB_ALLOW_NON_EXT4:-0}" == 1 ]]; then
    printf 'with-testdata: WARNING: %s is not ext4 (magic 0x%s, %s); DESIGN.md §12.1 requires ext4\n' \
      "${TESTDATA_ROOT}" "${fs_type_hex}" "${fs_source}" >&2
  else
    die "${TESTDATA_ROOT} is not ext4 (magic 0x${fs_type_hex}, ${fs_source}); DESIGN.md §12.1 requires ext4. Set MUSICLIB_ALLOW_NON_EXT4=1 to run anyway."
  fi
fi

# Remove leftovers of runs that were killed (SIGKILL skips the trap below).
# Only directories older than a day: a concurrent run may still be using a
# younger one.
find "${TESTDATA_ROOT}" -mindepth 1 -maxdepth 1 -name 'run.*' -type d -mmin +1440 \
  -exec rm -rf -- {} + 2>/dev/null || true

run_dir="$(mktemp -d "${TESTDATA_ROOT}/run.XXXXXXXX")"
# shellcheck disable=SC2329 # invoked by the EXIT trap
cleanup() {
  # Test trees may contain read-only directories: make them removable first.
  chmod -R u+rwx -- "${run_dir}" 2>/dev/null || true
  rm -rf -- "${run_dir}"
}
trap cleanup EXIT

export TMPDIR="${run_dir}"
printf 'with-testdata: TMPDIR=%s on %s (magic 0x%s), uid=%s gid=%s\n' \
  "${TMPDIR}" "${fs_source}" "${fs_type_hex}" "$(id -u)" "$(id -g)" >&2

# Not exec: the trap must run after the command to remove the run directory.
set +e
"$@"
rc=$?
set -e
exit "${rc}"
