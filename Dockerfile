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
#   build-tags    TagLib 2.3.2 (pinned tarball) and native/musiclib-tags, static + ASan
#   toolchain     Go compiler + gcc (race detector), ffmpeg, lame, musiclib-tags, non-root `dev` user
#   deps          toolchain + module cache downloaded from go.mod/go.sum
#   test          deps + a read-only snapshot of the source tree (scripts/check.sh)
#   build-app     compiles ./cmd/musiclibd (static, CGO_ENABLED=0), stamped with MUSICLIB_VERSION
#   runtime       the image of the `app` service (DESIGN.md §11.1), with ffmpeg and musiclib-tags

ARG GO_IMAGE=golang:1.25.14-trixie@sha256:2c4c60ef415fbfa5e90300722293bef36c5e63fae17570ce18f580af933dbd73
ARG RUNTIME_IMAGE=debian:trixie-20260918-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a

# UID/GID of the unprivileged `dev` user of the toolchain stages (see
# `toolchain`). Declared here so that every stage that uses them sees the
# same default; a stage must still redeclare them (without a value) to use
# them.
ARG DEV_UID=10001
ARG DEV_GID=10001

# Release metadata (NOTES.md N-325). Only `build-app` (the version stamped
# into musiclibd) and the end of `runtime` (OCI labels) redeclare them, so a
# new value never invalidates the toolchain, test or dev layers. The
# defaults suit a local build; the release workflow passes all three.
#   MUSICLIB_VERSION   the application version, a token of [0-9A-Za-z.+-]
#   MUSICLIB_REVISION  the source revision (git commit) of the build
#   MUSICLIB_SOURCE    the URL of the source repository
ARG MUSICLIB_VERSION=devel
ARG MUSICLIB_REVISION=
ARG MUSICLIB_SOURCE=

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
# The TagLib helper native/musiclib-tags (DESIGN.md §2.1, §8.1; NOTES.md
# N-025, N-083).
#
# TagLib is built from its release tarball, pinned by version and sha256.
# TagLib publishes no signature; the sha256 is the digest GitHub reports for
# the release asset and the one Homebrew's formula pins independently.
# utfcpp, the one dependency TagLib 2 needs, is bundled in that tarball
# (3rdparty/utfcpp). CMake is Kitware's binary release, pinned by sha256; the
# SHA-256 list it comes from is signed by Kitware's release key (checked when
# the pin was set). No zlib: only compressed ID3v2 frames need it (N-083).
#
# Only the TagLib modules of the supported formats are built: Vorbis (FLAC),
# MP4, and APE (the APE tags of MP3; MPEG and ID3 are always built).
#
# Two builds of the same sources:
#   - release: static TagLib and a fully static helper (libc and libstdc++
#     included), the same bytes in the test, dev and runtime images;
#   - asan: TagLib and the helper under AddressSanitizer and UBSan, used by
#     the hostile-input tests only, never in `runtime`.
# TAGLIB_BUILD_REVISION is the revision of this configuration: it is part of
# the version the helper prints and the application checks at boot. Bump it
# whenever the cmake line changes, and update media.PinnedTagLibVersion.
FROM ${GO_IMAGE} AS build-tags

ARG CMAKE_VERSION=4.4.3
ARG CMAKE_SHA256=d6c83076c575bc00b823522ac974bda66d0af05d6ddc30e739c12385cf32c6cc
ARG TAGLIB_VERSION=2.3.2
ARG TAGLIB_SHA256=3ca2d8afaa7f1cf7f6ed10e511ebc368bfacd6dcaa3dbfa690b89e502e8963dc
ARG TAGLIB_BUILD_REVISION=musiclib1

WORKDIR /build
RUN curl -fsSL -o cmake.tar.gz "https://github.com/Kitware/CMake/releases/download/v${CMAKE_VERSION}/cmake-${CMAKE_VERSION}-linux-x86_64.tar.gz" \
 && echo "${CMAKE_SHA256}  cmake.tar.gz" | sha256sum -c - \
 && mkdir cmake \
 && tar -xzf cmake.tar.gz -C cmake --strip-components=1 \
 && rm cmake.tar.gz
# CMAKE_*_FLAGS_RELEASE only keeps NDEBUG: the optimization level is set in
# CMAKE_CXX_FLAGS, per variant. -ffile-prefix-map keeps /build out of the
# objects, so that the build is reproducible byte for byte.
RUN curl -fsSL -o taglib.tar.gz "https://github.com/taglib/taglib/releases/download/v${TAGLIB_VERSION}/taglib-${TAGLIB_VERSION}.tar.gz" \
 && echo "${TAGLIB_SHA256}  taglib.tar.gz" | sha256sum -c - \
 && tar -xzf taglib.tar.gz \
 && for variant in release asan; do \
      if [ "${variant}" = release ]; then \
        flags="-O2"; prefix=/opt/taglib; \
      else \
        flags="-O1 -g -fno-omit-frame-pointer -fsanitize=address,undefined -fno-sanitize-recover=all"; prefix=/opt/taglib-asan; \
      fi; \
      /build/cmake/bin/cmake -S "taglib-${TAGLIB_VERSION}" -B "taglib-${variant}" \
        -DCMAKE_BUILD_TYPE=Release \
        -DCMAKE_C_FLAGS="${flags} -ffile-prefix-map=/build=." \
        -DCMAKE_CXX_FLAGS="${flags} -ffile-prefix-map=/build=." \
        -DCMAKE_C_FLAGS_RELEASE=-DNDEBUG -DCMAKE_CXX_FLAGS_RELEASE=-DNDEBUG \
        -DCMAKE_INSTALL_PREFIX="${prefix}" \
        -DBUILD_SHARED_LIBS=OFF -DBUILD_TESTING=OFF -DBUILD_EXAMPLES=OFF -DBUILD_BINDINGS=OFF \
        -DWITH_ZLIB=OFF \
        -DWITH_VORBIS=ON -DWITH_MP4=ON -DWITH_APE=ON \
        -DWITH_ASF=OFF -DWITH_DSF=OFF -DWITH_MATROSKA=OFF -DWITH_MOD=OFF -DWITH_RIFF=OFF \
        -DWITH_SHORTEN=OFF -DWITH_TRUEAUDIO=OFF \
      && /build/cmake/bin/cmake --build "taglib-${variant}" --parallel "$(nproc)" \
      && /build/cmake/bin/cmake --install "taglib-${variant}" \
      || exit 1; \
    done
COPY native/musiclib-tags/ /build/musiclib-tags/
RUN make -C musiclib-tags check \
 && make -C musiclib-tags MODE=release TAGLIB=/opt/taglib TAGLIB_BUILD="${TAGLIB_BUILD_REVISION}" \
 && make -C musiclib-tags MODE=asan TAGLIB=/opt/taglib-asan TAGLIB_BUILD="${TAGLIB_BUILD_REVISION}" \
 && install -D -m 0755 musiclib-tags/build/release/musiclib-tags /opt/musiclib-tags/bin/musiclib-tags \
 && install -D -m 0755 musiclib-tags/build/asan/musiclib-tags /opt/musiclib-tags/bin/musiclib-tags-asan \
 && /opt/musiclib-tags/bin/musiclib-tags version \
 && /opt/musiclib-tags/bin/musiclib-tags-asan version \
 && ! ldd /opt/musiclib-tags/bin/musiclib-tags

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
# The TagLib helper, the same bytes as in `runtime`, and its sanitized build
# for the hostile-input tests (toolchain images only).
COPY --from=build-tags /opt/musiclib-tags/bin/musiclib-tags /opt/musiclib-tags/bin/musiclib-tags-asan /usr/local/bin/

# Dev/test-only real browser for the §12.1 UI tests. Freeze the entire
# Debian dependency closure at a signed snapshot (N-212); the runtime stage
# does not inherit these apt sources or Chromium. The snapshot predates the
# build and contains the exact verified Chromium package below.
RUN printf '%s\n' \
      'deb [check-valid-until=no] https://snapshot.debian.org/archive/debian/20260926T000000Z trixie main' \
      'deb [check-valid-until=no] https://snapshot.debian.org/archive/debian-security/20260926T000000Z trixie-security main' \
      > /etc/apt/sources.list \
 && rm -f /etc/apt/sources.list.d/debian.sources
RUN apt-get update \
 && apt-get download chromium=154.0.8037.57-1~deb13u1 \
 && echo 'd70bab9fbcb7bfbb7227b9510fb5cf1f7290bd6d6af3dd168b19ba4cc9b8035d  chromium_154.0.8037.57-1~deb13u1_amd64.deb' | sha256sum -c - \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends ./chromium_154.0.8037.57-1~deb13u1_amd64.deb postgresql-client-17=17.11-0+deb13u1 \
 && rm -f chromium_*.deb && rm -rf /var/lib/apt/lists/*
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
# -trimpath + pinned toolchain + no VCS stamping => reproducible output: the
# same sources and MUSICLIB_VERSION give the same bytes.
#
# The version is the single source of buildinfo.Version (NOTES.md N-325). It
# must be a non-empty token (restore refuses an empty app_version), and the
# built binary must report it: a mistyped -X path would silently leave
# "devel".
ARG MUSICLIB_VERSION
RUN --mount=type=cache,target=/home/dev/.cache/go-build,uid=${DEV_UID},gid=${DEV_GID} \
    case "${MUSICLIB_VERSION}" in \
      ''|*[!0-9A-Za-z.+-]*) echo "MUSICLIB_VERSION must be a non-empty token of [0-9A-Za-z.+-]: '${MUSICLIB_VERSION}'" >&2; exit 1 ;; \
    esac \
 && CGO_ENABLED=0 go build -trimpath \
      -ldflags "-X musiclib/internal/buildinfo.Version=${MUSICLIB_VERSION}" \
      -o /home/dev/out/musiclibd ./cmd/musiclibd \
 && test "$(/home/dev/out/musiclibd version | sed -n 's/^version: //p')" = "${MUSICLIB_VERSION}"

# ---------------------------------------------------------------------------
FROM ${RUNTIME_IMAGE} AS runtime

# The image's uid:gid: the defaults, 1000:1000, in the published image and in
# the source build of compose.dev.yaml (NOTES.md N-330). /data and /backup
# are created owned by it: an empty named volume mounted there inherits that
# owner. Compose may run the process as other ids with `user:` (DESIGN.md
# §11.1); /data and /backup must then be bind mounts owned by those ids.
ARG APP_UID=1000
ARG APP_GID=1000

# PostgreSQL 17.11 clients used by the offline backup/restore commands.
# Debian's signed, dated snapshot pins the entire dependency closure. The
# slim base has no CA bundle; apt authenticates signed InRelease and Packages
# metadata even when fetching this fixed snapshot over HTTP. The two binaries
# are run at the absolute paths musiclibd uses, so a broken client (missing
# shared library, wrong layout) fails the image build instead of a backup
# (N-230 D3).
RUN printf '%s\n' \
      'deb [check-valid-until=no] http://snapshot.debian.org/archive/debian/20260926T000000Z trixie main' \
      'deb [check-valid-until=no] http://snapshot.debian.org/archive/debian-security/20260926T000000Z trixie-security main' \
      > /etc/apt/sources.list \
 && rm -f /etc/apt/sources.list.d/debian.sources \
 && apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends postgresql-client-17=17.11-0+deb13u1 \
 && rm -rf /var/lib/apt/lists/* \
 && /usr/lib/postgresql/17/bin/pg_dump --version \
 && /usr/lib/postgresql/17/bin/pg_restore --version

# ffmpeg and ffprobe (DESIGN.md §2.1, §8.4): static, the same bytes as in the
# test and dev images. musiclibd checks their version at boot (§11.1 step 3).
COPY --from=build-ffmpeg /opt/ffmpeg/bin/ffmpeg /opt/ffmpeg/bin/ffprobe /usr/local/bin/
# The TagLib helper (DESIGN.md §2.1, §8.1): static, the same bytes as in the
# test and dev images. musiclibd checks its version at boot.
COPY --from=build-tags /opt/musiclib-tags/bin/musiclib-tags /usr/local/bin/

RUN install -d -o "${APP_UID}" -g "${APP_GID}" -m 0755 /data /backup \
 && install -d -o root -g root -m 0755 /import

# MusicLib's license and the notices of the third-party software in this
# image, with the license texts they refer to (THIRD_PARTY_NOTICES.md).
COPY LICENSE LOGO.md THIRD_PARTY_NOTICES.md /usr/share/doc/musiclib/
COPY licenses/ /usr/share/doc/musiclib/licenses/
COPY web/OFL.txt /usr/share/doc/musiclib/web/OFL.txt

COPY --from=build-app /home/dev/out/musiclibd /usr/local/bin/musiclibd

# Never root. Compose overrides this with `user: UID:GID`; this is only the
# fallback for a bare `docker run`. umask 022 (DESIGN.md §11.1) is Docker's
# default and must additionally be enforced by musiclibd itself at startup.
USER ${APP_UID}:${APP_GID}
ENV HTTP_ADDR=:8080
EXPOSE 8080
WORKDIR /
ENTRYPOINT ["/usr/local/bin/musiclibd"]

# OCI annotations of the published image (NOTES.md N-325), last so that new
# values change no layer. The version is the one stamped into musiclibd.
ARG MUSICLIB_VERSION
ARG MUSICLIB_REVISION
ARG MUSICLIB_SOURCE
LABEL org.opencontainers.image.title="Vibrance MusicLib" \
      org.opencontainers.image.description="Self-hosted music library manager: imports albums, keeps the originals unchanged and generates an organized, tagged library." \
      org.opencontainers.image.version="${MUSICLIB_VERSION}" \
      org.opencontainers.image.revision="${MUSICLIB_REVISION}" \
      org.opencontainers.image.source="${MUSICLIB_SOURCE}" \
      org.opencontainers.image.licenses="MIT"
