#!/usr/bin/env bash
# Runs one Go fuzz target in Docker, on the live sources.
#
# Usage: scripts/fuzz.sh <FuzzTarget> <duration> [package]
#   scripts/fuzz.sh FuzzSegment 60s
#   scripts/fuzz.sh FuzzKey 10m ./internal/names
#
# The sources are bind-mounted, so a failing input is written to
# <package>/testdata/fuzz/<FuzzTarget>/ in your working tree, ready to be
# committed as a regression test. The fuzzing cache lives in the
# go-build-cache volume and survives across runs.
set -euo pipefail

# shellcheck source=scripts/lib/common.sh
source "$(dirname -- "${BASH_SOURCE[0]}")/lib/common.sh"

usage="usage: scripts/fuzz.sh <FuzzTarget> <duration> [package]  (e.g. FuzzSegment 60s)"
[[ $# -ge 2 && $# -le 3 ]] || die "${usage}"

target="$1"
duration="$2"
pkg="${3:-./internal/names}"

[[ "${target}" =~ ^Fuzz[A-Za-z0-9_]*$ ]] || die "invalid fuzz target '${target}': ${usage}"
[[ "${duration}" =~ ^[0-9]+(ns|us|ms|s|m|h|x)$ ]] \
  || die "invalid duration '${duration}': use a Go duration (e.g. 30s, 10m) or a count (e.g. 1000x)"
[[ "${pkg}" =~ ^\./[A-Za-z0-9_./-]+$ && "${pkg}" != *...* ]] \
  || die "invalid package '${pkg}': a single ./relative/dir, no '...'"
[[ -d "${REPO_ROOT}/${pkg#./}" ]] || die "no such package directory: ${pkg}"

info "building the dev image (uid ${MUSICLIB_DEV_UID}:${MUSICLIB_DEV_GID})"
compose_dev build dev || die "building the dev image failed"

info "fuzzing ${target} in ${pkg} for ${duration}"
compose_dev run --rm dev \
  go test "${pkg}" -run='^$' -fuzz="^${target}\$" -fuzztime="${duration}"
