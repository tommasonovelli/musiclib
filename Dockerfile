# syntax=docker/dockerfile:1.26.0@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32
#
# musiclib: one multi-stage Dockerfile for the toolchain, the test gate and the
# (future) runtime image. See docs/docker.md.
#
# Every base image is pinned by exact version AND by the digest of its
# multi-arch index (DESIGN.md §2.1: never `latest`). To bump one, resolve the
# new digest with `docker buildx imagetools inspect <image>:<exact-tag>` and
# update the ARG below together with docs/docker.md.
#
# Targets:
#   build-ffmpeg  ffmpeg + ffprobe from a pinned, signed source tarball (static)
#   build-lame    the LAME command line encoder, for the MP3 test fixtures only
#   toolchain     Go compiler + gcc (race detector), ffmpeg, lame, non-root `dev` user
#   deps          toolchain + module cache downloaded from go.mod/go.sum
#   test          deps + a read-only snapshot of the source tree (scripts/check.sh)
#   build-app     compiles ./cmd/musiclibd (static, CGO_ENABLED=0)
#   runtime       the image of the `app` service (DESIGN.md §11.1), with ffmpeg

ARG GO_IMAGE=golang:1.25.14-trixie@sha256:2c4c60ef415fbfa5e90300722293bef36c5e63fae17570ce18f580af933dbd73
ARG RUNTIME_IMAGE=debian:trixie-20260918-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a

# UID/GID of the unprivileged `dev` user of the toolchain stages (see
# `toolchain`). Declared here so that every stage that uses them sees the
# same default; a stage must still redeclare them (without a value) to use
# them.
ARG DEV_UID=10001
ARG DEV_GID=10001

# ---------------------------------------------------------------------------
# ffmpeg and ffprobe (DESIGN.md §2.1, §8.4; NOTES.md N-025, N-073).
#
# Built from the release tarball, pinned by version and sha256; the tarball's
# OpenPGP signature was checked against the FFmpeg release key
# (FCF9 86EA 15E6 E293 A564 4F10 B432 2F04 D676 58D8) when the pin was set.
# nasm (x86 SIMD code) is pinned the same way; its tarball has the content of
# Debian's nasm_2.16.03.orig.tar.xz.
#
# --disable-autodetect: no external library is picked up from the build
# image, so the configuration below is the whole feature set. The binaries
# are fully static (libc included) and therefore byte-identical in the test,
# dev and runtime images. Every native demuxer and decoder stays enabled:
# classification by content (§7.2) must recognize any audio, supported or
# not. --extra-version is part of the version string the application checks
# at boot and reports for render_version: bump it whenever this configure
# line changes, and update media.PinnedVersion.
FROM ${GO_IMAGE} AS build-ffmpeg

ARG NASM_VERSION=2.16.03
ARG NASM_SHA256=5bc940dd8a4245686976a8f7e96ba9340a0915f2d5b88356874890e207bdb581
ARG FFMPEG_VERSION=8.1.3
ARG FFMPEG_SHA256=bd458826a039b48a9606e794554c75eb4c4984b84173f7afa3128eae89336f2b
ARG FFMPEG_EXTRA_VERSION=musiclib1

WORKDIR /build
RUN curl -fsSL -o nasm.tar.gz "https://www.nasm.us/pub/nasm/releasebuilds/${NASM_VERSION}/nasm-${NASM_VERSION}.tar.gz" \
 && echo "${NASM_SHA256}  nasm.tar.gz" | sha256sum -c - \
 && tar -xzf nasm.tar.gz \
 && cd "nasm-${NASM_VERSION}" \
 && ./configure \
 && make -j"$(nproc)" nasm
RUN curl -fsSL -o ffmpeg.tar.gz "https://ffmpeg.org/releases/ffmpeg-${FFMPEG_VERSION}.tar.gz" \
 && echo "${FFMPEG_SHA256}  ffmpeg.tar.gz" | sha256sum -c - \
 && tar -xzf ffmpeg.tar.gz \
 && cd "ffmpeg-${FFMPEG_VERSION}" \
 && ./configure \
      --prefix=/opt/ffmpeg \
      --extra-version="${FFMPEG_EXTRA_VERSION}" \
      --x86asmexe="/build/nasm-${NASM_VERSION}/nasm" \
      --disable-autodetect \
      --disable-shared --enable-static --extra-ldflags=-static \
      --disable-debug --disable-doc \
      --disable-network --disable-ffplay \
      --disable-devices --enable-indev=lavfi \
 && make -j"$(nproc)" \
 && make install \
 && /opt/ffmpeg/bin/ffmpeg -hide_banner -version | head -n 1 \
 && /opt/ffmpeg/bin/ffprobe -hide_banner -version | head -n 1 \
 && ! ldd /opt/ffmpeg/bin/ffmpeg && ! ldd /opt/ffmpeg/bin/ffprobe

# ---------------------------------------------------------------------------
# LAME, only for the tests: FFmpeg has no native MP3 encoder, and the MP3
# fixtures (CBR, VBR, gapless header) are generated at test time (DESIGN.md
# §12.1). It goes into the toolchain images only, never into `runtime`, and
# ffmpeg is not linked against it: the application's ffmpeg is the same
# binary everywhere. The tarball has the content of Debian's
# lame_3.100.orig.tar.gz.
FROM ${GO_IMAGE} AS build-lame

ARG LAME_VERSION=3.100
ARG LAME_SHA256=ddfe36cab873794038ae2c1210557ad34857a4b6bdc515785d1da9e175b1da1e

WORKDIR /build
RUN curl -fsSL -o lame.tar.gz "https://downloads.sourceforge.net/project/lame/lame/${LAME_VERSION}/lame-${LAME_VERSION}.tar.gz" \
 && echo "${LAME_SHA256}  lame.tar.gz" | sha256sum -c - \
 && tar -xzf lame.tar.gz \
 && cd "lame-${LAME_VERSION}" \
 && ./configure --prefix=/opt/lame --disable-shared --enable-static --disable-gtktest --disable-decoder \
 && make -j"$(nproc)" \
 && make install \
 && /opt/lame/bin/lame --version | head -n 1

# ---------------------------------------------------------------------------
FROM ${GO_IMAGE} AS toolchain

# UID/GID of the unprivileged user that builds and runs the tests. The scripts
# pass the host user's ids so that bind-mounted sources stay writable on a
# native Docker Engine; running as root would also make permission tests
# meaningless (root bypasses DAC checks).
ARG DEV_UID
ARG DEV_GID

# GOTOOLCHAIN=local: never download a different toolchain behind our back.
# CGO_ENABLED=1: required by `go test -race` (gcc and libc6-dev are in the
# base image). -buildvcs=false: the source snapshot has no .git and bind
# mounts have foreign ownership; version stamping will come from -ldflags.
ENV GOTOOLCHAIN=local \
    CGO_ENABLED=1 \
    GOFLAGS="-mod=readonly -buildvcs=false" \
    GOPATH=/home/dev/go \
    GOCACHE=/home/dev/.cache/go-build \
    PATH=/usr/local/go/bin:/home/dev/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

# The mount points of the named volumes are created here, owned by `dev`: an
# empty named volume inherits owner and mode of the directory it is mounted
# on, so the caches and /testdata are writable without running as root.
RUN groupadd --non-unique --gid "${DEV_GID}" dev \
 && useradd --non-unique --uid "${DEV_UID}" --gid "${DEV_GID}" \
      --create-home --home-dir /home/dev --shell /bin/bash dev \
 && install -d -o dev -g dev -m 0755 \
      /home/dev/go /home/dev/go/pkg /home/dev/go/pkg/mod /home/dev/.cache /home/dev/.cache/go-build \
      /testdata /src

# The same ffmpeg/ffprobe binaries as `runtime`: the tests run the real tools
# (DESIGN.md §12.1). lame only generates MP3 fixtures in the tests.
COPY --from=build-ffmpeg /opt/ffmpeg/bin/ffmpeg /opt/ffmpeg/bin/ffprobe /usr/local/bin/
COPY --from=build-lame /opt/lame/bin/lame /usr/local/bin/lame

WORKDIR /src
USER dev

# ---------------------------------------------------------------------------
FROM toolchain AS deps

# Only go.mod/go.sum: this layer is rebuilt only when dependencies change.
# `go mod verify` checks the downloaded modules against go.sum.
COPY go.mod go.sum ./
RUN go mod download && go mod verify

# ---------------------------------------------------------------------------
FROM deps AS test

# Source snapshot owned by root and therefore read-only for `dev`: the gate
# tests exactly the tree that was in the build context, and a test cannot
# modify the sources. Tests write only to TMPDIR (/testdata, ext4 volume) and
# GOCACHE (named volume); see docker/with-testdata.sh.
COPY --chown=root:root . .

ENTRYPOINT ["/src/docker/with-testdata.sh"]
CMD ["/src/docker/gate.sh"]

# ---------------------------------------------------------------------------
FROM deps AS build-app

# Needed by the cache mount below: a stage sees only the ARGs it declares.
ARG DEV_UID
ARG DEV_GID
COPY . .
# Static binary: the Go side has no libc dependency. Native code (TagLib) lives
# in the separate `musiclib-tags` helper, not in this binary (DESIGN.md §2.1).
# -trimpath + pinned toolchain + no VCS stamping => reproducible output.
RUN --mount=type=cache,target=/home/dev/.cache/go-build,uid=${DEV_UID},gid=${DEV_GID} \
    CGO_ENABLED=0 go build -trimpath -o /home/dev/out/musiclibd ./cmd/musiclibd

# TODO(phase 2, DESIGN.md §2.1, §8.1): add a `build-tags` stage that compiles
# native/musiclib-tags against TagLib 2.x from a source tarball pinned by
# version and sha256, on the same Debian release as RUNTIME_IMAGE, and COPY
# the resulting binary into `runtime`. Its version feeds render_version.

# ---------------------------------------------------------------------------
FROM ${RUNTIME_IMAGE} AS runtime

# UID/GID are chosen in Compose (DESIGN.md §11.1). They are also build args so
# that /data is created with the right owner: an empty named volume mounted
# on /data inherits that owner. With a bind mount the host directory must be
# owned by the same ids.
ARG APP_UID=1000
ARG APP_GID=1000

# ffmpeg and ffprobe (DESIGN.md §2.1, §8.4): static, the same bytes as in the
# test and dev images. musiclibd checks their version at boot (§11.1 step 3).
COPY --from=build-ffmpeg /opt/ffmpeg/bin/ffmpeg /opt/ffmpeg/bin/ffprobe /usr/local/bin/

RUN install -d -o "${APP_UID}" -g "${APP_GID}" -m 0755 /data \
 && install -d -o root -g root -m 0755 /import

COPY --from=build-app /home/dev/out/musiclibd /usr/local/bin/musiclibd

# Never root. Compose overrides this with `user: UID:GID`; this is only the
# fallback for a bare `docker run`. umask 022 (DESIGN.md §11.1) is Docker's
# default and must additionally be enforced by musiclibd itself at startup.
USER ${APP_UID}:${APP_GID}
ENV HTTP_ADDR=:8080
EXPOSE 8080
WORKDIR /
ENTRYPOINT ["/usr/local/bin/musiclibd"]
