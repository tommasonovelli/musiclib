# Changelog

All notable changes to Vibrance MusicLib are recorded in this file, in the format of [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/).
Versions follow [Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [1.2.0] - 2026-10-01

Upgrading from 1.1.0 needs no extra step: take the new `compose.yaml` and update as usual. The database schema does not change.

### Added

- **Empty trash**: a button on the **Trash** page deletes the albums in the trash for good, after a confirmation. Only the catalog entries go: the original files stay in MusicLib, so no disk space is freed, and an album can come back only by importing its folder again or restoring a backup. An album still being removed from the library folder stays in the trash until its removal is done; the page says how many are waiting. Nothing is ever deleted automatically. Scripts use `POST /api/trash/empty`.
- **Add tracks** to an album: on the album's page, **Add tracks** chooses audio files, or drop them anywhere on the page except the cover. Each file is checked exactly as an import checks a track (fully decoded, tags read), then added in order: at the disc and number of its tags when that place is free, otherwise after the last track of the last disc. A file already in the album is refused, and the first refused file stops the rest and says why. A track can be up to 2 GiB. Scripts use `POST /api/albums/<id>/tracks?name=<file name>`. In **Needs attention**, an album whose title the artist already has now suggests opening that album and adding the tracks there.
- **Move tracks** to another album: **Move to another album…** in a track's menu, or **Move all tracks to another album…** in the album's menu, then search for the other album and choose it. Each track keeps its disc and number when free there, otherwise it goes after the last track, and takes its title, artist, genre and lyrics with it; the cover and the extra files stay. Both albums are updated in the library. An album left without tracks goes to the trash, cannot be restored, and is deleted with **Empty trash**. Scripts use `POST /api/albums/<id>/move-tracks`.

### Changed

- The UI budget of the contributor rules is now 100 KB of CSS and JavaScript (was 90 KB).

### Fixed

- MP3 files whose gapless header declares an end padding shorter than the decoder's delay, as written by FFmpeg 1.0 to 3.1, are no longer refused as damaged (`decoded … frames, the container declares …`). MusicLib now expects exactly the length such a header implies, so a file that really lost audio is still refused. After updating, retry the failed imports.

## [1.1.0] - 2026-09-30

**Upgrading from 1.0.0: MusicLib now asks for a password, and does not start without one.** Before updating, add one to `.env` (this line adds a random one unless `.env` already sets it):

```sh
grep -q '^MUSICLIB_PASSWORD=.' .env || { sed -i '/^MUSICLIB_PASSWORD=/d' .env && printf '\nMUSICLIB_PASSWORD=%s\n' "$(openssl rand -base64 24)" >> .env; }
```

Then take the new `compose.yaml`, which passes it to MusicLib, and update as usual. Read the password with `grep MUSICLIB_PASSWORD .env`.

### Added

- **Password sign-in**: one password, `MUSICLIB_PASSWORD` in `.env` (at least 12 characters), protects the web interface and the API, with a **Sign out** button at the foot of the sidebar. The sign-in page shows the MusicLib logo above the form, on the sidebar's violet glow. A session lasts 30 days and ends when MusicLib restarts. The install block generates the password; the offline commands (`doctor`, `backup`, `restore`, `rebuild`) don't need it. Scripts sign in with `POST /login`; without a session the API answers `401 login_required`. Over plain HTTP the password crosses the network in clear: from other devices, use a network you trust, or HTTPS.
- **HTTPS and a domain name**: the operations guide shows how to put Caddy in front of MusicLib, on the same machine or in Docker next to it, for a public name or for a name on the home network only, and what any other reverse proxy needs. MusicLib itself needs only `PUBLIC_ORIGIN` set to the proxy's `https://` address.
- **The data in a host folder**: the operations guide shows how to keep MusicLib's data in a folder of your choice, how to move an existing installation there from Docker's storage without losing anything or regenerating the library, and what happens to files other programs add to the library folder.
- **Theme switch**: a button at the foot of the sidebar cycles between the system's theme, light and dark. The choice is remembered by the browser.
- **A violet glow with a light grain** in the lower part of the sidebar, fading slowly upward, in both themes. The top of the sidebar stays plain.
- **The album page takes the colours of its cover**: the album's head lies on its own cover, blurred into a field of its colours with a light grain, fading into the page before the tracks. Only albums with a cover glow; a new cover changes it at once.

### Changed

- The sidebar and the page share one background: white in the light theme (the sidebar was light grey) and black in the dark theme (the page was dark grey, `#1c1c1e`).
- A thin line separates the sidebar from the page on wide screens, in both themes.
- With the sidebar expanded, the buttons at its foot show their names next to their icons, like the entries above them: **Sign out**, the theme switch (**Theme: System**, **Light** or **Dark**) and **Collapse sidebar**. Collapsed, they are icons with a tooltip, as before.
- When MusicLib cannot read or write its data folder, the error at startup (`volume_permission`) names the user and group it runs as and the `chown` command that fixes a host folder.
- The documentation is reorganized: `docs/operations.md` is one guide, from installation to troubleshooting and the API, for whoever runs MusicLib; `docs/docker.md` is for developers only.
- The README guides the installation step by step, without opening another file: checking the machine, the optional data folder, the install block, signing in, the settings most people change and how to apply them, the first import and where the library is; then the everyday commands, backups and updates, with links into the guide.
- The maintenance scripts (`scripts/doctor.sh`, `rebuild.sh`, `backup.sh`, `restore.sh`) refuse, before stopping anything, when the Compose project's `app` or `postgres` container was created in another folder than the clone's, such as an installation in `~/musiclib`, and print the commands to run by hand there. They used to stop that installation's app and run the command with the clone's settings.

### Fixed

- `backup --to` refuses a destination that is not a new folder under `/backup` (`backup_outside_backup`, exit 2) before doing anything. A path such as `/tmp/x` used to be written inside the one-off container and lost when it exited, although the backup reported success.
- `scripts/rebuild.sh` checks the store id before it stops the app: a mistyped id no longer leaves the app stopped.
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

[Unreleased]: https://github.com/tommasonovelli/vibrance-musiclib/compare/v1.2.0...HEAD
[1.2.0]: https://github.com/tommasonovelli/vibrance-musiclib/releases/tag/v1.2.0
[1.1.0]: https://github.com/tommasonovelli/vibrance-musiclib/releases/tag/v1.1.0
[1.0.0]: https://github.com/tommasonovelli/vibrance-musiclib/releases/tag/v1.0.0
