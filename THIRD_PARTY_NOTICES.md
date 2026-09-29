# Third-party notices

The container image `ghcr.io/tommasonovelli/musiclib` holds Vibrance MusicLib and the third-party software listed here. MusicLib's own code is under the MIT License ([LICENSE](LICENSE)); its sun logo is not ([LOGO.md](LOGO.md)). Each third-party component keeps its own license, below.

Paths in this file are relative to the root of the source repository. In the image, this file, `LICENSE`, `LOGO.md`, `licenses/` and `web/OFL.txt` are under `/usr/share/doc/musiclib/`.

**Sources.** Every GitHub release of MusicLib attaches this file and the unmodified source tarballs of FFmpeg and TagLib that its image was built from, checked against the sha256 pinned in the `Dockerfile`. The build instructions of every binary in the image are the `Dockerfile` and `native/musiclib-tags/` of the same release, included in the release's source code archives. The sources of the Debian packages are in Debian's permanent snapshot archive (see [Debian packages](#debian-packages)).

| Component | Version | License | In the image |
|---|---|---|---|
| [FFmpeg](#ffmpeg) | 8.1.3 | GPL-2.0-or-later as built for the image (FFmpeg is otherwise LGPL-2.1-or-later; some files BSD, MIT, ISC, IJG) | `/usr/local/bin/ffmpeg`, `/usr/local/bin/ffprobe` |
| [TagLib](#taglib) | 2.3.2 | MPL-1.1 (TagLib is dual-licensed LGPL-2.1 or MPL-1.1; MusicLib distributes it under MPL-1.1) | statically linked into `/usr/local/bin/musiclib-tags` |
| [utfcpp](#utfcpp) | 4.2.0, bundled with TagLib | BSL-1.0 | statically linked into `musiclib-tags` |
| [GNU C Library](#gnu-c-library-gcc-runtime-libraries) | 2.41-12+deb13u3 (Debian) | LGPL-2.1-or-later | statically linked into `ffmpeg`, `ffprobe`, `musiclib-tags` |
| [GCC runtime libraries](#gnu-c-library-gcc-runtime-libraries) (libstdc++, libgcc) | 14.2.0-19 (Debian) | GPL-3.0-or-later with the GCC Runtime Library Exception 3.1 | libgcc statically linked into `ffmpeg`, `ffprobe`, `musiclib-tags`; libstdc++ into `musiclib-tags` |
| [Go standard library and runtime](#go) | 1.25.14 | BSD-3-Clause | compiled into `/usr/local/bin/musiclibd` |
| [Go modules](#go) | see the table below | BSD-3-Clause, MIT, Apache-2.0 | compiled into `musiclibd` |
| [Hanken Grotesk](#hanken-grotesk) | v12 | OFL-1.1 | embedded in `musiclibd`, served to the browser |
| [Debian packages](#debian-packages) | Debian 13 (trixie) | per package | the base system and the PostgreSQL 17 client |

## FFmpeg

- Copyright (c) 2000-2026 the FFmpeg developers; the files under other licenses name their own authors in `licenses/ffmpeg/NOTICES.txt`.
- License: `ffmpeg` and `ffprobe` as distributed in the image are under the **GNU General Public License version 2 or later**. FFmpeg is otherwise under the GNU Lesser General Public License version 2.1 or later, and the build (the `build-ffmpeg` stage of the `Dockerfile`) passes no `--enable-gpl`, `--enable-version3` or `--enable-nonfree` and links no external library but the C library (`--disable-autodetect`). FFmpeg 8.1.3 nevertheless compiles three files under GPL-2.0-or-later into both binaries with its `perlin` and `codecview` filters, which it enables without `--enable-gpl`: `libavfilter/perlin.c`, `libavfilter/qp_table.c` and `libavfilter/qp_table.h`. The other files keep their own licenses (LGPL and the permissive ones below), all compatible with the GPL. `ffmpeg -L` prints the LGPL notice because the build does not set `--enable-gpl`; it does not account for these three files. `ffmpeg -buildconf` prints the configuration of the binaries in the image.
- MusicLib's own code is not affected: `musiclibd` runs `ffmpeg` and `ffprobe` as separate programs, with command-line arguments, pipes and files, and is not linked with FFmpeg. It stays under the MIT License.
- This software is based in part on the work of the Independent JPEG Group.
- Texts: `licenses/ffmpeg/COPYING.GPLv2` (the license of the binaries), `licenses/ffmpeg/COPYING.LGPLv2.1`, `licenses/ffmpeg/LICENSE.md` (FFmpeg's licensing summary), `licenses/ffmpeg/NOTICES.txt` (the BSD, MIT, ISC and IJG notices of the files compiled in).
- Source: `ffmpeg-8.1.3.tar.gz`, the complete FFmpeg source of both binaries, is attached unmodified to every MusicLib release; the scripts that control its compilation are the `build-ffmpeg` stage of the `Dockerfile`, in the same release's source code archives. Upstream at <https://ffmpeg.org/releases/ffmpeg-8.1.3.tar.gz>. The parts of the C library and of the GCC runtime statically linked into both binaries come from the Debian source packages named in [GNU C Library, GCC runtime libraries](#gnu-c-library-gcc-runtime-libraries). The binaries are static; to rebuild them, with a modified FFmpeg or a modified C library, follow the `build-ffmpeg` stage.

## TagLib

- Copyright (C) 2002-2026 Scott Wheeler, Lukáš Lalinský, Urs Fleisch, Tsuda Kageyu and the other TagLib authors (the `AUTHORS` file and the notice at the top of each source file).
- License: TagLib is available under the GNU LGPL version 2.1 or the Mozilla Public License 1.1. MusicLib distributes it under the **Mozilla Public License 1.1**. Text: `licenses/taglib/COPYING.MPL`.
- The Source Code version of TagLib 2.3.2, Covered Code under the Mozilla Public License 1.1, is available under the terms of that license: unmodified, as `taglib-2.3.2.tar.gz` attached to every MusicLib release (which is how MusicLib makes it available as section 3.2 of the license requires), and upstream at <https://github.com/taglib/taglib/releases/tag/v2.3.2>. MusicLib builds a subset of its modules (the `build-tags` stage of the `Dockerfile`) and makes no modification to it.
- `musiclib-tags` is MusicLib's own program (MIT, `native/musiclib-tags/`) linked with TagLib; its complete source and build files are in the MusicLib repository.

## utfcpp

- Copyright 2006-2022 Nemanja Trifunovic.
- License: Boost Software License 1.0. Text: `licenses/utfcpp/LICENSE`.
- Source: bundled with TagLib, in `3rdparty/utfcpp/` of `taglib-2.3.2.tar.gz`.

## GNU C Library, GCC runtime libraries

`ffmpeg`, `ffprobe` and `musiclib-tags` are fully static: they contain parts of the GNU C Library and of GCC's runtime libraries, taken unmodified from Debian's `glibc` 2.41-12+deb13u3 and `gcc-14` 14.2.0-19 (the packages `libc6-dev`, `libgcc-14-dev` and `libstdc++-14-dev` of the build image, `GO_IMAGE` in the `Dockerfile`, pinned by digest). `ffmpeg` and `ffprobe` are C programs and contain no libstdc++.

- **GNU C Library**: Copyright (C) 1991-2025 Free Software Foundation, Inc. and others. License: GNU LGPL version 2.1 or later; a few parts are under other permissive licenses, all listed in Debian's copyright file, which is in the image as `/usr/share/doc/libc6/copyright` (the image's own `libc6` package, 2.41-12+deb13u4, of the same upstream release). LGPL-2.1 text: `/usr/share/common-licenses/LGPL-2.1` in the image; the same license as `licenses/ffmpeg/COPYING.LGPLv2.1`, which differs only in the FSF's address. Source: Debian's source package, at <https://snapshot.debian.org/package/glibc/2.41-12%2Bdeb13u3/>. The programs that link it are free software with their complete source available (FFmpeg above; `musiclib-tags` in the MusicLib repository) together with their build instructions, so you can rebuild and relink them with a modified C library.
- **libstdc++ and libgcc**: Copyright (C) Free Software Foundation, Inc. License: GNU GPL version 3 or later with the GCC Runtime Library Exception version 3.1, which allows these libraries to be combined with programs under any license. Text: `/usr/share/doc/gcc-14-base/copyright` in the image (the same package version). Source: <https://snapshot.debian.org/package/gcc-14/14.2.0-19/>.

## Go

`musiclibd` is compiled with Go 1.25.14 (`go version -m /usr/local/bin/musiclibd` lists what it contains).

- **Go standard library and runtime**: Copyright 2009 The Go Authors. License: BSD-3-Clause, with Google's patent grant. Texts: `licenses/go/LICENSE`, `licenses/go/PATENTS`. Source: <https://go.dev/dl/go1.25.14.src.tar.gz>.
- **Go modules**, each at the version pinned in `go.sum`; texts under `licenses/go-modules/<module path>/`:

| Module | Version | License | Copyright |
|---|---|---|---|
| github.com/google/uuid | v1.6.0 | BSD-3-Clause | Copyright (c) 2009,2014 Google Inc. |
| github.com/jackc/pgpassfile | v1.0.0 | MIT | Copyright (c) 2019 Jack Christensen |
| github.com/jackc/pgservicefile | v0.0.0-20240606120523-5a60cdf6a761 | MIT | Copyright (c) 2020 Jack Christensen |
| github.com/jackc/pgx/v5 | v5.11.0 | MIT | Copyright (c) 2013-2021 Jack Christensen |
| github.com/jackc/puddle/v2 | v2.2.2 | MIT | Copyright (c) 2018 Jack Christensen |
| github.com/mfridman/interpolate | v0.0.2 | MIT | Copyright (c) 2014-2017 Buildkite Pty Ltd; Copyright (c) 2023 Michael Fridman |
| github.com/pressly/goose/v3 | v3.27.0 | MIT | Original work Copyright (c) 2012 Liam Staskawicz; Modified work Copyright (c) 2016 Vojtech Vitek |
| github.com/sethvargo/go-retry | v0.3.0 | Apache-2.0 | Seth Vargo (the module has no NOTICE file) |
| go.uber.org/multierr | v1.11.0 | MIT | Copyright (c) 2017-2021 Uber Technologies, Inc. |
| golang.org/x/sync | v0.22.0 | BSD-3-Clause (+ PATENTS) | Copyright 2009 The Go Authors |
| golang.org/x/sys | v0.47.0 | BSD-3-Clause (+ PATENTS) | Copyright 2009 The Go Authors |
| golang.org/x/text | v0.41.0 | BSD-3-Clause (+ PATENTS) | Copyright 2009 The Go Authors |

The sources of these modules are on <https://proxy.golang.org/> at the versions above.

## Hanken Grotesk

- Copyright 2021 The Hanken Grotesk Project Authors (<https://github.com/marcologous/hanken-grotesk>).
- License: SIL Open Font License 1.1. Text: `web/OFL.txt`, also embedded in `musiclibd` and served next to the fonts.
- The fonts are `web/hanken-grotesk-v12-latin.woff2` and `web/hanken-grotesk-v12-latin-ext.woff2`, unmodified.
- The "MusicLib" wordmark of the web pages is a drawing made from the letterforms of Bricolage Grotesque (SIL Open Font License 1.1); no font file of it is distributed.

## Debian packages

The image is based on `debian:trixie-20260918-slim` (pinned by digest as `RUNTIME_IMAGE` in the `Dockerfile`), built from Debian's archive as of 2026-09-18. The `runtime` stage adds `postgresql-client-17` 17.11-0+deb13u1 and its dependencies from Debian's dated snapshot:

- <http://snapshot.debian.org/archive/debian/20260926T000000Z/> (trixie main)
- <http://snapshot.debian.org/archive/debian-security/20260926T000000Z/> (trixie-security main)

Each package's copyright and license terms are in the image at `/usr/share/doc/<package>/copyright` (the slim image keeps these files), and the common license texts in `/usr/share/common-licenses/`. `dpkg-query -W` in the image lists every package with its version; the image's SBOM, attached to it in the registry, lists them too. The exact source of any package version is available from Debian's snapshot archive at `https://snapshot.debian.org/package/<source package>/<version>/`; the archives of the base image are <http://snapshot.debian.org/archive/debian/20260918T000000Z/> and <http://snapshot.debian.org/archive/debian-security/20260918T000000Z/>.

## Not in the image

LAME 3.100, nasm, CMake and Chromium are used only to build or test MusicLib: none of them is in the image, and `ffmpeg` is not linked with LAME (`ffmpeg -buildconf`).
