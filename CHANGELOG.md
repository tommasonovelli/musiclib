# Changelog

All notable changes to Vibrance MusicLib are recorded in this file, in the format of [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/).
Versions follow [Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Theme switch**: a button at the foot of the sidebar cycles between the system's theme, light and dark. The choice is remembered by the browser.
- **A violet glow with a light grain** in the lower part of the sidebar, fading slowly upward, in both themes. The top of the sidebar stays plain.
- **The album page takes the colours of its cover**: the album's head lies on its own cover, blurred into a field of its colours with a light grain, fading into the page before the tracks. Only albums with a cover glow; a new cover changes it at once.

### Changed

- The page background of the dark theme is darker (`#0f0f11`, was `#1c1c1e`), between the black sidebar and the old grey.

### Fixed

- Scrolling the Library no longer stutters when the head compacts: the head keeps a fixed height, so the album grid under it no longer shifts by a fraction of a pixel on every frame of the transition and every cover is no longer repainted.

## [1.0.0] - 2026-09-29

The first release of Vibrance MusicLib: a self-hosted web app, for one person, that keeps a music collection tidy without ever changing the original files.

### Added

- **Import of album folders** from a read-only import folder, multi-disc albums included (`CD1`, `Disc 1`, …). Each album is imported completely or not at all, and the report lists what was imported, what was skipped (such as an album imported before) and what needs attention, with the reason.
- **FLAC, MP3 and M4A (AAC or ALAC)** files, with their tags read on import and written in the library.
- **Metadata editing in the browser**: album title, artist, year, genre and compilation flag; track number, title, artist, genre and disc. When two browser tabs edit the same album, the second save is refused instead of overwriting the first.
- **Covers** (JPEG or PNG: from the album folder, embedded in the tracks or uploaded), **extra files** such as booklets, scans and logs, and **LRC lyrics** next to their tracks, all of which can also be changed in the browser.
- **A generated library folder** for any music player: `Artist/Album/[Disc N/]NN - Title.ext`, names that also work on Windows filesystems, complete tags and the cover in every track, and any other tag kept as it is. An album's folder is replaced as a whole, never left half-written, and two albums or tracks that would share a path are reported as a conflict, never renamed or dropped silently.
- **Originals are never modified**: the import folder is never changed, moved or deleted, and every imported file is kept as an unchanged copy. Every file in the library is checked against its original, and each track's audio is checked before and after its tags are written.
- **Search, Activity and Needs attention**: search albums and artists; follow scans, imports and library updates, and retry or dismiss failed work; see the albums whose last update failed.
- **Trash**: an album moved to the trash leaves the library folder and can be restored at any time.
- **Offline maintenance commands**: `doctor` checks the originals and the library and never repairs (`--deep` hashes every file); `backup` writes a verified full copy of the catalog and the originals and never overwrites an existing backup; `restore` restores a backup into a new, empty installation; `rebuild` regenerates the whole library folder, also available from Activity → Advanced.
- **A JSON HTTP API** for everything the interface does.
- **A Docker image** for linux/amd64, `ghcr.io/tommasonovelli/musiclib`, run with Docker Compose next to PostgreSQL 17, and a one-block install that writes a random database password into `.env`.

### Notes

- **Supported platform**: Ubuntu 24.04 or later on amd64, Docker Engine with the Compose v2 plugin, and local ext4 storage for MusicLib's data. Docker Desktop, NAS and network filesystems (NFS, SMB), other filesystems and ARM machines are not supported; MusicLib checks its storage at every start and refuses what it can't use safely.
- **There is no login.** MusicLib listens only on 127.0.0.1 by default: keep it there or on a trusted home network, and for remote access put it behind a reverse proxy that requires authentication. Never expose it directly to the Internet.
- **The database password defaults to `musiclib`.** The install block replaces it with a random one; if you install without the block, set `POSTGRES_PASSWORD` in `.env` before the first start, because PostgreSQL reads it only when it creates its database.
- **One user, one instance**: a single MusicLib instance per database and data folder.
- **Upgrades are one-way**: a new version can upgrade the database, and an older version refuses it afterwards. Back up before every update: going back to an older version means restoring that backup.
- **Third-party software**: `ffmpeg` and `ffprobe` in the image are under GPL-2.0-or-later; MusicLib's own code is under the MIT License. `THIRD_PARTY_NOTICES.md` lists every component of the image with its license, and is attached to this release together with the source tarballs of FFmpeg and TagLib.

[Unreleased]: https://github.com/tommasonovelli/vibrance-musiclib/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/tommasonovelli/vibrance-musiclib/releases/tag/v1.0.0
