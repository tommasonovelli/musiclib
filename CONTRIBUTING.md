# Contributing to Vibrance MusicLib

Read the [README](README.md), [design](DESIGN.md) and relevant [decisions](NOTES.md) before changing behavior. Contributions of original code and documentation must be available under the project's [MIT License](LICENSE). The logo, the sun symbol, stays outside the MIT License ([LOGO.md](LOGO.md)). Include the origin and applicable license of any third-party material, preserve its notices, and clarify compatibility with the maintainer before submitting it.

## Development setup

Use Docker with the Compose v2 plugin; the pinned toolchain is in the repository. See [docs/docker.md](docs/docker.md) for setup, profiles and test storage. Docker Desktop is a development option, while production acceptance requires native Ubuntu 24.04+ and local ext4. On Windows, run the shell scripts from Git Bash and keep the checkout's LF line endings.

```sh
scripts/dev.sh                  # shell in the toolchain container
scripts/check.sh                # sqlc diff, build, vet, formatting and race tests
scripts/lint-shell.sh           # when changing shell scripts
```

The test gate requires real PostgreSQL and ext4; it is not equivalent to a host `go test` that skips database tests. The test image contains a snapshot of the working tree; `dev` mounts the live sources. After changing SQL or migrations, regenerate the store with `scripts/sqlc.sh` and review the generated diff. Media/tool version changes have additional pinning steps in [the Docker guide](docs/docker.md#pinned-source-builds).

## Preparing a change

Keep changes focused and explain the problem, resulting behavior and verification. Preserve the storage invariants: immutable originals, one instance per paired database/volume, complete-album publication, explicit conflict handling and recoverable maintenance. Add meaningful regression coverage when changing those guarantees and update the relevant documentation. Use redistributable fixtures rather than personal music or artwork.

For documentation-only changes, review the full diff, check local links and command semantics, and run `git diff --check`; a container rebuild is unnecessary. For code changes, run the relevant tests and the repository gate. Record commands, results and any skips accurately. Passing Docker Desktop tests does not close the [native release acceptance gate](docs/operations.md#release-check-on-a-native-host).

## Reports and release work

For a reproducible bug report, include the commit, host/kernel, filesystem, Docker/Compose versions, relevant configuration without credentials, minimal steps, expected/actual behavior and redacted logs. Never attach `.env`, database URLs containing passwords, catalog dumps, private music or personal metadata. If an integrity problem is involved, preserve the evidence and backups before attempting recovery.

Use the [public release checklist](opensource.md) to track packaging and documentation gaps. Public support channels, a private security-reporting contact and the changelog policy still need to be established by the owner; do not publish vulnerability details or credentials in a public report.
