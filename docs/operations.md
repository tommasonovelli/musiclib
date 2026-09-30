# Running Vibrance MusicLib

This guide is for whoever installs and runs MusicLib. Read it from top to bottom the first time: each section builds on the one before. Every command is explained, with what a good result looks like.

The examples use the same values everywhere, so you can copy them and change only what differs on your machine:

| Example | What it stands for |
|---|---|
| `~/musiclib` | the folder that holds `compose.yaml` and `.env`. Run every command of this guide from there (`cd ~/musiclib`). |
| `/srv/musiclib/data` | a host folder for MusicLib's data, if you choose one |
| `/mnt/backup/musiclib` | a host folder for backups, on another disk |
| `192.168.1.20` | the machine's address on your home network |
| `music.example.com` | a domain name for HTTPS |
| `2026-09-29-2130` | the name of a backup |

For building, testing and releasing MusicLib itself, see the [developer guide](docker.md).

## Contents

1. [What you need](#what-you-need)
2. [Install](#install)
3. [Configuration](#configuration)
4. [Where your files are](#where-your-files-are)
5. [The passwords](#the-passwords)
6. [Access from other devices](#access-from-other-devices)
7. [Importing and the Activity page](#importing-and-the-activity-page)
8. [Backups, restore and moving](#backups-restore-and-moving)
9. [Upgrading](#upgrading)
10. [Maintenance](#maintenance)
11. [Troubleshooting](#troubleshooting)
12. [The API](#the-api)

## What you need

- **Ubuntu 24.04 or later** on an **amd64** (x86-64) machine. It was verified on Ubuntu 26.04 with Docker Engine 29.1.3 and Compose 2.40.3.
- **Docker Engine** with the **Compose v2** plugin (`docker compose`), from Docker's own packages or from Ubuntu's (`sudo apt install docker.io docker-compose-v2`). Your user must be able to run `docker` without `sudo`: `sudo usermod -aG docker "$USER"`, then sign out and in again.
- **Local ext4 storage** for MusicLib's data: Docker's own storage (`/var/lib/docker`, where named volumes live), or the host folder you choose for it.
- `curl` and `openssl`: `sudo apt install curl openssl` if they are missing.
- Preferably a **second disk** for backups.

You do not install Go, PostgreSQL, FFmpeg or any other tool on the machine: everything runs in Docker.

**Not supported:** Docker Desktop (on any system), NAS and network filesystems (NFS, SMB), filesystems other than ext4, and ARM machines (Raspberry Pi, Apple Silicon). MusicLib checks its storage at every start and refuses what it cannot use safely.

Check the machine before you install:

```sh
uname -m                                   # must print x86_64
docker compose version                     # prints "Docker Compose version v2...."
findmnt -no FSTYPE -T /var/lib/docker      # must print ext4
```

- `uname -m` prints the processor architecture.
- `docker compose version` proves that Docker and the Compose plugin work for your user. If it says "permission denied", your user is not in the `docker` group yet.
- `findmnt` prints the filesystem type of Docker's storage. If it is not `ext4`, keep MusicLib's data in an ext4 [host folder](#the-data-in-a-host-folder) instead.

## Install

### First start

MusicLib is published as a Docker image, `ghcr.io/tommasonovelli/musiclib`, for Linux amd64. An installation needs only two files of a release: `compose.yaml` and `env.example` (the repository's `.env.example`, attached without its leading dot). Nothing is built.

Paste this block into a terminal:

```sh
mkdir -p ~/musiclib/import && cd ~/musiclib
[ -e compose.yaml ] || curl -fsSLO https://github.com/tommasonovelli/vibrance-musiclib/releases/latest/download/compose.yaml
[ -e .env ] || { curl -fsSL -o .env https://github.com/tommasonovelli/vibrance-musiclib/releases/latest/download/env.example && chmod 600 .env && sed -i "s/^POSTGRES_PASSWORD=.*/POSTGRES_PASSWORD=$(openssl rand -hex 32)/" .env && sed -i "s|^MUSICLIB_PASSWORD=.*|MUSICLIB_PASSWORD=$(openssl rand -base64 24)|" .env; }
docker compose up -d --wait
```

What each line does:

1. Creates `~/musiclib` and the import folder `~/musiclib/import` in it, then enters `~/musiclib`.
2. Downloads `compose.yaml`, the description of the two containers, unless the folder already has one.
3. Downloads `env.example` as `.env`, your settings file, unless the folder already has one. It makes `.env` readable by you only (`chmod 600`), then writes a random database password (`POSTGRES_PASSWORD`) and a random sign-in password (`MUSICLIB_PASSWORD`) into it, without printing them.
4. Starts MusicLib. The first time, Docker downloads the images. The command returns when MusicLib reports that it is ready.

The block is safe to paste twice: it never replaces an existing `compose.yaml` or `.env`. Without the block, set `POSTGRES_PASSWORD` in `.env` before the first start: PostgreSQL reads it only once, and [The database password](#the-database-password) explains how to change it later. To keep MusicLib's data in a folder of your choice instead of Docker's storage, read [The data in a host folder](#the-data-in-a-host-folder) **before** you run the last line.

What the block creates:

- in `~/musiclib`: `compose.yaml`, `.env` and the empty folder `import`;
- in Docker: the containers `musiclib` (the app) and `musiclib-db` (PostgreSQL 17), and the volumes `musiclib_db` (the database), `musiclib_data` (the originals and the library) and `musiclib_backup` (backups).

Check that it works:

```sh
docker compose ps                                    # both containers show "healthy"
curl -f http://127.0.0.1:8080/health/ready           # prints {"status":"ready"}
```

Then open **<http://127.0.0.1:8080>** in a browser on the same machine and sign in. To read the password, run:

```sh
grep MUSICLIB_PASSWORD .env                          # prints MUSICLIB_PASSWORD=...
```

The password is everything after `=`. On a machine without a desktop, open MusicLib from your computer through an [SSH tunnel](#through-an-ssh-tunnel) or [on your home network](#on-your-home-network-lan).

MusicLib starts again by itself after a reboot, unless you stopped it. If `docker compose up -d --wait` reports the app as unhealthy, read its log with `docker compose logs --tail=20 app` and look up the `code` in [Troubleshooting](#troubleshooting).

### Running from source

`compose.dev.yaml` builds the app from a clone of the repository, as the image `musiclib-app:local` (version `devel`), with the same PostgreSQL, settings and volumes as `compose.yaml`:

```sh
git clone https://github.com/tommasonovelli/vibrance-musiclib.git && cd vibrance-musiclib
cp .env.example .env && chmod 600 .env
sed -i "s/^POSTGRES_PASSWORD=.*/POSTGRES_PASSWORD=$(openssl rand -hex 32)/" .env
sed -i "s|^MUSICLIB_PASSWORD=.*|MUSICLIB_PASSWORD=$(openssl rand -base64 24)|" .env
echo 'COMPOSE_FILE=compose.dev.yaml' >> .env
mkdir -p import
docker compose up -d --build --wait
```

- `COMPOSE_FILE=compose.dev.yaml` in `.env` makes every plain `docker compose` command, and the [maintenance scripts](#the-maintenance-scripts), use the source build. Run every command of this guide from the clone's folder, `vibrance-musiclib`, instead of `~/musiclib`, and add `--build` where this guide runs `docker compose up -d --wait` after changing the sources.
- The first build compiles the pinned tools and takes several minutes.
- `compose.dev.yaml` has the same database password default as `compose.yaml`.
- A source build and a published-image installation are the same Compose project, `musiclib`, with the same containers and volumes: on one machine they share the same data.
- `compose.dev.yaml` fixes the image tag `musiclib-app:local`. A second clone that also builds from source retags the first clone's image: the first one then runs the second one's code the next time its app is created. Keep one source build per machine, or rebuild before you use the other.

### Everyday commands

Run them from `~/musiclib` (from the clone's folder for a source build):

| Task | Command | Good result |
|---|---|---|
| Stop MusicLib | `docker compose stop` | both containers stopped; nothing is deleted |
| Start it again | `docker compose up -d --wait` | returns when the app is healthy |
| Show its status | `docker compose ps` | `musiclib` and `musiclib-db` show `healthy` |
| Read its log | `docker compose logs --tail=100 app` | JSON lines; `"level":"ERROR"` lines carry a `code` |
| Follow its log | `docker compose logs -f app` | new lines appear as they come; Ctrl-C stops following |
| Check that it is ready | `curl -f http://127.0.0.1:8080/health/ready` | `{"status":"ready"}` |
| Show its version | `docker compose run --rm --no-deps app version` | `version: …` and `render_version: …` |
| Show the sign-in password | `grep MUSICLIB_PASSWORD .env` | `MUSICLIB_PASSWORD=…` |

After the [LAN change](#on-your-home-network-lan), MusicLib no longer listens on `127.0.0.1`: check it with the LAN address, such as `curl -f http://192.168.1.20:8080/health/ready`. With [Caddy in Docker](#caddy-in-docker) the app publishes no port, so use `docker compose ps` or `curl -f https://music.example.com/health/ready`.

> **Never run `docker compose down -v`: it deletes your library, its database and its backup volume.** To stop MusicLib, use `docker compose stop`. Plain `docker compose down` removes the containers (not the volumes); `docker compose up -d --wait` creates them again.

## Configuration

All settings are in `.env`, next to `compose.yaml`. `.env.example` lists them, with comments. After changing `.env`, apply it:

```sh
docker compose up -d --wait
```

Compose recreates the containers whose settings changed and keeps the volumes. Every setting has a default except `MUSICLIB_PASSWORD`.

| Variable | Default | Example | What it does |
|---|---|---|---|
| `POSTGRES_PASSWORD` | `musiclib` (also when empty) | the output of `openssl rand -hex 32` | The database password. Read only when the database is first created: see [The database password](#the-database-password). |
| `MUSICLIB_PASSWORD` | none: required | the output of `openssl rand -base64 24` | The sign-in password of the web interface and the API. See [The sign-in password](#the-sign-in-password). |
| `PUBLIC_ORIGIN` | `http://127.0.0.1:8080` (the port is `MUSICLIB_PORT`) | `http://192.168.1.20:8080`, `https://music.example.com` | The exact address you open in the browser: scheme, host and port, nothing else. MusicLib answers only requests for this address. |
| `MUSICLIB_BIND` | `127.0.0.1` | `192.168.1.20` | The host address MusicLib listens on. `127.0.0.1`: this machine only. |
| `MUSICLIB_PORT` | `8080` | `8081` | The host port MusicLib listens on. Change the port in `PUBLIC_ORIGIN` with it. |
| `MUSICLIB_DATA` | `data` (the named volume `musiclib_data`) | `/srv/musiclib/data` | Where MusicLib keeps its data (`/data` in the container): a named volume, or the absolute path of an empty ext4 folder owned by uid 1000. See [The data in a host folder](#the-data-in-a-host-folder). |
| `MUSICLIB_BACKUP` | `backup` (the named volume `musiclib_backup`) | `/mnt/backup/musiclib` | Where backups go (`/backup` in the container): a named volume, or the absolute path of a folder owned by uid 1000, preferably on another disk, never inside the data folder. |
| `MUSICLIB_IMPORT` | `./import` (`~/musiclib/import`) | `/srv/music` | The folder with the music to import, mounted read-only on `/import`. It must exist before MusicLib starts. |
| `MUSICLIB_UID`, `MUSICLIB_GID` | `1000`, `1000` | `1001`, `1001` | The user and group MusicLib runs as. Another value needs `MUSICLIB_DATA` and `MUSICLIB_BACKUP` as host folders owned by it, and `/import` readable by it. Never `0` (root): MusicLib refuses to run as root. |
| `WORKERS` | empty: the number of CPUs, at least 1 and at most 4 | `2` | How many scans, imports and album updates run at once, 1 to 16. |
| `COMPOSE_PROJECT_NAME` | `musiclib` (the `name:` of `compose.yaml`) | `musiclib-restore` | Compose's own setting: the project name, which prefixes the containers and volumes. Only for a [second installation](#restoring-next-to-an-existing-installation). |
| `COMPOSE_FILE` | `compose.yaml` | `compose.dev.yaml` | Compose's own setting: which Compose file plain `docker compose` commands use. Only for a [source build](#running-from-source). |

Example: to use port 8081 instead of 8080, change two lines of `.env` together, then apply:

```sh
sed -i 's|^#\?MUSICLIB_PORT=.*|MUSICLIB_PORT=8081|' .env
sed -i 's|^PUBLIC_ORIGIN=.*|PUBLIC_ORIGIN=http://127.0.0.1:8081|' .env
docker compose up -d --wait
```

The first `sed` sets `MUSICLIB_PORT` (the line starts with `#` in `.env.example`, which the `\?` also matches); the second sets `PUBLIC_ORIGIN`, because the `.env` of the install block names the port there too.

Rules for `.env`:

- **Unix (LF) line endings only.** A `.env` saved with Windows (CRLF) line endings is not supported: the invisible carriage return becomes part of every value, so MusicLib refuses its password (`password_invalid`) or its address (`config_invalid`). `grep -c $'\r' .env` prints `0` for a good file; to fix one, run `sed -i 's/\r$//' .env`.
- One `NAME=value` per line, without spaces around `=`. A line that starts with `#` is a comment: the setting keeps its default.
- It holds both passwords: keep it readable by you only (`chmod 600 .env`), and keep it out of shared folders and copies you give away.

### What Compose passes to the app

You do not set these: `compose.yaml` does, and they are listed here for reference.

- `DATABASE_URL` is `postgres://musiclib@postgres:5432/musiclib?sslmode=disable`, without a password. The password goes to the app as `PGPASSWORD`, which the PostgreSQL client (pgx) uses when the URL has none, so it need not be URL-safe. The offline `backup` and `restore` pass it to `pg_dump` and `pg_restore` the same way, never on a command line.
- `HTTP_ADDR` is `:8080` inside the container; `MUSICLIB_BIND` and `MUSICLIB_PORT` choose where it is published on the host.
- `PUBLIC_ORIGIN`, `WORKERS` and `MUSICLIB_PASSWORD` come from `.env`.
- The pool of database connections is `WORKERS` + 8.

Outside Compose, `DATABASE_URL` may be any PostgreSQL URI or pgx keyword/value connection string, with its password or with `PGPASSWORD`. The offline `backup` and `restore` run `pg_dump` and `pg_restore` with a password-free URI built from the host, port, user and database, plus `sslmode`, `sslrootcert`, `sslcert`, `sslkey`, `connect_timeout` and `application_name` of a URI; options that only pgx understands are not passed. So for TLS use the URI form: they accept a keyword/value string only without TLS (`sslmode=disable`), and encrypted client keys that need `sslpassword` are not supported.

## Where your files are

| Data | Where | Default |
|---|---|---|
| Your source music | the import folder (`MUSICLIB_IMPORT`), mounted read-only | `~/musiclib/import` |
| The catalog: names, edits, the work queue | the PostgreSQL database | the volume `musiclib_db` |
| The originals: an unchanged copy of every imported or uploaded file | `originals/` in the data folder | the volume `musiclib_data` |
| The generated library | `library/` in the data folder | the volume `musiclib_data` |
| Temporary work | `work/` in the data folder | the volume `musiclib_data` |
| Backups | the backup folder (`MUSICLIB_BACKUP`) | the volume `musiclib_backup` |

With the default named volumes, the data is inside Docker's storage and readable only with `sudo`: the library is at `/var/lib/docker/volumes/musiclib_data/_data/library`. To have it at a path of your choice, see [The data in a host folder](#the-data-in-a-host-folder).

Inside the container the paths are fixed: `/data` (the data folder), `/import` (read-only, must exist) and `/backup` (never inside `/data`). The data folder holds:

```text
/data/.lock              the lock that keeps a second MusicLib off this folder: empty, never removed
/data/.musiclib-store    store_id=<uuid>: pairs the folder with its database (written once, read-only)
/data/.maintenance       only while a rebuild or restore runs: blocks the start until it completes
/data/originals/         the originals, named by their SHA-256: never changed
/data/library/           the generated library: output only
/data/work/              temporary builds and files
```

- **Never delete or edit `.lock`, `.musiclib-store` or `.maintenance` by hand**, and never change anything in `originals/`.
- One MusicLib per data folder and database. A data folder is paired with its database at the first start; a missing `.musiclib-store` cannot be replaced by pointing an existing database at a new folder.
- `/import` is never changed, moved or deleted by MusicLib. Never point it at the data folder or into it.

### The library folder

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
- An album's folder is replaced as a whole: a program reading the library sees either the old or the new version of an album, never a half-written one.
- `.musiclib.json` is a receipt that MusicLib uses to check its own output.
- Folders are created with mode `0755` and files with `0644`, owned by the user MusicLib runs as (uid 1000): every user of the machine can read them, and only MusicLib can write them.

**Treat the library folder as read-only.** Change metadata in MusicLib, never in the library: see [Other programs and the library folder](#other-programs-and-the-library-folder).

### Disk space

- Plan for about **twice the size of the music you import**: one copy for the originals and one for the library, plus room for work in progress and at least 1 GiB free.
- Every import, album update and upload first reserves its estimated size, and starts only if the free space of the data folder, minus a 1 GiB safety margin and every other reservation, covers it. Otherwise it fails with `insufficient_space` (an upload answers `507`). A disk can still fill up during a write: then the published album and the originals stay intact. Free some space and retry.
- Each backup needs about the size of the originals again, plus the database dump, on the backup disk.
- MusicLib never deletes an original: trashing an album or deleting a track frees no space. Never delete originals to make room.

### The data in a host folder

By default `/data` is the named volume `musiclib_data`, inside Docker's storage and readable only with `sudo`. With `MUSICLIB_DATA` in `.env` it is a folder of your choice, and the generated library is `<folder>/library`, readable by every user of the machine. All of `/data` stays one folder on one filesystem: `library/` cannot be mounted somewhere else on its own, because MusicLib publishes an album with an atomic rename inside `/data`.

**Decide this before the first start**, because MusicLib pairs its database with its data folder when it first starts. An installation that already runs on the named volume can [move to a host folder](#moving-an-existing-installation-to-a-host-folder).

The data folder must be:

- **new, empty and used only by MusicLib** (never your music collection);
- on **local ext4**, with **nothing mounted inside it**;
- **owned by uid 1000** (or by `MUSICLIB_UID`:`MUSICLIB_GID`).

Create it yourself: if it does not exist, Docker creates it owned by root, and MusicLib does not start. The backup folder (`MUSICLIB_BACKUP`) must be owned by uid 1000 too (a backup folder owned by root makes every backup fail), and must not be inside the data folder; put it on another disk if you can.

1. Paste the [install block](#first-start) **without its last line** (`docker compose up -d --wait`).
2. Create the two folders and check them:

   ```sh
   sudo mkdir -p /srv/musiclib/data /mnt/backup/musiclib
   sudo chown 1000:1000 /srv/musiclib/data /mnt/backup/musiclib
   findmnt -no FSTYPE -T /srv/musiclib/data      # must print ext4
   ls -A /srv/musiclib/data                       # must print nothing (lost+found at the root of a disk is fine)
   ```

   `mkdir -p` creates the folders, `chown` gives them to uid 1000, `findmnt` prints the filesystem type and `ls -A` lists every entry, hidden ones included.
3. Add the two settings to `.env` and start:

   ```sh
   cat >> .env <<'EOF'
   MUSICLIB_DATA=/srv/musiclib/data
   MUSICLIB_BACKUP=/mnt/backup/musiclib
   EOF
   docker compose up -d --wait
   ls /srv/musiclib/data                          # library  originals  work
   ```

   If the music you want to import is not in `~/musiclib/import`, add `MUSICLIB_IMPORT=/srv/music` too. The import folder must exist, and its files must be readable by uid 1000 (files readable by everyone are fine).
4. Your library is at `/srv/musiclib/data/library`.

MusicLib keeps everything there: `originals/`, `library/`, `work/` and its marker files `.lock` and `.musiclib-store` (and `.maintenance` while a rebuild or restore runs). It never changes or deletes any other entry at the top of the folder, but it does not refuse a folder that already holds other files, so keeping it empty is up to you.

If MusicLib cannot read or write the folder, it does not start, and `docker compose logs --tail=20 app` shows a message such as:

```text
volume_permission: cannot open the volume lock .lock: this process runs as uid 1000 and gid 1000 (MUSICLIB_UID, MUSICLIB_GID) and must be able to read and write all of /data, on a read-write mount; if MUSICLIB_DATA is a host folder, run `sudo chown -R 1000:1000 <that folder>` on the host: fs_permission (openat2): data/.lock: permission denied
```

Run that command on the folder you set in `MUSICLIB_DATA`, here `sudo chown -R 1000:1000 /srv/musiclib/data`, then `docker compose restart app`.

### Moving an existing installation to a host folder

An installation that already runs on the named volume moves by copying the volume into the new folder while MusicLib is stopped. The database stays where it is, and nothing is regenerated. The folder needs room for the whole data volume (about twice the size of the music you imported), and MusicLib stays stopped while the copy runs.

1. Create the new folder with the `mkdir`, `chown`, `findmnt` and `ls -A` commands of [The data in a host folder](#the-data-in-a-host-folder), for the data folder only, but do not change `.env` yet.
2. Stop MusicLib and copy the volume into the folder:

   ```sh
   docker compose stop app
   docker compose run --rm --no-deps -v /srv/musiclib/data:/to --entrypoint flock app --verbose -n /data/.lock cp -a /data/. /to/
   ```

   The copy runs in MusicLib's own image, as MusicLib's user, so every file keeps its owner, modes and dates. `flock` first takes MusicLib's lock on the volume: it prints `flock: executing cp` and copies, or, if MusicLib or a maintenance command still uses the volume, it prints `flock: failed to get lock` and copies nothing. Then stop MusicLib and repeat. If `cp` prints an error (a full disk, for example), the copy is incomplete: do not go on, see "If the move fails" below.
3. Point MusicLib at the new folder, check the copy, and start MusicLib only if the check passes. `doctor --deep` reads every original and every library file against its recorded hash; it must end with `Doctor complete: no damage found.` (exit 0), and `&&` runs the start only then:

   ```sh
   sed -i '/^MUSICLIB_DATA=/d' .env && echo 'MUSICLIB_DATA=/srv/musiclib/data' >> .env
   docker compose run --rm --no-deps app doctor --deep && docker compose up -d --wait
   ```

   The start then checks what doctor does not: that MusicLib can read and write the folder, that it is one filesystem with nothing mounted inside it, and that an atomic rename exchange works there. MusicLib pairs the copy with its database as before (the `volume identified` line of the log shows the same `store_id`) and renders nothing again.
4. Your library is now at `/srv/musiclib/data/library`.

**If the move fails.** If the copy stops with an error, or doctor does not end with `no damage found`, do not start MusicLib on the new folder. Delete the `MUSICLIB_DATA` line from `.env` and start again on the old volume, which the move has not changed:

```sh
sed -i '/^MUSICLIB_DATA=/d' .env
docker compose up -d --wait
```

If doctor reported damage, check the old volume too, with MusicLib stopped: `docker compose stop app && docker compose run --rm --no-deps app doctor --deep`, then `docker compose start app`. If it reports the same damage, the damage was already there before the move: deal with it first (see [doctor](#doctor-check-the-library) and [Errors of the maintenance commands](#errors-of-the-maintenance-commands)), or every new try fails the same way.

Before another try, empty the new folder (a second copy cannot replace the read-only originals of the first one), check it with `ls -A` and repeat from step 2. Run this only on the new folder, which MusicLib has never started on, never on a folder MusicLib uses:

```sh
sudo find /srv/musiclib/data -mindepth 1 -delete
```

**The old volume.** The move leaves it as it was. As long as you have not imported or changed anything, you can go back by deleting the `MUSICLIB_DATA` line from `.env` and running `docker compose up -d --wait`. Keep the old volume until MusicLib has started on the new folder and a first [backup](#making-a-backup) of it has completed; then remove it. Its name is `musiclib_data`, or `<name>_data` if you set `COMPOSE_PROJECT_NAME=<name>` in `.env`; check it in the list first:

```sh
docker volume ls
```

then remove it by that name:

```sh
docker volume rm musiclib_data
```

Do not keep it for good: it carries the same identity as your database, so if the `MUSICLIB_DATA` line were ever lost, MusicLib would start on the old volume again, with the originals and the library as they were at the time of the copy.

To move to **another machine**, use a backup and a restore instead: see [Moving to another machine](#moving-to-another-machine).

### Other programs and the library folder

Other programs may **read** `library/`. They must **never write** in it: no tags, covers, index or thumbnail files, no renamed folders. MusicLib owns every album folder and replaces it as a whole at the album's next update. What another program leaves there:

- **A file or folder inside an album folder** (such as `folder.jpg`, `.DS_Store`, `Thumbs.db` or a thumbnail folder) is deleted at that album's next update (an edit, a new title, a manual render, **Rebuild the library folder**), together with the old version of the album folder.
- **A file inside an artist folder**, next to the album folders, or at the top of `library/`, is left alone, also by **Rebuild the library folder**. MusicLib removes an artist folder only when it is empty, so such a file keeps the artist folder in `library/` after its last album has gone to the trash or to another folder (after a new artist name, for example).
- **A folder created where an album is about to go** (after a new title, for example) blocks that album's update: Activity shows it as failed with `publish_destination_occupied`, and `library/` is not touched. Move the folder away, then retry. MusicLib never adopts or deletes it.
- **A program running as root** that leaves folders owned by root in `library/`, or changes owners there, stops MusicLib at that album's next update, or at its next start: the log shows a permission error (`publish_io` or `fs_permission`) and MusicLib restarts in a loop. Give the folder back with `sudo chown -R 1000:1000 /srv/musiclib/data` and run `docker compose restart app`: the update then completes. No album is lost in between.

`doctor` lists each foreign file and folder as `doctor_output_extra`: a warning, not damage, so it still ends with "no damage found". Delete them by hand, or regenerate the whole folder with the offline [rebuild](#rebuild-regenerate-the-library-folder), which deletes everything in `library/` and renders every album again. **Rebuild the library folder** in Activity regenerates the album folders only.

Give other programs read-only access: for a container, mount the library with `:ro`; for a network share, share it read-only. On many Linux desktops your own account is uid 1000 too (`id -u` prints `1000`), so the programs you run yourself can write there: set them not to change the files or folders of the library.

## The passwords

MusicLib has two passwords, both in `.env`: the **sign-in password**, which you type in the browser, and the **database password**, which only MusicLib and PostgreSQL use. The install block generates both.

### The sign-in password

One password, `MUSICLIB_PASSWORD`, protects the web interface and the API. There are no user accounts and no user name.

- It must have at least 12 characters, at most 1024 bytes, and no line break or other control character. MusicLib does not start without a valid one (`password_invalid`) and never writes it to its log.
- Read it with `grep MUSICLIB_PASSWORD .env`.
- **To change it**, edit the line in `.env` (for a new random one: `sed -i "s|^MUSICLIB_PASSWORD=.*|MUSICLIB_PASSWORD=$(openssl rand -base64 24)|" .env`), then run `docker compose up -d --wait`. Compose recreates the app, and every browser is signed out.
- **Forgotten it?** It is in `.env`: `grep MUSICLIB_PASSWORD .env`.
- A sign-in lasts 30 days from the moment you sign in, and ends earlier when MusicLib restarts (sessions live in memory only) or when you choose **Sign out** at the foot of the sidebar.
- A wrong password is answered after about one second, and attempts are checked one at a time, so a password cannot be guessed quickly. Sign-ins and refused attempts are logged with the client's address, never with the password.
- Only the server reads it: `doctor`, `rebuild`, `backup`, `restore`, `version` and the health check run without it.
- Two installations on the same machine (such as a [restore next to an existing one](#restoring-next-to-an-existing-installation)) share the browser's sign-in cookie, because browsers do not separate cookies by port: signing in to one signs you out of the other. Open the second one in a private window.

**Over plain HTTP the password travels in clear.** With the default setting it never leaves the machine. From other devices, see [Access from other devices](#access-from-other-devices).

### The database password

`compose.yaml` defines the database password once, for both containers (`x-db-password`): `POSTGRES_PASSWORD` from `.env`, or the default `musiclib` when it is unset or empty. The database publishes no port, so other machines cannot reach it; a password of your own is still better, and the install block writes a random one into `.env` without printing it.

- **Set it before the first start.** PostgreSQL reads it only when it creates its database. Changing it in `.env` later does not change the database's password, and MusicLib can no longer connect.
- Keep it in `.env`, not in `compose.yaml`: a new release's `compose.yaml` comes with the default again.
- **To change it later**, change it in PostgreSQL first, then in `.env`:

  ```sh
  docker compose exec postgres psql -U musiclib -d musiclib -c '\password musiclib'
  ```

  The command asks for the new password twice. Put the same value in `.env` as `POSTGRES_PASSWORD=...`, then run `docker compose up -d --wait`.

## Access from other devices

By default MusicLib listens only on `127.0.0.1`: only the machine it runs on can open it. `PUBLIC_ORIGIN` must match the address in the browser exactly: `http://localhost:8080` and `http://127.0.0.1:8080` are different addresses, and with the default setting only the second one works. Any other address answers `421 host_not_allowed`.

Choose one way:

| Way | Encryption | Setting |
|---|---|---|
| [SSH tunnel](#through-an-ssh-tunnel) | yes (SSH) | none |
| [Home network (LAN)](#on-your-home-network-lan) | **no**: the password crosses the network in clear | `MUSICLIB_BIND`, `PUBLIC_ORIGIN` |
| [HTTPS through Caddy](#https-and-a-domain-name) | yes (HTTPS) | `PUBLIC_ORIGIN` |

Never expose MusicLib's own port directly to the Internet.

### Through an SSH tunnel

From your computer to a server without a desktop, an SSH tunnel keeps the default settings:

```sh
ssh -L 8080:127.0.0.1:8080 you@server
```

It forwards port 8080 of your computer to port 8080 of the server, through SSH. While the connection stays open, open `http://127.0.0.1:8080` on your computer.

### On your home network (LAN)

Give the machine a fixed address in your router first. Then set that address in both settings: in `.env`, change these two lines as below (the `MUSICLIB_BIND` line starts with `#`: remove it), then run `docker compose up -d --wait`.

```text
MUSICLIB_BIND=192.168.1.20
PUBLIC_ORIGIN=http://192.168.1.20:8080
```

Open `http://192.168.1.20:8080` on any device of your network. MusicLib then no longer answers at `http://127.0.0.1:8080`, not even on the machine itself.

**Use this only on a network you trust.** Over plain HTTP the password and the sign-in cookie cross the network in clear: anyone who can watch the network can read them. From a shared network or from outside, use [HTTPS](#https-and-a-domain-name).

### HTTPS and a domain name

MusicLib serves plain HTTP. To open it from other devices at an address such as `https://music.example.com`, put a reverse proxy in front of it. The proxy adds the name and HTTPS, so the password and the sign-in cookie no longer cross the network in clear; the password (`MUSICLIB_PASSWORD`) still protects every page and the API. This guide uses [Caddy](https://caddyserver.com), which obtains and renews the certificate by itself, passes the original `Host` to MusicLib and sets no limit on upload sizes. The examples were checked with Caddy 2.11.4.

MusicLib needs one setting for it, `PUBLIC_ORIGIN`: the exact address of the proxy, `https://` and the name, all lowercase (an international name in its `xn--` form), with no trailing slash and no port unless the proxy listens on a port other than 443 (MusicLib refuses `:443`, which browsers leave out). With an `https://` address the sign-in cookie is marked `Secure`. Whichever way you choose:

- **MusicLib answers only at that address.** From then on `http://127.0.0.1:8080`, a LAN address and an SSH tunnel answer `421 host_not_allowed`; only the health checks (`/health/live`, `/health/ready`) answer at any address. Scripts that use the API use the new address.
- **MusicLib takes the whole name**: it cannot be served under a path, such as `https://example.com/music/`.
- **Keep MusicLib's own port private**: leave `MUSICLIB_BIND` at `127.0.0.1`, so the proxy is the only way in.
- MusicLib needs no `X-Forwarded-*` header and ignores them: its log of sign-ins shows the proxy's address, not the device's.
- Caddy keeps its certificates and keys in its own storage, which MusicLib's backups do not include.

#### Caddy on the same host

A public name needs a DNS record (`A`, and `AAAA` for IPv6) pointing `music.example.com` to your public address, and your router forwarding TCP ports 80 and 443 (and UDP 443, for HTTP/3) to the machine: Caddy uses them to obtain its certificate from Let's Encrypt. Devices at home use the same name; if they cannot reach it from inside your network, add the name with the machine's LAN address to your home network's DNS. The sign-in page is then on the Internet: MusicLib checks one attempt at a time and holds each refused one for a second, so a random password cannot be guessed, but a stranger's attempts can make your own sign-in wait. If you do not need MusicLib from outside your home, prefer [a name for your home network only](#a-name-for-your-home-network-only).

1. Install Caddy from its [official packages](https://caddyserver.com/docs/install); it runs as a service.
2. Replace `/etc/caddy/Caddyfile` with these two lines, using your name (and your `MUSICLIB_PORT`, if you changed it), then run `sudo systemctl reload caddy`:

   ```text
   music.example.com
   reverse_proxy 127.0.0.1:8080
   ```

3. In `~/musiclib`, point MusicLib at the new address (the `.env` of the install block has this line) and apply it:

   ```sh
   sed -i 's|^PUBLIC_ORIGIN=.*|PUBLIC_ORIGIN=https://music.example.com|' .env
   docker compose up -d --wait
   ```

4. Open `https://music.example.com` and sign in.

#### Caddy in Docker

Caddy can also run as a third container next to MusicLib, with nothing installed on the host. Next to `compose.yaml`, create `compose.override.yaml`, which Docker Compose reads together with `compose.yaml` by itself:

```yaml
# Caddy in front of MusicLib: docs/operations.md, "HTTPS and a domain name".
services:
  app:
    # No port on the host: only Caddy reaches MusicLib, over the Compose network.
    ports: !reset []

  caddy:
    image: caddy:2.11.4-alpine@sha256:6aeddd44c3078b0f9a35206472a11420648a79c184603ef95957d0a20044cb2b
    container_name: ${COMPOSE_PROJECT_NAME:-musiclib}-caddy
    restart: unless-stopped
    security_opt:
      - no-new-privileges:true
    cap_drop: [ALL]
    cap_add: [NET_BIND_SERVICE]
    ports:
      - "80:80"
      - "443:443"
      - "443:443/udp"
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy_data:/data
      - caddy_config:/config

volumes:
  caddy_data:
  caddy_config:
```

and a `Caddyfile` next to it:

```text
music.example.com {
	reverse_proxy app:8080
}
```

The DNS record and the router's ports are those of the previous section. Then point MusicLib at the new address and start everything:

```sh
sed -i 's|^PUBLIC_ORIGIN=.*|PUBLIC_ORIGIN=https://music.example.com|' .env
docker compose up -d --wait
```

- `!reset` removes MusicLib's port on the host; it needs Docker Compose 2.24 or later. Check with `docker compose config`: the `app` service must show no `ports`. If your Compose refuses `!reset`, delete the `app:` entry (its first three lines): MusicLib then stays reachable on `127.0.0.1:8080` from this machine only, as before.
- Without that port, the health check of the install block (`curl http://127.0.0.1:8080/health/ready`) no longer answers: use `docker compose ps` (the app shows `healthy`) or `curl -f https://music.example.com/health/ready`.
- The backup, check and restore commands work as before: they stop and start MusicLib only, and Caddy answers `502` in the meantime.
- Caddy's certificates, and for a home name its certificate authority (below), are in the volume `musiclib_caddy_data`: keep it.
- Caddy is yours to update: replace its image line with a newer release, by version and digest, then `docker compose up -d --wait`.
- A source build (`COMPOSE_FILE=compose.dev.yaml` in `.env`) names the override too: `COMPOSE_FILE=compose.dev.yaml:compose.override.yaml`.

#### A name for your home network only

Without a public name or an open port, MusicLib can still have a name and HTTPS on your home network, such as `music.home.arpa` (`home.arpa` is reserved for home networks). Caddy then signs the certificate with its own local certificate authority instead of Let's Encrypt.

1. Make the name point to the machine's LAN address on every device: add a local DNS record in your router or home DNS server (such as Pi-hole), or, on a computer, a line in its hosts file (`192.168.1.20 music.home.arpa`). Phones and tablets need the DNS record.
2. Use the `Caddyfile` below, in Docker as in the previous section; with Caddy on the host, write `reverse_proxy 127.0.0.1:8080` instead of `app:8080` and reload Caddy. Forward no port in the router: ports 80 and 443 need to be reachable only from your network.

   ```text
   music.home.arpa {
       tls internal
       reverse_proxy app:8080
   }
   ```

3. Set the address and apply it: `sed -i 's|^PUBLIC_ORIGIN=.*|PUBLIC_ORIGIN=https://music.home.arpa|' .env`, then `docker compose up -d --wait`.
4. Trust Caddy's certificate authority on each device, as below. Until then, browsers show a certificate warning.

**Trusting Caddy's local certificate authority.** Its root certificate is `root.crt`. Copy it with `docker compose cp caddy:/data/caddy/pki/authorities/local/root.crt .` (Caddy in Docker) or `sudo install -m 644 -o "$USER" /var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt .` (Caddy's package, so the copy is yours and readable), and install it on each device as a trusted root certificate authority:

- Windows: open the file, **Install Certificate**, *Local Machine*, and place it in *Trusted Root Certification Authorities*.
- macOS: open it in Keychain Access, add it to the *System* keychain, and set it to *Always Trust*.
- iPhone and iPad: open the file, install the profile (Settings, General, VPN & Device Management), then turn on full trust in Settings, General, About, Certificate Trust Settings.
- Android: Settings, Security, Encryption & credentials, Install a certificate, CA certificate. Chrome uses it; some apps do not.
- Firefox keeps its own list on every system: Settings, Privacy & Security, Certificates, View Certificates, Authorities, Import, and trust it to identify websites.

A device that trusts this root accepts **every** certificate Caddy's authority signs, for any name, not only `music.home.arpa`: whoever obtains its private key, stored next to `root.crt` in Caddy's data, could impersonate any website to that device. Keep Caddy's data private (do not copy it elsewhere or into a shared backup), install the root only on your own devices, and remove it from a device you no longer use. The root lasts about ten years and Caddy renews the site's certificate by itself; if Caddy's data is lost, Caddy makes a new root, which every device must trust again.

#### Other reverse proxies

Any reverse proxy works if it:

- passes the browser's `Host` header unchanged, port included. nginx does not by default: set `proxy_set_header Host $http_host;` (`$host` drops the port). Apache: `ProxyPreserveHost On`;
- leaves the other headers alone, in particular `Origin`, `Cookie`, `Set-Cookie`, `If-Match`, `ETag` and `X-Musiclib-Request`;
- serves MusicLib at the root of its own name;
- accepts large request bodies: an extra file can be 256 MiB and a cover 20 MiB. nginx refuses bodies over 1 MB by default, and the upload then fails with "The server doesn't answer": set `client_max_body_size 0;`, MusicLib applies its own limits;
- lets long transfers run. For nginx, `proxy_request_buffering off;` and `proxy_buffering off;` pass uploads and downloads through instead of storing them in temporary files, and a longer `proxy_read_timeout` and `proxy_send_timeout` (60 s by default) help large uploads over slow connections.

Response compression is not needed. If you turn it on (Caddy's `encode`, nginx's `gzip`), the `ETag` header changes: the web interface is not affected, and API scripts should take the `etag` field of the JSON body instead of the header.

## Importing and the Activity page

### How folders become albums

- A folder with audio files directly inside it is **one album**. Its subfolders without audio (scans, artwork) come along as extra files.
- A folder whose audio is only in subfolders named `CD1`, `CD2`, … or `Disc 1`, `Disc 2`, … is **one multi-disc album**.
- Other folders are searched for albums inside them. Files that belong to no album are listed as not imported.
- A folder with audio both directly inside it and in its subfolders is ambiguous: it is not imported, and the report says why. So is an album with audio in a format MusicLib does not support (OGG or WAV, for example), with the name of the file.
- Audio must be **FLAC**, **MP3** or **M4A** (AAC or ALAC).
- The cover is `cover.*`, `folder.*` or `front.*` at the album's root, or else a picture embedded in the tracks: JPEG or PNG, up to 20 MB and 40 megapixels. A cover file is also kept as an extra file.
- An `.lrc` file with the same name as a track, in the same folder, becomes that track's lyrics. Every other file is kept as an extra file of the album.
- Symbolic links and special files are never followed or opened. Inside an album they stop that album's import (remove them, then import the folder again); outside every album they are only listed as warnings.
- Importing the same files again is recognized and skipped. MusicLib never merges albums or artists on its own.

### Importing

1. Copy your albums into the import folder, `~/musiclib/import` (or the folder of `MUSICLIB_IMPORT`). For example:

   ```text
   ~/musiclib/import/
     Miles Davis - Kind of Blue/
       01 So What.flac
       02 Freddie Freeloader.flac
       cover.jpg
   ```

2. Open the **Import** page. It shows the contents of the import folder. Open a folder, or stay at the top to take everything, and choose **Import everything in …**.
3. MusicLib looks for albums in that folder, then imports each one completely or not at all: every track is read and checked before the album appears in the library.
4. The results appear in three tabs: **Needs attention** (albums that could not be imported, each with the reason and, where possible, a fix), **Imported**, and **Already there** (albums imported before, skipped).

**Leave the source files where they are, unchanged, until the import has finished.** A folder that changes during its import makes that import fail; retry it once nothing else is changing it. MusicLib never changes, moves or deletes anything in the import folder.

When an album needs attention, its row says why in a plain sentence, and **Details** shows the error code. Some problems are fixed right there: a missing or ambiguous album title (**Import with this title**), an ambiguous album artist (**Import with this artist**), or a temporary failure (**Retry**). Others need a change of the files, such as a damaged track or an unsupported format: fix the files in the import folder, then import the folder again. **Dismiss** hides a problem you do not want to fix.

The import reports are kept for 90 days, then deleted.

### The Activity page

**Activity** shows the work of MusicLib: scans, imports and library updates, in three groups:

- **In progress** and **Waiting**: work that is running or queued.
- **Needs attention**: work that failed, with the reason. Failed work stays there until you retry or dismiss it: **Retry all** retries every failed job, **Dismiss** hides a failed scan or import. A failed scan or import also stops needing attention once a later import of its folder succeeds.

**Advanced → Rebuild the library folder** writes every album's folder again. Use it if files in the library were changed or deleted outside MusicLib. It takes time and disk space; Activity shows the progress.

### Editing albums and the trash

- Open an album from the **Library** to change its title, artist, year, genre and compilation flag, the tracks (number, title, artist, genre, disc), the cover, lyrics and extra files. When you save, MusicLib writes that album's folder in the library again.
- If two browser tabs edit the same album, the second save is refused instead of overwriting the first: reload the page and redo the change.
- **Needs attention**, in the sidebar, lists the albums whose last library update failed. On the album's page, **Update in library** tries again.
- **Trash.** Moving an album to the trash removes it from the library folder; you can restore it from the **Trash** page at any time. Its originals stay.
- Uploads: a cover up to 20 MiB (JPEG or PNG, 40 megapixels), an extra file up to 256 MiB, lyrics (LRC) up to 2 MiB. At most two uploads copy at once; a third waits for its turn.

## Backups, restore and moving

A backup is a folder with the database dump (`catalog.dump`), a copy of every original (`originals/`) and a `manifest.json` with the hash of each file. The generated library is not saved (a restore regenerates it), and neither is your import folder, `work/` or Caddy's data.

### Making a backup

MusicLib must be stopped while a backup runs; PostgreSQL keeps running. From `~/musiclib`:

```sh
docker compose stop app
docker compose run --rm --no-deps app backup --to "/backup/$(date +%F-%H%M)"
docker compose start app
```

- The first line stops MusicLib (the app only).
- The second runs the offline `backup` command in a one-off container. `$(date +%F-%H%M)` names the backup after the date and time, such as `2026-09-29-2130`. It prints `Backup completed: /backup/2026-09-29-2130` and exits with 0.
- The third starts MusicLib again.

When MusicLib runs from a clone of the repository, `scripts/backup.sh 2026-09-29-2130` does the same three steps: see [The maintenance scripts](#the-maintenance-scripts).

What to know:

- The backup is a new folder under `/backup`, the backup folder (`MUSICLIB_BACKUP`). It is a full copy, and every original is verified while it is copied.
- It **never overwrites**: a name that already exists is refused (`backup_exists`).
- **The destination is always a new folder under `/backup/`**, the backup folder, written as a plain absolute path such as `/backup/2026-09-29-2130` (a folder inside an existing subfolder, such as `/backup/old/2026-09-29-2130`, works too). Any other path, such as `/tmp/…`, would be inside the one-off container and lost with it, so `backup` refuses it before doing anything (`backup_outside_backup`, exit 2). So does `/backup` itself, and a path with `.` or `..`, a trailing slash or a double slash.
- The backup is written under a temporary name, `.musiclib-backup-….tmp`, and renamed to its final name only when it is complete. A backup that fails after it started writing leaves that temporary folder: inspect it and delete it yourself; MusicLib never deletes it. Never use a temporary folder as a backup.
- A backup folder that MusicLib cannot write (for example a host folder owned by another uid) is refused (`fs_permission`, exit 2) before anything is written.
- `MUSICLIB_BACKUP` must not be inside `MUSICLIB_DATA` on the host either. Backup and restore compare the filesystem and folder behind both mounts and refuse a nested destination (`backup_destination`, exit 2). The check sees bind mounts of one host filesystem, but not through a network share or a second filesystem mounted over the data folder, so keep the two host paths plainly separate.
- **The default backup volume is on the same disk as your data**: it protects against mistakes, not against a disk failure. Set `MUSICLIB_BACKUP` to a folder on another disk, keep several backups, and copy them to an external device.
- To copy a backup elsewhere, use `sudo cp -a` (or `sudo rsync -a`): the dump is readable only by uid 1000, and the copy must keep its owner.
- A backup covers completed imports. Scans and imports still unfinished at backup time appear as failed after a restore, until you check the source and retry them.
- Try a restore now and then, [next to your installation](#restoring-next-to-an-existing-installation): a backup you have never restored is not a proven backup.

### Restore: use new empty destinations

A restore puts a backup into a **new, empty installation**: a new, empty database and a new, empty data folder. It never overwrites. Use it after a disk failure, on a new machine, or to try a backup.

"Empty" means: a database with no tables, sequences or views in its schema (not even MusicLib's migration table), and a data folder with no entries except `.lock` and an empty `lost+found` folder (at the root of an ext4 disk).

1. On the new machine, or in a new folder, paste the [install block](#first-start) **without its last line**.
2. In `.env`, set `MUSICLIB_BACKUP` to the host folder that contains your backup, such as `/mnt/backup/musiclib` (and `MUSICLIB_DATA` to a new empty folder, if you keep the data in a [host folder](#the-data-in-a-host-folder)).
3. Run:

   ```sh
   docker compose up -d --wait postgres
   docker compose run --rm --no-deps app restore --from /backup/2026-09-29-2130
   docker compose up -d --wait
   curl -f http://127.0.0.1:8080/health/ready
   ```

   - The first line starts **only PostgreSQL**, never the app: the first start of the app would initialize the empty data folder, and the restore would then refuse it.
   - The second restores the backup. It verifies the whole backup before writing anything, then prints `Restore completed. Start the app to render active albums.` and exits with 0.
   - The third starts MusicLib (it creates the app's container if needed), which regenerates the library folder by itself. Activity shows the progress.
4. When Activity is idle, run a deep check: `docker compose stop app && docker compose run --rm --no-deps app doctor --deep`, then `docker compose start app`.

When the new installation runs from a clone of the repository, `scripts/restore.sh 2026-09-29-2130` can run the second step ([The maintenance scripts](#the-maintenance-scripts)); it does not start the app.

The restored catalog and originals keep their identities, and all published output is regenerated. Keep the backup unchanged during the restore: a change between its verification and the database restore can leave the destinations unusable.

**If the restore fails**, its exit code and log line say which case you are in:

- Exit 2 (a refusal, such as a destination that is not empty or a backup that fails verification): nothing was written. Fix the cause and retry.
- Exit 1 with the advice "restore failed before its maintenance marker: nothing was restored": fix the cause and retry on the same destinations.
- Exit 1 with the advice "restore incomplete; .maintenance blocks server boot": the destinations are partly restored. Do not start the app, do not retry into them, and never remove `.maintenance`. Create **both** a new empty database and a new empty data folder, keep the old ones for investigation, and repeat the restore.

To get a new empty database with Compose, use a new project (below) or a new `db` volume: confirm with `docker compose ps` and `docker volume ls` what you are about to use, and never run `docker compose down -v` against a database or a backup you still need.

A backup made before schema 3 (by a development version older than 1.0.0) restores the same way: the start adds the track durations, and the renders that regenerate the output record the duration of every active album's tracks (trashed albums get theirs when restored). On an installation that stays up, **Rebuild the library folder** (Activity → Advanced) does the same. Such a backup may also hold artists without any album, created before MusicLib began deleting an artist together with its last album; they are kept as they are, and nothing removes them automatically.

### Restoring next to an existing installation

To restore on a machine that still runs an installation (to test a backup, for example), use a second folder with its own `compose.yaml` and `.env`, such as `~/musiclib-restore`: paste the install block without its last line, with `~/musiclib` replaced by `~/musiclib-restore`. `compose.yaml` fixes the Compose project name, `name: musiclib`: in a second folder the same name would select the **existing** installation's containers and volumes, so every command there would act on it. Three settings in the new folder's `.env` keep the two apart:

1. **Its own project name.** `COMPOSE_PROJECT_NAME` overrides `name:`; the new project gets its own containers (`musiclib-restore`, `musiclib-restore-db`) and volumes (`musiclib-restore_db`, `musiclib-restore_data`, `musiclib-restore_backup`). Check it before any other command:

   ```sh
   cd ~/musiclib-restore
   echo 'COMPOSE_PROJECT_NAME=musiclib-restore' >> .env
   docker compose config | head -1          # must print: name: musiclib-restore
   ```

2. **Another port**, with `MUSICLIB_PORT` and `PUBLIC_ORIGIN` changed together (the app refuses requests for any other origin):

   ```sh
   echo 'MUSICLIB_PORT=8081' >> .env
   sed -i 's|^PUBLIC_ORIGIN=.*|PUBLIC_ORIGIN=http://127.0.0.1:8081|' .env
   ```

3. **The backup in a host folder.** The new folder's `compose.yaml` cannot mount the old project's named volume, so copy the completed backup out of it (with `cp -a`, which keeps its owner, uid 1000, and modes), into a folder writable by the app's uid for the new installation's own later backups:

   ```sh
   sudo mkdir -p /srv/musiclib-restore/backup
   sudo cp -a /var/lib/docker/volumes/musiclib_backup/_data/2026-09-29-2130 /srv/musiclib-restore/backup/
   sudo chown 1000:1000 /srv/musiclib-restore/backup
   echo 'MUSICLIB_BACKUP=/srv/musiclib-restore/backup' >> .env
   ```

   If the existing installation keeps its backups in a host folder (`MUSICLIB_BACKUP`), copy from there.

Then run the restore commands of the previous section in `~/musiclib-restore`, and open `http://127.0.0.1:8081` (in a private window: the two installations share the browser's sign-in cookie). The two installations share nothing and can run side by side. When you are done with the test, `docker compose stop` in `~/musiclib-restore` stops it.

### Moving to another machine

1. On the old machine, [make a backup](#making-a-backup) and copy it to a disk the new machine can mount, keeping its owner: for example `sudo cp -a /var/lib/docker/volumes/musiclib_backup/_data/2026-09-29-2130 /media/usb/` (or copy from your `MUSICLIB_BACKUP` folder).
2. On the new machine, check [What you need](#what-you-need), and copy the backup into a folder such as `/mnt/backup/musiclib`. Then give it to uid 1000, because a copy made through another disk may lose its owner, and the dump is readable only by its owner:

   ```sh
   sudo mkdir -p /mnt/backup/musiclib
   sudo cp -a /media/usb/2026-09-29-2130 /mnt/backup/musiclib/
   sudo chown -R 1000:1000 /mnt/backup/musiclib
   ```

   Then [restore](#restore-use-new-empty-destinations) it with `MUSICLIB_BACKUP=/mnt/backup/musiclib` in `.env`.
3. Copy the settings you changed in the old `.env` (such as `PUBLIC_ORIGIN`, `MUSICLIB_IMPORT`, `MUSICLIB_PASSWORD`), not the database password: the new database has the one the install block wrote.
4. Keep the old installation until the new one works and has a backup of its own.

To move the data to another folder on the same machine, see [Moving an existing installation to a host folder](#moving-an-existing-installation-to-a-host-folder).

## Upgrading

**Back up before every upgrade.** A new version can upgrade the database at its first start, and there is no way back except restoring that backup with the older version.

### From 1.0.0 to 1.1.0

1.1.0 **does not start without a sign-in password** (`MUSICLIB_PASSWORD`), and the `.env` of a 1.0.0 installation has none. Add one first:

```sh
cd ~/musiclib
grep -q '^MUSICLIB_PASSWORD=.' .env || { sed -i '/^MUSICLIB_PASSWORD=/d' .env && printf '\nMUSICLIB_PASSWORD=%s\n' "$(openssl rand -base64 24)" >> .env; }
grep MUSICLIB_PASSWORD .env
```

- The second line adds a random password, unless `.env` already sets one. It removes an empty `MUSICLIB_PASSWORD=` line first.
- The third prints it: you sign in with it after the upgrade.

Then upgrade as below. You need the new `compose.yaml` too: it passes the password to the app, and the 1.0.0 one does not.

If you forget the password, `docker compose up -d --wait` only reports the app as unhealthy. `docker compose logs --tail=20 app` then shows the reason, a line with `"code":"password_invalid"`:

```text
musiclib  | {"time":"…","level":"ERROR","msg":"password_invalid: MUSICLIB_PASSWORD is missing or empty: set it in .env next to compose.yaml, for example to the output of `openssl rand -base64 24`, then run `docker compose up -d --wait`","code":"password_invalid"}
```

Add the password as above and run `docker compose up -d --wait` again. A `.env` with Windows (CRLF) line endings gives the same error: see [Configuration](#configuration).

### Upgrading the published image

```sh
cd ~/musiclib
docker compose stop app &&
docker compose run --rm --no-deps app backup --to "/backup/before-update-$(date +%F-%H%M)" &&
curl -fsSLO https://github.com/tommasonovelli/vibrance-musiclib/releases/latest/download/compose.yaml &&
docker compose pull && docker compose up -d --wait
docker compose run --rm --no-deps app version
```

- The commands are chained with `&&`: if the backup fails, nothing is updated, and MusicLib stays stopped until you fix the cause (`docker compose start app` restarts the old version).
- `curl` replaces `compose.yaml` with the latest release's. Your settings are in `.env`, which stays. If you changed the database password in `compose.yaml` instead of `.env`, move it to `.env` first: the new file comes with the default.
- `docker compose pull` downloads the new images; `docker compose up -d --wait` recreates the app on them and keeps the volumes.
- The last line prints the running version.

Instead of downloading a new `compose.yaml`, you can change the version on the app's `image:` line by hand: it is the only place that names it. Never use a floating tag such as `latest`.

At its first start the new version applies its database migrations, before any work starts. If its `render_version` changed, it queues a new render of every active album; failed renders are not retried silently. Check Activity, then run [doctor](#doctor-check-the-library).

**Downgrading is not possible**: an older version refuses a database that a newer one has upgraded (`store_schema_too_new`). To go back, restore the backup made before the upgrade, using the older version.

### Upgrading a source build

In the clone's folder, fetch the reviewed changes and rebuild:

```sh
git pull
docker compose up -d --build --wait
```

With `COMPOSE_FILE=compose.dev.yaml` in `.env`, this rebuilds the app from the new sources and recreates it. Back up first, as above.

## Maintenance

Four offline commands check, repair the output of, save and restore an installation: `doctor`, `rebuild`, `backup` and `restore`. They are subcommands of the app's image, run in a one-off container with `docker compose run --rm --no-deps app …`.

| Command | What it does | Changes |
|---|---|---|
| `doctor` | checks the originals, the library and the catalog | nothing (it only reads) |
| `doctor --deep` | the same, and also reads every original and every library file against its hash | nothing |
| `rebuild --store-id UUID` | deletes and regenerates the library folder and `work/` | `library/`, `work/` and the publication state |
| `backup --to /backup/NAME` | writes a new, verified backup | a new folder in `/backup` |
| `restore --from /backup/NAME` | restores a backup into a new, empty installation | the empty database and data folder |
| `version` | prints the version and `render_version` | nothing |

Rules for all of them except `version`:

- **The app must be stopped; PostgreSQL must be running.** Each command takes the lock on `/data/.lock` and refuses at once (`volume_locked`) if the app or another command holds it.
- `doctor`, `rebuild` and `backup` never migrate the database: after an upgrade, start the app once so that it applies the migrations, stop it, then run the command.
- They do not need the sign-in password.
- Exit codes: **0** success (for doctor: no damage), **1** damage found or an operation that failed (logged with its `code` and an `advice` field), **2** refused before doing anything, or invalid arguments (nothing written except `.lock`), or an incomplete or foreign `.maintenance` marker.
- Start the app again only after exit 0. After doctor's exit 1, read the findings first. After a failed `rebuild`, `backup` or `restore`, leave the app stopped and read [Errors of the maintenance commands](#errors-of-the-maintenance-commands).

The pattern, by hand:

```sh
docker compose stop app
docker compose run --rm --no-deps app doctor --deep
docker compose start app
```

### doctor: check the library

```sh
docker compose stop app
docker compose run --rm --no-deps app doctor              # quick: presence and sizes
docker compose run --rm --no-deps app doctor --deep       # full: reads and hashes every file
docker compose start app
```

- Without `--deep`, doctor checks that every original and every library file exists with its expected size, the path reservations, the receipts and the expected output. It is quick.
- With `--deep`, it also reads every original and every file listed in the receipts (tracks, covers, extra files) and compares its hash. A file changed without a change of size or date is found only this way. It takes time: it reads the whole data folder.
- Each finding is one line: severity (`error`, `warning` or `info`), code, the path or id it concerns, and advice, such as `warning doctor_output_extra Miles Davis/Kind of Blue/Thumbs.db: This output file is not in a DB-anchored receipt; inspect it before rebuilding.` Only `error` findings are damage. The last line is `Doctor complete: no damage found. Nothing was changed.` (exit 0) or `Doctor found damaged or inconsistent data. Nothing was changed.` (exit 1).
- **Doctor never repairs, never deletes and never resolves a pending publication.** An extra file is reported, not deleted. Unreferenced originals are information: do **not** delete them. A pending publication is reported: start the app to complete it, or use `rebuild`.
- Damaged originals need a good copy from a backup: a render or a rebuild only regenerates the library.

What each finding means: [Errors of the maintenance commands](#errors-of-the-maintenance-commands).

### rebuild: regenerate the library folder

`rebuild` is the universal repair of the library folder. It **deletes only `library/` and `work/`**, resets the publication state, the path reservations and the render jobs in one transaction, and queues a new render of every active album. It keeps the catalog, the originals and the import reports. Trashed albums stay trashed. It does not undo edits and never fixes a damaged original.

It asks for the store id, the UUID that pairs the data folder with its database, so that it never runs on the wrong installation by mistake:

```sh
docker compose stop app
docker compose run --rm --no-deps --entrypoint cat app /data/.musiclib-store
```

The second line prints the data folder's marker, such as `store_id=0b6f2c1e-8a4d-4f7e-9c3b-2d1e5a6f7b8c`. Copy the UUID after `store_id=`, without the prefix, and pass it to `rebuild`:

```sh
docker compose run --rm --no-deps app rebuild --store-id '0b6f2c1e-8a4d-4f7e-9c3b-2d1e5a6f7b8c'
docker compose start app
```

- Pass the UUID exactly as printed, in lowercase: anything else is refused as invalid usage (exit 2). It must match both the marker and the database; otherwise the rebuild refuses (`rebuild_store_id`, exit 2) and deletes nothing.
- On success it prints `Rebuild prepared. Start the app to render active albums; originals and catalog were kept.` Starting the app renders every album again; Activity shows the progress.
- **An interrupted or failed rebuild leaves `/data/.maintenance`**, which blocks the start of the app. Never delete it: run exactly the same rebuild command again, with the same store id, until it succeeds.
- Rebuild is not a backup: it regenerates output only.

**Rebuild the library folder** in Activity (Advanced) is the lighter, online variant: it writes every album's folder again while MusicLib runs, and leaves other files in `library/` alone.

### The maintenance scripts

The repository has four wrapper scripts in `scripts/`. Each one stops the app, runs one offline command, and starts the app again when that is safe. They are **not part of a release**: they exist only in a clone of the repository.

**Where they act.** A script runs `docker compose` in the clone's folder, on the Compose file and `.env` there: `compose.yaml` and its published image, or the file named by `COMPOSE_FILE` (from the environment or from `.env`), such as `compose.dev.yaml` for a source build, so that the offline command runs the same image as the server. **Use them only when your installation runs from that clone's folder.** For an installation in `~/musiclib`, run the commands by hand in `~/musiclib`, as shown above.

A clone next to an installation in `~/musiclib` names the same Compose project, `musiclib`, but not its `.env`: a script run there would stop your app and run the command with other settings (another data folder, the default database password). So, before stopping anything, every script checks the folder that Docker records in each container of the project's `app` and `postgres` (the label `com.docker.compose.project.working_dir`). If a container was created in another folder, or in a folder this shell cannot see, the script stops, exits 1, and prints the commands to run by hand instead, for example:

```text
doctor.sh: error: the Compose project of this clone (/home/you/src/vibrance-musiclib) already has containers created from /home/you/musiclib, which has its own compose.yaml and .env (a folder this shell cannot see counts as another one). Nothing was stopped. Run the commands by hand in the installation's folder (docs/operations.md, "Maintenance"):
  cd /home/you/musiclib
  docker compose stop app
  docker compose run --rm --no-deps app doctor --deep
  docker compose start app    # after exit 0 or 1
```

When the project has no `app` or `postgres` container yet, the check passes.

What every script does, in order:

1. Checks that `docker` and `docker compose` work and that the Docker daemon answers.
2. Checks its arguments; a wrong use prints `usage: …` (or, for `rebuild.sh`, `STORE_ID must be …`) and exits 1 without touching anything.
3. Checks that the project's containers were created in the clone's folder, as above; otherwise it exits 1 without touching anything.
4. Notes whether the app is running, then stops it (`stopping app (PostgreSQL remains up)`); PostgreSQL keeps running.
5. Runs the offline command (`running offline doctor`, for example) with `docker compose run --rm --no-deps app …`.
6. Starts the app again (`starting app again`) only if it was running before and the command exited 0, or, for doctor only, 1 (findings). Otherwise it prints `offline command exited N; app remains stopped; inspect the logs before starting it` and leaves the app stopped.

The script exits with the command's exit code (0, 1 or 2, as above).

| Script | Syntax | Runs | Example |
|---|---|---|---|
| `scripts/doctor.sh` | `scripts/doctor.sh [--deep]` | `doctor`, or `doctor --deep` | `scripts/doctor.sh --deep` |
| `scripts/rebuild.sh` | `scripts/rebuild.sh STORE_ID` | `rebuild --store-id STORE_ID` | `scripts/rebuild.sh 0b6f2c1e-8a4d-4f7e-9c3b-2d1e5a6f7b8c` |
| `scripts/backup.sh` | `scripts/backup.sh BACKUP_NAME` | `backup --to /backup/BACKUP_NAME` | `scripts/backup.sh 2026-09-29-2130` |
| `scripts/restore.sh` | `scripts/restore.sh BACKUP_NAME` | `restore --from /backup/BACKUP_NAME` | `scripts/restore.sh 2026-09-29-2130` |

- `doctor.sh` takes no argument or exactly `--deep`.
- `rebuild.sh` takes exactly one argument, the store id: read it first with `docker compose run --rm --no-deps --entrypoint cat app /data/.musiclib-store`, and pass the UUID after `store_id=` exactly as printed. Anything that is not a lowercase UUID prints `rebuild.sh: error: STORE_ID must be the UUID after store_id= in /data/.musiclib-store, exactly as printed, in lowercase: nothing was stopped` and exits 1 before the app is stopped.
- `backup.sh` and `restore.sh` take exactly one backup name: a single folder name under `/backup`, not `.` or `..`, with no `/`. Quote a name with spaces (`scripts/backup.sh '2026-09-29 full'`).
- `restore.sh` is for a new, empty installation where only PostgreSQL runs (`docker compose up -d --wait postgres` first). The app was not running, so the script does not start it: run `docker compose up -d --wait` after exit 0.

### Showing the version

```sh
docker compose run --rm --no-deps app version
```

It prints two lines, `version: 1.1.0` (the release; `devel` for a source build) and `render_version: …` (the identity of the renderer, the naming rules and the pinned tools), and exits 0. It needs neither the database nor the data folder, and can run while the app runs. The app also logs its version in its first log line, `starting`.

## Troubleshooting

### Reading the log

```sh
docker compose logs --tail=100 app
```

MusicLib logs one JSON object per line. A problem is a line with `"level":"ERROR"` and a stable `code`, such as `"code":"volume_permission"`, and a message that says what to do. Nothing is ever repaired or rewritten automatically.

When MusicLib refuses to start, the process exits and Docker starts it again (`restart: unless-stopped`), so the same line repeats until you fix the cause. `docker compose up -d --wait` then reports the app as unhealthy, and `docker compose ps` shows it `restarting` or `unhealthy`.

### Common problems

| Symptom | Cause | What to do |
|---|---|---|
| The browser shows `host_not_allowed` (421) | the address in the browser is not `PUBLIC_ORIGIN` (for example `localhost` instead of `127.0.0.1`, or a LAN address the origin does not name) | open exactly the address of `PUBLIC_ORIGIN`, or [change it](#access-from-other-devices) |
| The page does not open at all | MusicLib listens on another address or port, or is stopped | `docker compose ps`; check `MUSICLIB_BIND`, `MUSICLIB_PORT` and `PUBLIC_ORIGIN` in `.env` |
| The sign-in password is refused | a typo, or the password was changed | `grep MUSICLIB_PASSWORD .env`; after a change in `.env`, run `docker compose up -d --wait` |
| Signed out unexpectedly | MusicLib restarted, 30 days passed, or you signed in to a second installation on the same machine | sign in again |
| `password_invalid` after an upgrade from 1.0.0 | `.env` has no `MUSICLIB_PASSWORD` | see [From 1.0.0 to 1.1.0](#from-100-to-110) |
| `password_invalid` or `config_invalid` although `.env` looks right | `.env` has Windows (CRLF) line endings | `sed -i 's/\r$//' .env`, then `docker compose up -d --wait` |
| `docker compose up` fails with a mount error for `import` | the import folder does not exist; Compose does not create it | `mkdir -p ~/musiclib/import`, or fix `MUSICLIB_IMPORT` |
| `volume_permission` | the data folder is not owned by uid 1000 | `sudo chown -R 1000:1000 /srv/musiclib/data`, then `docker compose restart app` |
| An upload fails with 507 `insufficient_space` | not enough free space beyond the 1 GiB margin | free space on the data disk, then retry |
| An album stays in **Needs attention** with `publish_destination_occupied` | a folder that MusicLib did not create is where the album must go | move that folder out of `library/`, then retry |

### What MusicLib does when it starts

Each step logs a line, in this order; a refusal stops at its step:

1. `http listening`: `/health/live` answers 200 from here on; `/health/ready` answers 503 `not_ready` until the end of the start.
2. `volume lock acquired`: MusicLib holds `/data/.lock` until it exits. A second MusicLib, or a maintenance command, is refused with `volume_locked`.
3. If `/data/.maintenance` exists, the start stops with `volume_maintenance_pending` (or `_malformed`), before touching the database.
4. `database not reachable yet` (retried, from 250 ms to 5 s apart) until `database reachable`; then the database migrations.
5. `volume identified`: `/data/.musiclib-store` and the database must name the same store. Only a new, empty data folder with a new database is initialized: the first start creates both.
6. `originals/`, `library/` and `work/` are created if missing. Then the checks: one filesystem and one mount for all of `/data`, read and write permission, a real `RENAME_EXCHANGE` in `work/`; `media tools verified`: ffmpeg, ffprobe and the tag helper at their pinned versions (the line lists `ffmpeg`, `ffprobe`, `musiclib_tags` and `taglib`); `/import` readable and not part of `/data`.
7. `journal recovered` (or `no pending publication`): a publication that a crash, a lost database or an error left half done is completed forward before anything else. If the disk matches no legal step of it, publishing is **suspended** (`publish_illegal_state`, see below).
8. `work cleaned`: leftovers of an interrupted run are removed from `work/`; `running jobs recovered`: work of the previous run goes back to waiting.
9. `stale renders enqueued`: every active album rendered by another version of the renderer, and without a job, gets a new render. A failed render stays failed.
10. `workers started` (`WORKERS` of them), then `ready`.

While MusicLib runs, `/health/ready` also checks PostgreSQL (2 s timeout) and answers 503 `db_unavailable` while it is unreachable. If the database is lost, or a commit's outcome is unknown, MusicLib stops its work and exits (`fatal failure, stopping the workers` or `fatal failure in an API request, stopping`, with the `code`); Docker starts it again, and the new process waits for PostgreSQL and recovers.

`docker compose stop` stops the work in progress, gives a publication already under way up to 30 s to finish (otherwise the next start completes it), then releases the lock (`http server stopped`, `workers stopped`, `database pool closed`, `volume lock released`, `stopped`; exit 0). Compose waits up to 45 s.

### When MusicLib refuses to start

The process exits with 2 for the first three codes and 1 for the others, except `publish_illegal_state`, which keeps it running.

| `code` | What it means | What to do |
|---|---|---|
| `config_invalid` (exit 2) | a setting is missing or invalid (`PUBLIC_ORIGIN`, `WORKERS`, `DATABASE_URL`, `HTTP_ADDR`); the message lists all of them | fix `.env`, then `docker compose up -d --wait`. `PUBLIC_ORIGIN` must be exactly `scheme://host[:port]`, lowercase, no trailing slash, no default port |
| `password_invalid` (exit 2) | `MUSICLIB_PASSWORD` is missing, empty, shorter than 12 characters, longer than 1024 bytes, not UTF-8, or has a control character such as a line break; the value is never logged | set it in `.env` (for example `openssl rand -base64 24`), then `docker compose up -d --wait`. Check the [line endings](#configuration) |
| `run_as_root` (exit 2) | MusicLib would run as uid 0 | set `MUSICLIB_UID` and `MUSICLIB_GID` to a normal user, or remove them |
| `http_listen` | the app cannot listen on `HTTP_ADDR` inside the container | keep `compose.yaml` as released; `HTTP_ADDR` is `:8080` |
| `volume_unavailable` | `/data` cannot be opened for a reason other than permissions: it is missing or not a folder, `/data/.lock` is not a regular file, or an I/O error | check `MUSICLIB_DATA` and the disk; `.lock` must be a regular file and is never removed |
| `volume_locked` | another process holds `/data/.lock` | stop the other MusicLib or the maintenance command; never delete `.lock` |
| `volume_maintenance_pending`, `volume_maintenance_malformed` | a rebuild or restore did not finish | after a rebuild, run the same rebuild again with the same store id; after a restore, create **both** new empty destinations and restore again. Never delete `.maintenance` |
| `volume_store_mismatch` | the data folder belongs to another database | mount the right data folder, or use the right database |
| `volume_db_uninitialized` | the data folder is initialized, but the database is new or was reset | restore the database and the data folder from a backup into [new empty destinations](#restore-use-new-empty-destinations) |
| `volume_marker_missing`, `volume_not_empty` | `/data/.musiclib-store` is missing, and either the data folder is not empty or the database already has a catalog | check `MUSICLIB_DATA`: the marker is created only for an empty data folder with a new database |
| `volume_marker_malformed` | `/data/.musiclib-store` is not in the expected format | inspect it; MusicLib never rewrites it |
| `volume_cross_device`, `volume_nested_mount` | something is mounted inside `/data` | mount one ext4 filesystem on the data folder, nothing below it |
| `volume_permission` | `/data`, its `.lock` or a folder in it is not readable and writable by `MUSICLIB_UID`:`MUSICLIB_GID` (a host folder owned by another uid, or mode 555), or `/data` is mounted read-only | run the `sudo chown -R` command the message names on the data folder, then `docker compose restart app` |
| `volume_rename_exchange_unsupported` | the filesystem lacks `renameat2(RENAME_EXCHANGE)` | use ext4 |
| `import_unavailable` | `/import` is missing or not readable | check `MUSICLIB_IMPORT` and its permissions (readable by uid 1000) |
| `import_is_data` | `/import` is the data folder or one of its folders | point `MUSICLIB_IMPORT` at your music, never at the data folder |
| `media_tool_unavailable`, `media_tool_version` | ffmpeg, ffprobe or the tag helper is missing, broken or not the pinned version | pull the image again (`docker compose pull app`), or rebuild a source build (`docker compose -f compose.dev.yaml build app`); never replace the tools by hand |
| `store_migrate`, `store_schema_too_new` | the migrations failed, or the database is newer than this version | read the message; never downgrade: restore a backup made by this version instead |
| `publish_illegal_state` (keeps running, unhealthy, no work) | the pending publication does not match what is on disk (a folder moved or created by hand in `library/` or `work/`, a missing build); nothing was deleted; `/health/ready` answers 503 with the album and the build | put back what was moved and run `docker compose restart app` (the recovery runs again), or stop the app and run [rebuild](#rebuild-regenerate-the-library-folder) |
| `publish_io` | a filesystem error (I/O error, full disk, permission) while completing the pending publication | fix the disk, the space or the owner; the next start retries |
| `store_connection_lost`, `store_commit_uncertain` (while running) | the database was lost, or a commit's outcome is unknown | nothing: Docker restarts MusicLib, which recovers. If it repeats, check PostgreSQL: `docker compose logs --tail=50 postgres` |

### Errors of the maintenance commands

Each code appears in the command's log line, with an `advice` field for failures (exit 1).

| `code` | What it means | What to do |
|---|---|---|
| `volume_locked` (exit 2) | the app or another command holds `/data/.lock` | stop the app (`docker compose stop app`) or wait for the other command; never delete `.lock` |
| `maintenance_database` (exit 2) | PostgreSQL is not reachable, or the connection settings are invalid | start PostgreSQL (`docker compose up -d --wait postgres`) and check the database password in `.env` |
| `maintenance_schema` (exit 2) | the database is not at this version's schema; the offline commands never migrate | start the app once with this version so it applies the migrations, stop it, retry |
| `rebuild_store_id` (exit 2) | the store id does not match the marker and the database | pass the exact UUID of `/data/.musiclib-store`; nothing was deleted |
| `rebuild_delete`, `rebuild_unsafe_tree` (exit 1) | the rebuild could not delete `library/` or `work/` (permissions, a mount or a non-folder entry there) | fix the cause named in the log, then run the same rebuild again |
| `backup_exists` (exit 2) | a backup with that name exists | choose another name; backups are never overwritten |
| `backup_outside_backup` (exit 2) | the destination is not a new folder under `/backup`: another path such as `/tmp/…`, which would be lost with the one-off container, `/backup` itself, or a path with `.`, `..`, a trailing slash or a double slash; nothing was written | use `/backup/NAME`, such as `/backup/2026-09-29-2130` |
| `backup_destination` (exit 2) | the destination is invalid: not under an existing folder, through a symbolic link, or `MUSICLIB_BACKUP` is inside the `MUSICLIB_DATA` host folder | use `/backup/NAME`, and a `MUSICLIB_BACKUP` outside the data folder |
| `fs_permission`, `fs_read_only` (backup or restore, exit 2) | the backup folder cannot be written (owned by another uid, mode 555, or read-only) | `sudo chown 1000:1000` the `MUSICLIB_BACKUP` host folder, or mount a writable one |
| `backup_corrupt_blob`, `backup_blob_size`, `backup_blob_missing`, `backup_unsafe_original` (exit 1) | an original is damaged, missing or unexpected | run `doctor --deep`, recover the original from an older backup, and delete the leftover `.musiclib-backup-*.tmp` |
| `backup_dump_failed` (exit 1) | `pg_dump` failed; its redacted error is in the message | fix the cause, delete the leftover temporary folder, retry |
| `restore_volume_not_empty`, `restore_database_not_empty` (exit 2) | the destination is not empty; nothing was written | restore into a new empty database and a new empty data folder |
| `restore_manifest_invalid`, `restore_archive_invalid`, `restore_archive_extra`, `restore_dump_hash`, `restore_blob_hash`, `restore_blob_missing` (exit 2) | the backup failed verification before anything was written | use another complete backup |
| `restore_schema_too_new`, `restore_schema_unsupported` (exit 2) | the backup was made by a newer version, or by one this version cannot restore | use a version at least as new as the one that made the backup |
| `restore_dump_failed`, `restore_migrate`, `restore_store_id`, `restore_catalog_blob_missing`, `restore_catalog_blob_size`, `restore_blob_mismatch`, `restore_database_check`, `restore_schema` (exit 1) | the restore failed; read the advice | if it says `.maintenance` blocks the start: create **both** new empty destinations and restore again (from another backup if it fails again) |
| `doctor_receipt_hash`, `doctor_receipt_invalid`, `doctor_receipt_unreadable`, `doctor_output_hash`, `doctor_output_size`, `doctor_output_missing` | a library file or receipt is damaged or missing | render the album again (**Update in library**) or [rebuild](#rebuild-regenerate-the-library-folder) after reviewing the findings |
| `doctor_output_type`, `doctor_output_extra`, `doctor_originals_extra`, `doctor_unreadable` | an unexpected or unreadable entry | inspect the named path by hand; doctor never follows symbolic links and never deletes |
| `doctor_claim_invalid`, `doctor_claim_missing`, `doctor_claim_collision`, `doctor_invalid_path`, `doctor_published_state`, `doctor_album_no_tracks`, `doctor_missing_blob`, `doctor_missing_album` | the catalog is inconsistent | keep the app stopped, back up the evidence and investigate; rebuild resets reservations and publication state only |
| `doctor_blob_damaged`, `doctor_blob_hash` | an original is damaged | restore a good copy of it from a backup; rebuild cannot fix it |
| `doctor_pending_work`, `doctor_journal_pending`, `doctor_unreferenced_blob` | not damage | start the app to finish the work or the publication, or use rebuild for a stuck publication; keep unreferenced originals |
| `doctor_failed` (exit 1) | the check could not complete (I/O or database error); nothing was changed | fix the cause and run it again |

## The API

Everything the interface does is also available through a JSON HTTP API under `/api`. This section shows it with `curl`, from the folder that holds `.env`. `<id>`, `<revision>`, `<track>`, `<attachment>` and `<job>` stand for the values the API returns.

### Rules for every request

- **`Host`** must be the host and port of `PUBLIC_ORIGIN`, or the answer is `421 host_not_allowed`. With the default `PUBLIC_ORIGIN=http://127.0.0.1:8080`, `curl http://127.0.0.1:8080/...` sends the right `Host`; `http://localhost:8080` does not, and neither does a LAN address the origin does not name.
- **`Origin`**, if sent, must be exactly `PUBLIC_ORIGIN` (`403 origin_not_allowed`). `curl` sends none.
- **A session.** Without a live session every `/api` request answers `401 login_required`, and every page redirects (`303`) to `/login`. Only `/login`, `/logout`, `/static/*` and the health endpoints answer without one.
- **`X-Musiclib-Request: 1`** is required on every request other than GET and HEAD (`403 request_header_required`), except the two plain HTML forms `POST /login` and `POST /logout`, which keep the `Host` and `Origin` checks.
- **`If-Match`** with the `ETag` you just read is required on every change of an existing album or artist, covers, extra files, lyrics, tracks, trash and manual renders included: `428 precondition_required` without it, `412 precondition_failed` if someone changed it in between (read it again and redo the change). Creating an artist, starting an import, `POST /api/render-all` and the job actions take no `If-Match`.
- **JSON bodies**: `Content-Type: application/json`, 16 MiB at most, no unknown or duplicate keys, every field present (`null` where allowed).
- **Errors** are `{"code": …, "message": …, "details": …}` with a stable `code`.
- No CORS header is ever sent, and every response has `X-Content-Type-Options: nosniff`.
- `/health/live` and `/health/ready` are outside the `Host` check, so probes by IP work. During the start, while publishing is suspended and while stopping, `/api` answers `503` (`not_ready`, `publish_illegal_state`, `shutting_down`).

### Signing in

```sh
O=http://127.0.0.1:8080                  # your PUBLIC_ORIGIN
J="$(mktemp)"                            # the cookie file, readable only by you
sed -n 's/^MUSICLIB_PASSWORD=//p' .env | tr -d '\n' |
  curl -s -o /dev/null -w '%{http_code}\n' -c "$J" --data-urlencode password@- "$O/login"    # prints 303
```

- `O` is the address; `J` is a temporary file that holds the session cookie.
- `sed` reads the password from `.env` and `curl` sends it through its standard input (`password@-`), so it never appears on a command line.
- `POST /login` takes a form with one field, `password`. On success it answers `303` and sets the cookie `musiclib_session`, which `-c "$J"` saves. A wrong password answers `401` after about one second.
- Every later command passes the cookie with `-b "$J"`. The session lasts 30 days, until MusicLib restarts, or until you sign out:

```sh
curl -s -b "$J" -X POST -o /dev/null "$O/logout"; rm -f "$J"
```

### Artists and albums

```sh
curl -s -b "$J" "$O/api/artists"                                      # every artist, with its etag
curl -s -b "$J" "$O/api/albums?q=miles&limit=50"                      # search by title or artist; trash=true, artist=<id>, after=<next>
curl -si -b "$J" "$O/api/albums/<id>" | grep -i '^etag'               # ETag: "album:<id>:<revision>"
curl -s -b "$J" "$O/api/albums/<id>/status"                           # the album's library update
```

Every artist and album carries its revision and an `etag`, both in the JSON body and, for a single resource, in the `ETag` header: `"album:<id>:<revision>"` or `"artist:<id>:<revision>"`, quotes included. Send it back as `If-Match`:

```sh
curl -s -b "$J" -X PUT "$O/api/artists/<id>" -H 'X-Musiclib-Request: 1' -H 'Content-Type: application/json' \
  -H 'If-Match: "artist:<id>:<revision>"' -d '{"name":"Miles Dewey Davis"}'
curl -s -b "$J" -X POST "$O/api/albums/<id>/render" -H 'X-Musiclib-Request: 1' -H 'If-Match: "album:<id>:<revision>"'
curl -s -b "$J" -X DELETE "$O/api/albums/<id>" -H 'X-Musiclib-Request: 1' -H 'If-Match: "album:<id>:<revision>"'           # to the trash
curl -s -b "$J" -X POST "$O/api/albums/<id>/restore" -H 'X-Musiclib-Request: 1' -H 'If-Match: "album:<id>:<revision>"'    # back from the trash
```

- **Saving an album** (`PUT /api/albums/<id>`) takes every key of `{artist_id, new_artist, title, year, genre, compilation, tracks}`. The album's artist is exactly one of `artist_id` (an existing artist) and `new_artist` (the name of an artist to create with this save, in the same transaction); the other is `null`. A save that fails (428, 412, 422, 409) creates nothing; a `new_artist` that already exists is `409 artist_exists` (or `artist_folder_conflict`) with the existing artist's `artist_id` and both names in `details`.
- An artist left without any album, trashed ones included, by a save that moves its last album elsewhere is deleted by that save. `POST /api/artists` creates an artist without an album; it stays until an album arrives and leaves it.
- Each track of `GET /api/albums/<id>` carries `duration_ms`, its duration in milliseconds, or `null` while unknown: read-only, not a field of the PUT body.

### Covers, extra files and lyrics

An upload's body is the file itself, `Content-Type: application/octet-stream`; its format is read from the content. Limits: cover 20 MiB (JPEG or PNG, 40 megapixels, embeddable in every audio format of the album), extra file 256 MiB, lyrics 2 MiB of UTF-8 (`413` one byte over; `507 insufficient_space` when the data folder has no room for it beyond the 1 GiB margin). Every change needs the album's `If-Match`. Downloads go by id, never by a path.

```sh
H=(-H 'X-Musiclib-Request: 1' -H 'If-Match: "album:<id>:<revision>"')
curl -s -b "$J" -X PUT "${H[@]}" -H 'Content-Type: application/octet-stream' --data-binary @front.jpg "$O/api/albums/<id>/cover"
curl -s -b "$J" -X PUT "${H[@]}" -H 'Content-Type: application/json' -d '{"attachment_id":"<attachment>"}' "$O/api/albums/<id>/cover"
curl -s -b "$J" -X DELETE "${H[@]}" "$O/api/albums/<id>/cover"
curl -s -b "$J" -X POST "${H[@]}" -H 'Content-Type: application/octet-stream' --data-binary @booklet.pdf "$O/api/albums/<id>/attachments?path=Scans%2FBooklet.pdf"
curl -s -b "$J" -X DELETE "${H[@]}" "$O/api/albums/<id>/attachments/<attachment>"
curl -s -b "$J" -X PUT "${H[@]}" -H 'Content-Type: application/octet-stream' --data-binary @01.lrc "$O/api/albums/<id>/tracks/<track>/lyrics"
curl -s -b "$J" -X DELETE "${H[@]}" "$O/api/albums/<id>/tracks/<track>"
curl -sOJ -b "$J" "$O/api/albums/<id>/tracks/<track>/original"      # also .../lyrics, .../cover, .../attachments/<attachment>/content
```

- `H` is a bash array with the two headers every change needs; `"${H[@]}"` passes them to `curl`. Update the revision after every change: each one gives the album a new ETag.
- The second line chooses an extra file of the album as its cover; the third removes the cover.
- An extra file's `path` is a percent-encoded query parameter: a `+` in it is read as a space, as in any query string, so a literal plus is sent as `%2B` (`?path=Side%20A%2BB.pdf` is `Side A+B.pdf`).
- `-OJ` saves a download under the name the server gives it.

### Importing and the queue

```sh
M=(-H 'X-Musiclib-Request: 1' -H 'Content-Type: application/json')
curl -s -b "$J" "$O/api/import-source"                          # the import folder, sorted; symbolic links listed, never followed
curl -s -b "$J" "$O/api/import-source?path=Jazz"                # a folder under it (relative, no ..)
ID=$(cat /proc/sys/kernel/random/uuid)                          # the request id: keep it to repeat the request
curl -s -b "$J" -X POST "${M[@]}" -d "{\"id\":\"$ID\",\"path\":\"Jazz\"}" "$O/api/imports"   # 201; the same again: 200; another path: 409
curl -s -b "$J" "$O/api/imports/$ID"                            # the report: scanning, importing, completed; each candidate
curl -s -b "$J" "$O/api/jobs?state=failed"                      # pending, running, failed jobs (state=, kind=, limit=, after=)
curl -s -b "$J" -X POST "${M[@]}" -d '{"artist":null,"title":"Kind of Blue"}' "$O/api/jobs/<job>/retry"   # a failed import, with artist/title overrides
curl -s -b "$J" -X POST -H 'X-Musiclib-Request: 1' "$O/api/jobs/<job>/retry"          # any failed job, overrides kept
curl -s -b "$J" -X POST -H 'X-Musiclib-Request: 1' "$O/api/jobs/<job>/dismiss"        # a failed scan or import: no longer needs attention
curl -s -b "$J" -X POST -H 'X-Musiclib-Request: 1' "$O/api/jobs/retry-failed"         # every failed job that still needs attention
curl -s -b "$J" -X POST -H 'X-Musiclib-Request: 1' "$O/api/render-all"                # write every album's folder again
```

- `path` `""` imports the whole import folder. A batch with nothing to import completes with the scan failed as `no_valid_candidate`; files outside every album are the scan's `unassigned_file` warnings.
- A retry needs no `If-Match` (it changes no album); it is idempotent while the job is waiting or running; a done or skipped job is `409`.
- A failed scan or import stops needing attention once dismissed, or once a later import of its folder succeeds: the job shows `dismissed_at`, `superseded` and `needs_attention`, and `retry-failed` leaves it alone. A retry of it clears the dismissal.
- The import reports are kept 90 days, then deleted at the start or by the daily run of the server; batches with a job still to run are never deleted.

### When the connection to the database is lost

A database lost, or a commit left without an answer, during an API request stops MusicLib like one in its background work (it exits, and Docker starts it again). The request gets `503 store_connection_lost` or `503 store_commit_uncertain`: read the resource again before retrying, because the change may or may not have been saved.
