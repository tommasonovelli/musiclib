#!/usr/bin/env bash
# The quality gate: build, vet, gofmt, race-enabled tests.
#
# Runs INSIDE the container, from the module root, normally wrapped by
# with-testdata.sh (which puts TMPDIR on the ext4 test volume).
#
# Usage: gate.sh [package-pattern...]      default: ./...
#   Patterns must be directory patterns relative to the module root, e.g.
#   ./internal/names or ./internal/names/... ; gofmt is limited to the same
#   directories.
set -euo pipefail

die() {
  printf 'gate: error: %s\n' "$*" >&2
  exit 1
}

step() {
  printf '\n==> %s\n' "$*"
}

[[ -f go.mod ]] || die "go.mod not found in $(pwd): run from the module root"

if [[ $# -gt 0 ]]; then
  pkgs=("$@")
else
  pkgs=(./...)
fi

# Directories for gofmt, derived from the package patterns.
fmt_dirs=()
for p in "${pkgs[@]}"; do
  [[ "${p}" == ./* ]] || die "package pattern must start with './': ${p}"
  d="${p%/...}"
  [[ "${d}" == ... ]] && d=.
  [[ -d "${d}" ]] || die "no such directory for pattern ${p}: ${d}"
  fmt_dirs+=("${d}")
done

step "toolchain: $(go version), CGO_ENABLED=$(go env CGO_ENABLED), TMPDIR=${TMPDIR:-unset}"

step "go build ${pkgs[*]}"
go build "${pkgs[@]}"

step "go vet ${pkgs[*]}"
go vet "${pkgs[@]}"

step "gofmt -l ${fmt_dirs[*]}"
unformatted="$(gofmt -l "${fmt_dirs[@]}")"
if [[ -n "${unformatted}" ]]; then
  printf '%s\n' "${unformatted}" >&2
  die "the files above are not gofmt-formatted (run: gofmt -w <file>)"
fi

step "go test -race -count=1 -timeout=${GATE_TEST_TIMEOUT:-15m} ${pkgs[*]}"
go test -race -count=1 -timeout="${GATE_TEST_TIMEOUT:-15m}" "${pkgs[@]}"

step "gate passed"
