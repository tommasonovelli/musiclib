![Vibrance MusicLib](docs/assets/banner.jpg)

# Vibrance MusicLib

MusicLib keeps your music collection tidy without ever touching your original files.

1. **Import** your album folders. MusicLib copies each file into its own store and never changes, moves or deletes the source.
2. **Fix the metadata in your browser**: artists, album titles, years, genres, track titles and numbers, covers, lyrics and extra files such as booklets.
3. **MusicLib writes a clean library folder**: one folder per artist and per album, consistently named files, complete tags and the cover in every track. Any program that reads music folders can use it.

Each edit regenerates only the album it affects, and your original copies are kept forever. MusicLib is a self-hosted web app for one person. It runs in Docker on a Linux machine.

![The Library page of MusicLib: a grid of album covers with titles, artists and years, and a sidebar with Library, Import, Activity, Trash and Needs attention](docs/assets/screenshot-library.jpg)

![The album page of MusicLib: the cover, title, artist, year and genre of an album, its tracklist with durations and a lyrics mark, and its extra files](docs/assets/screenshot-album.jpg)

<sub>The artists, albums and covers in these screenshots are invented.</sub>

## Contents

- [Install](#install): seven steps, from checking your machine to your first album
- [Everyday use](#everyday-use): start, stop, logs, backups, updates
- [What it does and doesn't do](#what-it-does-and-doesnt-do)
- [The guide](#the-guide)
- [Building from source](#building-from-source)
- [License](#license)

## Install

Each step says what it does and what a good result looks like. The [operations guide](docs/operations.md) explains every step in more depth.

### 1. Check your machine

MusicLib needs **Ubuntu 24.04 or later** on an **amd64** (x86-64) machine, **Docker Engine** with the **Compose v2** plugin, usable without `sudo`, **local ext4 storage** for its data, and `curl` and `openssl` (`sudo apt install curl openssl`). Docker Desktop, NAS and network filesystems (NFS, SMB), filesystems other than ext4 and ARM machines are **not supported**: MusicLib checks its storage at every start and refuses what it can't use safely.

Run these three checks:

```sh
uname -m                                   # must print x86_64
docker compose version                     # prints "Docker Compose version v2...."
findmnt -no FSTYPE -T /var/lib/docker      # must print ext4
```

- If `docker` is missing, install it with `sudo apt install docker.io docker-compose-v2`. If it says "permission denied", run `sudo usermod -aG docker "$USER"`, then sign out and in again.
- If `findmnt` does not print `ext4`, Docker's storage can't hold MusicLib's data: choose an ext4 folder in step 2.

More: [What you need](docs/operations.md#what-you-need).

### 2. Optional: choose a folder for the data

By default MusicLib keeps its data (the originals and the generated library) in Docker's own storage, readable only with `sudo`. To keep it in a folder of your choice instead, such as `/srv/musiclib/data`, **decide now**: MusicLib pairs its database with its data folder at the first start.

To do it, follow [The data in a host folder](docs/operations.md#the-data-in-a-host-folder). It takes the place of step 3 (it runs the same block, creates the folder and starts MusicLib); then go on with step 4. Otherwise, skip this step.

### 3. Install and start

Paste this block into a terminal. It creates `~/musiclib`, downloads the two files of the latest release, writes a random database password and a random sign-in password into `.env`, and starts MusicLib:

```sh
mkdir -p ~/musiclib/import && cd ~/musiclib
[ -e compose.yaml ] || curl -fsSLO https://github.com/tommasonovelli/vibrance-musiclib/releases/latest/download/compose.yaml
[ -e .env ] || { curl -fsSL -o .env https://github.com/tommasonovelli/vibrance-musiclib/releases/latest/download/env.example && chmod 600 .env && sed -i "s/^POSTGRES_PASSWORD=.*/POSTGRES_PASSWORD=$(openssl rand -hex 32)/" .env && sed -i "s|^MUSICLIB_PASSWORD=.*|MUSICLIB_PASSWORD=$(openssl rand -base64 24)|" .env; }
docker compose up -d --wait
```

The first time, Docker downloads the images; the last command returns when MusicLib is ready. Check it:

```sh
docker compose ps                                    # both containers show "healthy"
curl -f http://127.0.0.1:8080/health/ready           # prints {"status":"ready"}
```

- **Run every command of this README from `~/musiclib`** (`cd ~/musiclib`).
- The block is safe to paste twice: it never replaces an existing `compose.yaml` or `.env`. What each line does: [First start](docs/operations.md#first-start).
- `.env` holds both passwords: keep it private (the block makes it readable by you only).
- If the app is reported unhealthy, `docker compose logs --tail=20 app` shows a line with a `code`: look it up in [Troubleshooting](docs/operations.md#troubleshooting).

### 4. Sign in

Open **<http://127.0.0.1:8080>** in a browser on the same machine (exactly this address: `localhost` is refused). Sign in with the password the block wrote into `.env`:

```sh
grep MUSICLIB_PASSWORD .env                          # prints MUSICLIB_PASSWORD=...
```

The password is everything after `=`. A good result is the Library page, empty for now.

On a machine without a desktop, open MusicLib from your computer through an SSH tunnel: run `ssh -L 8080:127.0.0.1:8080 you@server` on your computer, and while it stays connected open <http://127.0.0.1:8080> there.

### 5. Configure what you need (optional)

Your settings are in `.env`. The defaults suit most people; these are the ones most often changed. **After changing `.env`, apply it** with `docker compose up -d --wait`: Compose recreates the containers whose settings changed, keeps your data, and returns when MusicLib is ready. (`docker compose restart` does not apply a change of `.env`.)

- **Import from your music folder** (`MUSICLIB_IMPORT`, at any time). Instead of `~/musiclib/import`, import from the folder where your music already is. MusicLib mounts it **read-only** and never changes, moves or deletes anything in it. The folder must exist, and its files must be readable by uid 1000 (files readable by everyone are fine).

  ```sh
  sed -i '/^#\?MUSICLIB_IMPORT=/d' .env && echo 'MUSICLIB_IMPORT=/srv/music' >> .env
  docker compose up -d --wait
  ```

- **The data folder** (`MUSICLIB_DATA`, only before the first start): see step 2. To move an installation that already runs, follow [Moving an existing installation to a host folder](docs/operations.md#moving-an-existing-installation-to-a-host-folder).

- **The backup folder** (`MUSICLIB_BACKUP`, at any time, best before your first backup). By default backups go to a Docker volume on the same disk as your data, which protects against mistakes but not against a disk failure. Prefer a folder on another disk, owned by uid 1000 and never inside the data folder. Backups made before the change stay where they were.

  ```sh
  sudo mkdir -p /mnt/backup/musiclib && sudo chown 1000:1000 /mnt/backup/musiclib
  sed -i '/^#\?MUSICLIB_BACKUP=/d' .env && echo 'MUSICLIB_BACKUP=/mnt/backup/musiclib' >> .env
  docker compose up -d --wait
  ```

- **Other devices on your home network** (`MUSICLIB_BIND` and `PUBLIC_ORIGIN`, at any time). By default only this machine can open MusicLib. Give the machine a fixed address in your router, such as `192.168.1.20`, and set it in both settings:

  ```sh
  sed -i 's|^#\?MUSICLIB_BIND=.*|MUSICLIB_BIND=192.168.1.20|' .env
  sed -i 's|^PUBLIC_ORIGIN=.*|PUBLIC_ORIGIN=http://192.168.1.20:8080|' .env
  docker compose up -d --wait
  ```

  Then open <http://192.168.1.20:8080> on any device of your network; <http://127.0.0.1:8080> no longer answers, not even on the machine itself. For another port, such as 8081, also set `MUSICLIB_PORT=8081` and write that port in `PUBLIC_ORIGIN` (an example is in [Configuration](docs/operations.md#configuration)). **Over plain HTTP the password crosses the network in clear**: do this only on a network you trust. Otherwise use [HTTPS with Caddy](docs/operations.md#https-and-a-domain-name) or an SSH tunnel.

- **A new sign-in password** (`MUSICLIB_PASSWORD`, at any time). This writes a new random one; you can also type your own, at least 12 characters, after `MUSICLIB_PASSWORD=` in `.env`. Applying it signs every browser out.

  ```sh
  sed -i "s|^MUSICLIB_PASSWORD=.*|MUSICLIB_PASSWORD=$(openssl rand -base64 24)|" .env
  docker compose up -d --wait
  grep MUSICLIB_PASSWORD .env
  ```

- **Leave the database password (`POSTGRES_PASSWORD`) alone.** PostgreSQL reads it only when it creates its database: changing it in `.env` later locks MusicLib out of its database. To change it, follow [The database password](docs/operations.md#the-database-password).

Every setting, with its default: [Configuration](docs/operations.md#configuration).

### 6. Import your first album

1. Copy an album folder into the import folder, `~/musiclib/import` (or your `MUSICLIB_IMPORT` folder), for example:

   ```sh
   cp -r ~/Music/"Miles Davis - Kind of Blue" ~/musiclib/import/
   ```

2. In MusicLib, open the **Import** page and choose **Import everything in …**.
3. A good result: the album appears in the **Library** and under the **Imported** tab. An album that could not be imported is under **Needs attention**, with the reason.

Audio must be **FLAC**, **MP3** or **M4A** (AAC or ALAC). Leave the files unchanged until the import has finished. How folders become albums, and what to do when one needs attention: [Importing and the Activity page](docs/operations.md#importing-and-the-activity-page).

### 7. Find your library

MusicLib writes the library folder, `Artist/Album/NN - Title.ext` with the cover and complete tags:

- by default in Docker's storage, readable with `sudo`: `sudo ls /var/lib/docker/volumes/musiclib_data/_data/library`;
- with a data folder of your choice (step 2), in its `library` folder, such as `/srv/musiclib/data/library`, readable by every user of the machine.

Other programs may read it, but **never write in it**: change metadata in MusicLib, which replaces an album's folder at each update. See [The library folder](docs/operations.md#the-library-folder) and [Other programs and the library folder](docs/operations.md#other-programs-and-the-library-folder).

## Everyday use

Run these from `~/musiclib`. More in [Everyday commands](docs/operations.md#everyday-commands).

| Task | Command |
|---|---|
| Stop MusicLib | `docker compose stop` |
| Start it again | `docker compose up -d --wait` |
| Show its status | `docker compose ps` |
| Read its log | `docker compose logs --tail=100 app` |
| Show the sign-in password | `grep MUSICLIB_PASSWORD .env` |

MusicLib starts again by itself after a reboot, unless you stopped it.

> **Never run `docker compose down -v`: it deletes your library, its database and its backup volume.** To stop MusicLib, use `docker compose stop`.

### Back up

MusicLib must be stopped while a backup runs; PostgreSQL keeps running:

```sh
docker compose stop app
docker compose run --rm --no-deps app backup --to "/backup/$(date +%F-%H%M)"
docker compose start app
```

A good result is `Backup completed: /backup/2026-09-29-2130`: a new folder, named after the date and time, in the backup folder (step 5). A backup never overwrites another. Restoring, and moving to another machine: [Backups, restore and moving](docs/operations.md#backups-restore-and-moving).

### Update to a new version

```sh
cd ~/musiclib
docker compose stop app &&
docker compose run --rm --no-deps app backup --to "/backup/before-update-$(date +%F-%H%M)" &&
curl -fsSLO https://github.com/tommasonovelli/vibrance-musiclib/releases/latest/download/compose.yaml &&
docker compose pull && docker compose up -d --wait
docker compose run --rm --no-deps app version
```

The block backs up first, and updates nothing if the backup fails. `compose.yaml` names the version to run, so the update downloads the latest release's `compose.yaml`: `docker compose pull` alone keeps the version you have. Your settings stay in `.env`. The last line prints the new version. Going back to an older version is possible only by restoring the backup: see [Upgrading](docs/operations.md#upgrading).

**Upgrading from 1.0.0?** 1.1.0 does not start without a sign-in password, and the `.env` of 1.0.0 has none. Add one **before** the update block:

```sh
cd ~/musiclib
grep -q '^MUSICLIB_PASSWORD=.' .env || { sed -i '/^MUSICLIB_PASSWORD=/d' .env && printf '\nMUSICLIB_PASSWORD=%s\n' "$(openssl rand -base64 24)" >> .env; }
grep MUSICLIB_PASSWORD .env
```

The last line prints it: you sign in with it after the update. More: [From 1.0.0 to 1.1.0](docs/operations.md#from-100-to-110).

### Use it from other devices

By default only the machine MusicLib runs on can open it; from other devices use an SSH tunnel, your home network (step 5) or HTTPS with a name through Caddy, as [Access from other devices](docs/operations.md#access-from-other-devices) explains.

## What it does and doesn't do

**It does:**

- import album folders, including multi-disc albums, and report what it imported and what it skipped, and why; add tracks to an album from the browser, checked like an import;
- read and write tags of **FLAC**, **MP3** and **M4A** (AAC or ALAC) files;
- manage **JPEG and PNG covers**, **LRC lyrics** and **extra files** (booklets, scans, logs);
- let you search the library, move albums to the trash, restore them or empty the trash (the originals stay);
- verify its work: every copy is checked against the original, and the audio of each track is checked before and after its tags are written;
- check, back up and restore the whole collection.

**It doesn't:** play or stream music, convert audio between formats, look up metadata or recognize music online, watch folders for new files (you start each import), manage user accounts (there is one user, with one password), or edit arbitrary tags. The interface is in English.

## The guide

Everything else is in the [operations guide](docs/operations.md), in the order you need it:

- [Configuration](docs/operations.md#configuration): every setting of `.env`, with its default and an example.
- [Where your files are](docs/operations.md#where-your-files-are): the library folder, disk space, and [the data in a host folder](docs/operations.md#the-data-in-a-host-folder).
- [The passwords](docs/operations.md#the-passwords): reading and changing the sign-in and database passwords.
- [Access from other devices](docs/operations.md#access-from-other-devices): an SSH tunnel, your home network, or [HTTPS and a domain name](docs/operations.md#https-and-a-domain-name) with Caddy.
- [Importing and the Activity page](docs/operations.md#importing-and-the-activity-page): how folders become albums, and what to do when an import needs attention.
- [Backups, restore and moving](docs/operations.md#backups-restore-and-moving).
- [Upgrading](docs/operations.md#upgrading).
- [Maintenance](docs/operations.md#maintenance): checking the library with `doctor`, regenerating it with `rebuild`.
- [Troubleshooting](docs/operations.md#troubleshooting): what each error code means and what to do.
- [The API](docs/operations.md#the-api): everything the interface does, with `curl` examples.

## Building from source

The app image holds a Go server with its web interface built in, FFmpeg and a small TagLib-based tag writer, all pinned to exact versions; PostgreSQL 17 runs in its own container, from its own pinned image. To build the app from a clone of the repository instead of using the published image, see [Running from source](docs/operations.md#running-from-source).

To contribute, read [CONTRIBUTING.md](CONTRIBUTING.md). Development, tests, pinned dependencies and releases are in the [developer guide](docs/docker.md).

## License

MusicLib's code and documentation are released under the [MIT License](LICENSE), copyright 2026 tommasonovelli.

The sun logo is the author's artwork and is **not** covered by the MIT License: you may keep it in unmodified copies, but a modified version you distribute must use its own symbol. See [LOGO.md](LOGO.md).

Third-party software and assets keep their own licenses, among them the Hanken Grotesk font, under the [SIL Open Font License](web/OFL.txt). [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) lists everything the Docker image contains, with versions, licenses and where to get the sources; it is also in the image, under `/usr/share/doc/musiclib/`. Each GitHub release attaches it together with the source tarballs of FFmpeg and TagLib.
