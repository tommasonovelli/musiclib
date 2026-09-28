# Browser interface

Open `http://127.0.0.1:8080/` after starting the app (see [Docker](../docker.md)). Use the exact host and port in `PUBLIC_ORIGIN`; a different Host is rejected. The interface is in English (NOTES.md N-256).

## Layout

A sidebar on the left opens with the MusicLib symbol and name, a link to the Library, and leads to **Library**, **Import**, **Activity** and **Trash** (the Library filtered to trashed albums), each with its icon. Below them, **Needs attention** shows the number of albums whose last update failed and opens the Library filtered to them, trashed ones included; the number is computed when a page loads and is not refreshed in the background, and its icon turns red when it is not zero. The dot next to Activity appears on the Import and Activity pages while work they show is waiting or in progress.

The button at the foot of the sidebar collapses it to its icons, and expands it again; it stays where it was, under the pointer (NOTES.md N-311). Collapsed, the symbol stays at the top without the name, pointing at an icon or reaching it with Tab shows its name beside it, the Needs-attention number becomes a small badge on its icon, and the activity dot sits on the Activity icon. The browser remembers the choice (per browser, in its local storage) and applies it before the page appears, so nothing moves when a page opens; if the browser keeps no storage, the sidebar starts expanded. Without JavaScript the sidebar is always expanded and the button is not shown.

On a screen narrower than 52rem the sidebar becomes a row at the top: each view as its icon over its name, then Needs attention as its icon and number, only when it is not zero. The symbol, the name and the collapse button are not shown there.

## Library

Albums appear as a grid of their covers, with title, artist and year underneath. An album without a cover shows a grey square with the initials of its title. A red dot on a cover means the album needs attention (see Activity); a grey dot means it is waiting to be updated; an orange pulsing dot means it is being updated; an up-to-date album shows nothing. The word under the artist says the same thing in text.

The search at the top right stays in view while you scroll: once the covers pass beneath, the head becomes smaller (the title shrinks to the size of a heading) and a thin line appears under it; back at the top it is full size again. The page never jumps while it does, and nothing animates with reduced motion. The search filters as you type, by title or artist; matching artists are offered above the results, and choosing one shows only that artist's albums. More albums load as you scroll. Without JavaScript the search is an ordinary form and a **Load more** link loads the next page.

Clicking a cover (or pressing Enter or Space on it) opens the album under its row: large cover, title, artist, genre and year, tracks (a track's own artist on a second line, its duration at the right) and status, and **Edit album**, which opens the editor. The panel takes its colours from the cover, with text contrast of at least 4.5:1. Esc, a second click, or another cover closes it. Arrow keys move between covers; Home and End go to the first and last. Without JavaScript a cover is a link to the album page.

## Album

The album page shows the cover (240 px), the title, the artist, year, genre and whether the album is a compilation, then the tracks grouped by disc, then the extra files. Without JavaScript it is read-only, with every download.

- **Editing.** The fields look like text until you click them. Each change counts in the **Save** bar, which rises from the bottom («2 changes»); **Save** saves them all at once, without reloading the page, and says **Saved**. Enter in a field (the title too) saves. Leaving the page with unsaved changes asks first. Choosing files, lyrics or menus is never a change.
- **Inherited values.** An empty track artist or genre shows the album's value in grey: the track uses it. Clearing a field goes back to it. **No genre**, in the track's ⋯ menu, gives the track no genre at all. The ⋯ menu also has the track's disc number.
- **Artist.** Type to choose among the existing artists; **Create artist** appears only for a name that does not exist yet. It creates nothing by itself: the field says «New artist» and the artist is created when **Save** succeeds, together with the album's change. Choosing an existing artist again, or typing another name, drops it. An artist left without any album (in the trash included) when its last album moves to another artist is removed at once, so the list never offers one. **Rename artist** renames the album's artist on all of its albums, trashed ones included, after saying how many.
- **Duration.** Each track shows how long it plays (m:ss, or h:mm:ss from an hour), read-only; a dash while it is not known yet, which is the case for albums imported before this column existed until they are written to the library folder again (**Rebuild the library folder**, in Activity → Advanced, fills every album at once). Under the tracks, their number and, when every duration is known, the album's length. The Library's open album shows the same times.
- **Cover.** Drop an image on the cover, or use **Change cover**: upload an image, choose one of the album's images (only JPEG and PNG are offered, as thumbnails), or remove the cover. The limit is shown in words: JPEG or PNG up to 16 MB when the album has a FLAC track, 20 MB otherwise, and 40 megapixels.
- **Lyrics.** **Add lyrics** takes several `.lrc` files at once and proposes a track for each, by the number and title in the file name; files it cannot place are listed. Confirm to use them. Each track's ⋯ menu can upload lyrics, use one of the album's `.lrc` files, download or remove them. Lyrics files are UTF-8, up to 2 MB.
- **Extra files** (booklets, scans, logs): download by name, delete, or add one with **Save as** (for example `Scans/front.jpg`), up to 256 MB.
- **Menus.** A track's ⋯ menu: disc, No genre, lyrics, **Download original**, **Delete track** (asks first; it cannot be undone). The album's ⋯ menu: **Update in library** (writes the album again into the library folder) and **Move to trash** (asks first; a trashed album shows «This album is in the trash.» with **Restore**, and its back link leads to the **Trash**). Menus open with Enter and close with Esc. Downloads never ask about unsaved changes: the page stays as it is.
- **Errors** are one sentence where they happened, with the technical answer in a closed **Details**. Two tracks with the same number are named and highlighted. If another window changed the album first, Save says «This album was changed in another window.»: **Reload and reapply my changes** loads the new version, puts your changes back on top, unsaved, for you to check and save.
- **Status.** A dot and a word under the fields while the album is waiting for, or going through, an update (checked every two seconds only then), or needs attention: the same words as the Library.

## Import

Import shows the music folder mounted at `/import`, like a file browser: its folders as rows, each with what it holds («2 folders», «12 tracks, 2 images»), and a path at the top to go back up. Audio files are not listed one by one: the open folder's own files are summed up on one line. Links, special files and names that cannot be read are listed but never followed. Arrow keys move between folders; Enter opens one. It all works without JavaScript.

**Import everything in …** (named after the open folder) imports it and everything under it. The originals are never changed; don't move them until the import has finished. If the answer to the request is lost, pressing the button again sends the same request and makes a single import. Without JavaScript the button is not shown.

While the import runs, one row shows its progress (looking for albums, then album 4 of 12). Its results are in three tabs with their counts, **Needs attention**, **Imported** and **Already there**, and the page opens on the first that is not empty (arrow keys move between tabs; without JavaScript they are links). Imported albums link to their page; an album already there that is in the trash says so. Each row that needs attention says the problem in one sentence and offers the one thing that fixes it: a title or an artist to import it with (the other value given before is kept), or **Retry** when trying again can work as the files are (the folder was unplugged, had changed, or the disk was full). When only a change to the files can help (a damaged track, a folder with tracks and more tracks in its subfolders), the sentence says what to change, and the folder is then imported again. The technical details are in a closed **Details**. **Dismiss** hides a problem you have dealt with elsewhere; a problem also disappears by itself once a later import of the same folder (or, for a folder that was not an album, of a folder inside it) succeeds or finds the album already there. Files that belong to no album are listed under the tabs. The page updates every two seconds while the import runs, in place: what you are typing, an open **Details**, the focus and the scroll stay as they are.

**Recent imports**, at the bottom of the music folder, lists the imports of the last 90 days (the results are kept that long) with what came of each. Import results can be bookmarked as `/import?batch=<id>`.

## Activity

Activity lists what is **In progress**, **Waiting** and **Needs attention**, each with its count: albums with their cover and name (linking to the album), and imports by their folder (linking to their results). Times are relative («2 minutes ago»), with the full date on hover and next to the time on keyboard focus. Rows that need attention say the problem in one sentence; imports can be dismissed there too. **Retry all** tries every one of them again, leaving work in progress alone, and says how many were queued («3 albums queued again»). Up to 100 rows are shown per group. With nothing to do, the page says «Nothing in progress. Your library is up to date.».

**Advanced**, closed by default, holds **Rebuild the library folder**: use it when files in the library folder were changed or deleted outside MusicLib. It says how many albums it writes again before it starts (this takes time and disk space), then how many it queued. This is not the offline `rebuild` command of [Operations](../operations.md): the app stays running and nothing is deleted first.

Import and Activity check for changes every two seconds only while something is waiting or in progress; an idle page makes no requests. Without JavaScript both pages can be read, and have no actions.

Every album change is a conditional request (`If-Match`, NOTES.md N-260): the page never overwrites a newer version from another window. Relative paths and image formats are validated by the server. All downloads use entity IDs, never client-supplied filesystem paths.

## Appearance and assets

The interface follows the operating system's light or dark preference; there is no theme switcher. Its accent is MusicLib's Violet (`#643FD1`, `#A79BFE` in the dark theme, NOTES.md N-310), used only for links, the primary button, keyboard focus, checkboxes, folder icons and the symbol; apart from the status colours the rest is neutral and leaves colour to the covers. Reduced motion is honoured: the panel opens without sliding, dots stop pulsing and page changes do not animate. In browsers that support cross-document view transitions (Chromium), the cover of the open album moves into place when the editor opens; others simply navigate.

Assets are embedded in the server (`web/`) and served from `/static/`: one stylesheet, three small JavaScript modules (`app.js` for the album page, `queue.js` for Import and Activity, `library.js` for the Library and its head), one small classic script loaded in the page head of every page (`sidebar.js`, the sidebar state, NOTES.md N-272), the Hanken Grotesk font (two WOFF2 files, SIL Open Font License, `/static/OFL.txt`) and the favicon (`favicon.svg`: the symbol in Violet, with its dark-theme colour inside the file). There is no frontend build, Node dependency, CDN or raster image: the sidebar icons and the MusicLib symbol and name are inline SVG in the page, and the name is drawn as outlines, so no second font is shipped (NOTES.md N-309); the page CSP (`default-src 'self'`) blocks everything else, including `data:` images. The fonts are cached for a year (their names carry their version), the favicon for a day; the stylesheet and scripts are not cached. `/favicon.ico` is not served (404): every page names the SVG icon (NOTES.md N-312). CSS plus JavaScript stay under 90 KB, uncompressed and without the fonts (owner, NOTES.md N-267): 79,209 bytes after round 23 (NOTES.md N-315).

## Tests and review screenshots

The dev/test Docker image includes Chromium solely for real-browser tests; the runtime image does not. Tests use chromedp with real PostgreSQL and ext4 on an ephemeral localhost port (NOTES.md N-203 to N-213, N-243 to N-252, N-256 to N-278, N-284 to N-294). To look at the pages in both themes at laptop and phone widths, the album editor in its states, Import and Activity in theirs, and the sidebar expanded, collapsed and with a tooltip at laptop width, write review screenshots with:

```sh
scripts/dev.sh env MUSICLIB_UI_SHOTS=/src/tmp/ui-shots go test -count=1 -run 'TestBrowser(Library|Album|Queue)Screenshots' ./internal/http/
```

The album shots show the page as loaded, with unsaved changes (also with the sidebar collapsed, at laptop width), after a conflict, with a track menu open, and a trashed album. The Library's `scrolled` shots show the compact head. The Import shots show the music folder, a folder of folders, an album folder, an import in progress, the results on each tab, a problem with its Details open and an empty music folder; the Activity shots all three groups, Advanced open, and the empty page; both also with the sidebar collapsed at laptop width.

They land in `tmp/ui-shots/` (gitignored). Without the variable the test is skipped. The test browser has no CJK fonts, so Japanese titles render as boxes in these screenshots only.
