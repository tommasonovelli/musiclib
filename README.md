![Vibrance MusicLib](docs/assets/banner.jpg)

# Vibrance MusicLib

MusicLib keeps your music collection tidy without ever touching your original files.

1. **Import** your album folders. MusicLib copies each file into its own store and never changes, moves or deletes the source.
2. **Fix the metadata in your browser**: artists, album titles, years, genres, track titles and numbers, covers, lyrics and extra files such as booklets.
3. **MusicLib writes a clean library folder**: one folder per artist and per album, consistently named files, complete tags and the cover in every track. Point your own music player at it.

Each edit regenerates only the album it affects, and your original copies are kept forever. MusicLib is a self-hosted web app for one person. It runs in Docker on a Linux machine.

![The Library page of MusicLib: a grid of album covers with titles, artists and years, and a sidebar with Library, Import, Activity, Trash and Needs attention](docs/assets/screenshot-library.jpg)

![The album page of MusicLib: the cover, title, artist, year and genre of an album, its tracklist with durations and a lyrics mark, and its extra files](docs/assets/screenshot-album.jpg)

<sub>The artists, albums and covers in these screenshots are invented.</sub>

## Contents

- [What it does and doesn't do](#what-it-does-and-doesnt-do)
- [Requirements](#requirements)
- [Install](#install)
- [First steps](#first-steps)
- [How folders become albums](#how-folders-become-albums)
- [The library folder](#the-library-folder)
- [Where your data lives](#where-your-data-lives)
- [Configuration](#configuration)
- [Access and security](#access-and-security)
- [Backup, check and restore](#backup-check-and-restore)
- [Updating](#updating)
- [Everyday commands](#everyday-commands)
- [Building from source](#building-from-source)
- [License](#license)

## What it does and doesn't do

**It does:**

- import album folders, including multi-disc albums, and report what it imported and what it skipped, and why;
- read and write tags of **FLAC**, **MP3** and **M4A** (AAC or ALAC) files;
- manage **JPEG and PNG covers**, **LRC lyrics** and **extra files** (booklets, scans, logs);
- let you search the library, move albums to the trash and restore them;
- verify its work: every copy is checked against the original, and the audio of each track is checked before and after its tags are written;
- check, back up and restore the whole collection.

**It doesn't:** play or stream music (use your own player), convert audio between formats, look up metadata or recognize music online, watch folders for new files (you start each import), manage user accounts (there is one user and no login), or edit arbitrary tags. The interface is in English.

## Requirements

MusicLib is designed to run inside Docker. The supported setup is:

- **Ubuntu 24.04 or later** on an **amd64** (x86-64) machine;
- **Docker Engine** with the **Compose v2** plugin (`docker compose`), installed from Docker's own packages, and a user that can run `docker` without `sudo`;
- **local ext4 storage** for MusicLib's data: Docker's own storage (`/var/lib/docker`, where named volumes live), or the host folder you choose for it;
- `curl` and `openssl` (`sudo apt install curl openssl` if they are missing).

MusicLib checks the storage at every start and refuses what it can't use safely. **Not supported:** Docker Desktop (on any system), NAS and network filesystems (NFS, SMB), filesystems other than ext4, and ARM machines (Raspberry Pi, Apple Silicon).

To check the filesystem of Docker's storage:

```sh
findmnt -no FSTYPE -T /var/lib/docker    # must print ext4
```

## Install

Paste this block into a terminal. It creates `~/musiclib`, downloads the two files of the latest release, writes a random database password into `.env`, and starts MusicLib:

```sh
mkdir -p ~/musiclib/import && cd ~/musiclib
[ -e compose.yaml ] || curl -fsSLO https://github.com/tommasonovelli/vibrance-musiclib/releases/latest/download/compose.yaml
[ -e .env ] || { curl -fsSL -o .env https://github.com/tommasonovelli/vibrance-musiclib/releases/latest/download/env.example && chmod 600 .env && sed -i "s/^POSTGRES_PASSWORD=.*/POSTGRES_PASSWORD=$(openssl rand -hex 32)/" .env; }
docker compose up -d --wait
```

When the last command returns, open **<http://127.0.0.1:8080>** in a browser on the same machine. On a machine without a desktop, see [Access and security](#access-and-security).

Put your albums in **`~/musiclib/import`**, then import them from the **Import** page.

Good to know:

- The block is safe to paste twice: it never replaces an existing `compose.yaml` or `.env`.
- `docker compose up -d --wait` pulls the images the first time, then waits until MusicLib reports that it is ready. MusicLib starts again by itself after a reboot, unless you stopped it.
- **The database password.** `compose.yaml` comes with a default password, `musiclib`, and it is better not to keep it: the block writes a random one into `.env`, which replaces the default. If you install without the block, set `POSTGRES_PASSWORD` in `.env` **before the first start**: PostgreSQL reads it only once, when it creates its database. Changing it in `.env` later doesn't change the database's password, and MusicLib can no longer connect (see [docs/operations.md](docs/operations.md#first-start) to change it).
- `.env` holds that password: keep it private (the block makes it readable by you only).
- To keep your library in a folder of your choice instead of Docker's storage, read the next section **before** the first start.

### Keeping the data in a host folder

By default MusicLib keeps its data in Docker named volumes. You can put it in folders of the host instead: for example the library on a data disk, and backups on a second disk. Decide this before the first start, because MusicLib pairs its database with its data folder when it first starts.

1. Paste the install block **without its last line** (`docker compose up -d --wait`).
2. Create the folders: absolute paths, on **local ext4**, **empty**, and **owned by uid 1000** (the user MusicLib runs as):

   ```sh
   sudo mkdir -p /srv/musiclib/data /mnt/backup/musiclib
   sudo chown 1000:1000 /srv/musiclib/data /mnt/backup/musiclib
   findmnt -no FSTYPE -T /srv/musiclib/data    # must print ext4
   ```

3. Add these lines to `~/musiclib/.env`, with your folders; the last one only if the music you want to import isn't in `~/musiclib/import`:

   ```text
   MUSICLIB_DATA=/srv/musiclib/data
   MUSICLIB_BACKUP=/mnt/backup/musiclib
   MUSICLIB_IMPORT=/srv/music
   ```

4. Start: `docker compose up -d --wait`.

The import folder is mounted **read-only**: it must exist before MusicLib starts, and its files must be readable by uid 1000 (files readable by everyone are fine). The backup folder must not be inside the data folder. With `MUSICLIB_DATA` set, the generated library is at `/srv/musiclib/data/library`, ready for your player.

## First steps

- **Import.** The Import page shows the contents of your import folder. Open a folder, or stay at the top to take everything, and choose **Import everything in …**. MusicLib finds the albums inside, imports each one completely or not at all, and lists what was imported, what was skipped (for example an album you had already imported) and what needs your attention, with the reason. Leave the source files where they are until the import has finished.
- **Edit.** Open an album from the **Library** to change its title, artist, year, genre and compilation flag, the tracks (number, title, artist, genre, disc), the cover, lyrics and extra files. When you save, MusicLib regenerates that album's folder in the library. If two browser tabs edit the same album, the second save is refused instead of overwriting the first.
- **Activity** shows the work in progress: scans, imports and library updates. Failed work stays there, with its reason, until you retry or dismiss it. **Needs attention**, in the sidebar, lists the albums whose last update failed.
- **Trash.** Moving an album to the trash removes it from the library folder; you can restore it at any time.

Everything the interface does is also available through a JSON HTTP API: see [docs/docker.md](docs/docker.md#the-api).

## How folders become albums

- A folder with audio files directly inside it is **one album**. Its subfolders without audio (scans, artwork) come along as extra files.
- A folder whose audio is only in subfolders named `CD1`, `CD2`, … or `Disc 1`, `Disc 2`, … is **one multi-disc album**.
- Other folders are searched for albums inside them. Files that belong to no album are listed as not imported.
- A folder with audio both directly inside it and in its subfolders is ambiguous: it isn't imported, and the report says why. So is an album with audio in a format MusicLib doesn't support (OGG or WAV, for example), with the name of the file.
- The cover is `cover.*`, `folder.*` or `front.*` at the album's root, or else a picture embedded in the tracks: JPEG or PNG, up to 20 MB and 40 megapixels. A cover file is also kept as an extra file.
- An `.lrc` file with the same name as a track, in the same folder, becomes that track's lyrics. Every other file is kept as an extra file of the album.
- Importing the same files again is recognized and skipped. MusicLib never merges albums or artists on its own.

## The library folder

MusicLib generates the library folder from the database and the originals. A generated album looks like this:

```text
library/
  Miles Davis/
    Kind of Blue/
      .musiclib.json
      cover.jpg
      01 - So What.flac
      01 - So What.lrc
      02 - Freddie Freeloader.flac
      Extras/
        booklet.pdf
        cover.jpg
```

- The layout is always `Artist/Album/NN - Title.ext`, with `Disc N/` folders for a multi-disc album, `cover.jpg` or `cover.png` at the album's root, lyrics next to their track and every extra file under `Extras/`.
- Names are cleaned up so that they also work on Windows filesystems: characters such as `/ \ : * ? " < > |` become `_`, and very long names are shortened. Two albums or tracks that would end up at the same path are reported as a conflict for you to resolve; nothing is dropped or renamed silently.
- MusicLib writes the title, artist, album artist, album, track and disc numbers and totals, year, genre, compilation flag and cover into every track. Any other tag the files already had is kept as it is.
- An album's folder is replaced as a whole: your player sees either the old or the new version of an album, never a half-written one.
- `.musiclib.json` is a receipt that MusicLib uses to check its own output.

**Treat the library folder as read-only.** Change metadata in MusicLib: manual changes to files in `library/` are replaced by the next update of that album. If files there were changed or deleted anyway, **Rebuild the library folder** (Activity → Advanced) regenerates all of it.

## Where your data lives

| Data | Where | Default |
|---|---|---|
| Your source music | the import folder, read-only | `~/musiclib/import` |
| Catalog: names, edits, work queue | PostgreSQL database | volume `musiclib_pgdata` |
| Originals: an unchanged copy of every imported file | `originals/` in the data folder | volume `musiclib_musiclib-data` |
| The generated library | `library/` in the data folder | volume `musiclib_musiclib-data` |
| Temporary work | `work/` in the data folder | volume `musiclib_musiclib-data` |
| Backups | the backup folder | volume `musiclib_musiclib-backup` |

With the default named volumes, the data is inside Docker's storage, readable only with `sudo`: the library is at `/var/lib/docker/volumes/musiclib_musiclib-data/_data/library`. Set `MUSICLIB_DATA` (see [above](#keeping-the-data-in-a-host-folder)) to have it at a path of your choice.

**Disk space.** Plan for about **twice the size of the music you import**, one copy for the originals and one for the library, plus room for work in progress and at least 1 GiB free. Each backup needs about the size of the originals again, on the backup disk. MusicLib never deletes an original: trashing an album or deleting a track frees no space.

## Configuration

All settings are in `~/musiclib/.env`. After changing it, apply it with `docker compose up -d --wait`.

| Variable | Default | Meaning |
|---|---|---|
| `POSTGRES_PASSWORD` | `musiclib`: better change it | Database password; the install block writes a random one. Read only when the database is first created: set it before the first start. |
| `PUBLIC_ORIGIN` | `http://127.0.0.1:8080` | The exact address you open in the browser: scheme, host and port. |
| `MUSICLIB_BIND` | `127.0.0.1` | Host address the web interface listens on. Loopback: this machine only. |
| `MUSICLIB_PORT` | `8080` | Host port of the web interface. Keep it equal to the port in `PUBLIC_ORIGIN`. |
| `MUSICLIB_IMPORT` | `./import` | Folder with the music to import, mounted read-only. It must exist. |
| `MUSICLIB_DATA` | `musiclib-data` (named volume) | Or an absolute path of an empty ext4 folder owned by uid 1000. Set it before the first start. |
| `MUSICLIB_BACKUP` | `musiclib-backup` (named volume) | Or an absolute path of a folder owned by uid 1000, preferably on another disk. |
| `WORKERS` | empty: the number of CPUs, at most 4 | How many imports and album updates run at once, 1 to 16. |
| `MUSICLIB_UID`, `MUSICLIB_GID` | `1000` | The user and group MusicLib runs as. Another value needs `MUSICLIB_DATA` and `MUSICLIB_BACKUP` as folders owned by it. |

Every setting is described in [docs/operations.md](docs/operations.md) and [docs/docker.md](docs/docker.md#variables).

## Access and security

**MusicLib has no login.** Anyone who can reach its address can change or delete your catalog. It is meant for one person on a trusted machine or home network.

- By default it listens only on `127.0.0.1`: it can be opened only from the machine it runs on.
- `PUBLIC_ORIGIN` must match the address in the browser exactly. `http://localhost:8080` and `http://127.0.0.1:8080` are different addresses: with the default setting, only the second one works.
- **From another device on your network**, set the machine's LAN address in both settings: in `.env`, change these two lines as below (`MUSICLIB_BIND` starts with a `#`: remove it), then run `docker compose up -d --wait`. Give the machine a fixed address in your router first.

  ```text
  MUSICLIB_BIND=192.168.1.20
  PUBLIC_ORIGIN=http://192.168.1.20:8080
  ```

- **From your computer to a server without a desktop**, an SSH tunnel keeps the default settings: run `ssh -L 8080:127.0.0.1:8080 you@server` and open `http://127.0.0.1:8080` on your computer.
- **The database** has a default password, `musiclib`, unless you set `POSTGRES_PASSWORD` in `.env` before the first start; the install block does it for you with a random one. The database publishes no port, so other machines can't reach it, but a password of your own is still better.
- **Never expose MusicLib directly to the Internet.** For remote access, put it behind a reverse proxy that requires authentication, and set `PUBLIC_ORIGIN` to the proxy's address.

## Backup, check and restore

These commands run from `~/musiclib`. Each one stops MusicLib, runs a one-off maintenance command while the database keeps running, and starts MusicLib again.

**Back up** the catalog and all the originals:

```sh
docker compose stop app
docker compose run --rm --no-deps app backup --to "/backup/$(date +%F-%H%M)"
docker compose start app
```

- The backup is a new folder under `/backup`, the backup folder (`MUSICLIB_BACKUP`), named after the date and time, such as `2026-09-29-2130`. It is a full copy, verified while it is written, and never overwrites an existing backup: a name that already exists is refused (`backup_exists`).
- The generated library isn't saved (it is regenerated after a restore), and neither is your import folder.
- The default backup volume is on the same disk as your data: it protects against mistakes, not against a disk failure. Set `MUSICLIB_BACKUP` to a folder on another disk, keep several backups, and try a restore now and then.
- To copy a backup elsewhere, use `sudo cp -a`: some of its files are readable only by uid 1000.
- If a backup fails, it leaves a `.musiclib-backup-*.tmp` folder, which you can delete.

**Check** the library: every original, and every file of the library, against its recorded hash (without `--deep`, a quicker check of sizes and presence):

```sh
docker compose stop app
docker compose run --rm --no-deps app doctor --deep
docker compose start app
```

Doctor only reads and never repairs. It exits with 0 when nothing is damaged, 1 when it found damage (each finding comes with advice), and 2 when it refused to run.

**Restore** a backup into a new, empty installation, for example on a new machine after a disk failure. Restore never overwrites an existing catalog or library.

1. Paste the install block **without its last line**.
2. In `.env`, set `MUSICLIB_BACKUP` to the folder that contains your backup (and `MUSICLIB_DATA` to an empty folder, if you keep the data in a host folder).
3. Run:

   ```sh
   docker compose up -d --wait postgres
   docker compose run --rm --no-deps app restore --from "/backup/2026-09-29-2130"
   docker compose up -d --wait
   ```

Start only the database before the restore, as above: the first start of MusicLib would initialize the empty data folder, and the restore would then refuse it. After the restore, MusicLib regenerates the library folder by itself; Activity shows the progress. If a restore fails, start again with a new, empty database and data folder. To restore on a machine that still has an installation, and for every error code, see [docs/operations.md](docs/operations.md#restore-use-new-empty-destinations).

> **Never run `docker compose down -v`: it deletes your library, its database and its backup volume.** To stop MusicLib, use `docker compose stop`.

## Updating

Back up first: an update can upgrade the database, and there is no way back except restoring that backup. Your settings are in `.env`, so the new release's `compose.yaml` simply replaces the old one. (If you changed the database password in `compose.yaml` instead of `.env`, move it to `.env` first: the new file comes with the default.)

```sh
cd ~/musiclib
docker compose stop app &&
docker compose run --rm --no-deps app backup --to "/backup/before-update-$(date +%F-%H%M)" &&
curl -fsSLO https://github.com/tommasonovelli/vibrance-musiclib/releases/latest/download/compose.yaml &&
docker compose pull && docker compose up -d --wait
```

The commands are chained: if the backup fails, nothing is updated and MusicLib stays stopped until you fix the cause (`docker compose start app` restarts the old version). You can also change the version on the `image:` line of `compose.yaml` by hand. Check the running version with `docker compose run --rm --no-deps app version`.

At its first start, the new version updates the database. **Downgrading isn't possible**: an older version refuses a database that a newer one has updated. To go back, restore the backup made before the update, using the older version.

## Everyday commands

Run them from `~/musiclib` (from the clone's folder for a [source build](#building-from-source)):

| Task | Command |
|---|---|
| Stop MusicLib | `docker compose stop` |
| Start it again | `docker compose up -d --wait` |
| Show its status | `docker compose ps` |
| Read its log | `docker compose logs --tail=100 app` |
| Check that it is ready | `curl -f http://127.0.0.1:8080/health/ready` |
| Show its version | `docker compose run --rm --no-deps app version` |

After the LAN change of [Access and security](#access-and-security), MusicLib no longer listens on `127.0.0.1`: check it with the LAN address, such as `curl -f http://192.168.1.20:8080/health/ready`.

If MusicLib refuses to start, its log names the problem with a stable code, and nothing is repaired or deleted automatically. The codes and what to do about each are in [docs/operations.md](docs/operations.md#security-and-troubleshooting) and [docs/docker.md](docs/docker.md#when-the-app-refuses-to-start).

## Building from source

The app image holds a Go server with its web interface built in, FFmpeg and a small TagLib-based tag writer, all pinned to exact versions; PostgreSQL 17 runs in its own container, from its own pinned image. To build the app image from a clone of the repository instead of using the published one:

```sh
git clone https://github.com/tommasonovelli/vibrance-musiclib.git && cd vibrance-musiclib
cp .env.example .env && chmod 600 .env
sed -i "s/^POSTGRES_PASSWORD=.*/POSTGRES_PASSWORD=$(openssl rand -hex 32)/" .env
echo 'COMPOSE_FILE=compose.dev.yaml' >> .env
mkdir -p import
docker compose up -d --build --wait
```

`COMPOSE_FILE=compose.dev.yaml` makes every `docker compose` command in this README use the source build: run them from the clone's folder, `vibrance-musiclib`, instead of `~/musiclib`. The first build compiles its pinned dependencies and takes several minutes. A source build and a published-image installation are the same Compose project, `musiclib`, with the same volumes: on one machine they share the same data.

To contribute, read [CONTRIBUTING.md](CONTRIBUTING.md). Operations in depth are in [docs/operations.md](docs/operations.md), and development, tests and pinned dependencies in [docs/docker.md](docs/docker.md).

## License

MusicLib's code and documentation are released under the [MIT License](LICENSE), copyright 2026 tommasonovelli.

The sun logo is the author's artwork and is **not** covered by the MIT License: you may keep it in unmodified copies, but a modified version you distribute must use its own symbol. See [LOGO.md](LOGO.md).

Third-party software and assets keep their own licenses, among them the Hanken Grotesk font, under the [SIL Open Font License](web/OFL.txt). [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) lists everything the Docker image contains, with versions, licenses and where to get the sources; it is also in the image, under `/usr/share/doc/musiclib/`. Each GitHub release attaches it together with the source tarballs of FFmpeg and TagLib.
