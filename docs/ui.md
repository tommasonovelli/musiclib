# Browser interface

Open `http://127.0.0.1:8080/` after starting the app (see [Docker](docker.md)). Use the exact host and port in `PUBLIC_ORIGIN`; a different Host is rejected. The interface is in English (NOTES.md N-256). Import and Activity keep their current layout until the redesign round 21.

## Layout

A sidebar on the left leads to **Library**, **Import**, **Activity** and **Trash** (the Library filtered to trashed albums), each with its icon. Below them, **Needs attention** shows the number of albums whose last update failed and opens the Library filtered to them, trashed ones included; the number is computed when a page loads and is not refreshed in the background, and its icon turns red when it is not zero. The dot next to Activity appears while work is running on the Import and Activity pages.

The button at the top of the sidebar collapses it to its icons, and expands it again. Collapsed, pointing at an icon or reaching it with Tab shows its name beside it, the Needs-attention number becomes a small badge on its icon, and the activity dot sits on the Activity icon. The browser remembers the choice (per browser, in its local storage) and applies it before the page appears, so nothing moves when a page opens; if the browser keeps no storage, the sidebar starts expanded. Without JavaScript the sidebar is always expanded and the button is not shown.

On a screen narrower than 52rem the sidebar becomes a row at the top: each view as its icon over its name, then Needs attention as its icon and number, only when it is not zero. The collapse button is not shown there.

## Library

Albums appear as a grid of their covers, with title, artist and year underneath. An album without a cover shows a grey square with the initials of its title. A red dot on a cover means the album needs attention (see Activity); a grey dot means it is waiting to be updated; an orange pulsing dot means it is being updated; an up-to-date album shows nothing. The word under the artist says the same thing in text.

The search at the top right stays in view while you scroll, with a thin line under it once the covers pass beneath; it filters as you type, by title or artist; matching artists are offered above the results, and choosing one shows only that artist's albums. More albums load as you scroll. Without JavaScript the search is an ordinary form and a **Load more** link loads the next page.

Clicking a cover (or pressing Enter or Space on it) opens the album under its row: large cover, title, artist, genre and year, tracks (a track's own artist on a second line) and status, and **Edit album**, which opens the editor. The panel takes its colours from the cover, with text contrast of at least 4.5:1. Esc, a second click, or another cover closes it. Arrow keys move between covers; Home and End go to the first and last. Without JavaScript a cover is a link to the album page.

## Album

The album page shows the cover (240 px), the title, the artist, year, genre and whether the album is a compilation, then the tracks grouped by disc, then the extra files. Without JavaScript it is read-only, with every download.

- **Editing.** The fields look like text until you click them. Each change counts in the **Save** bar, which rises from the bottom («2 changes»); **Save** saves them all at once, without reloading the page, and says **Saved**. Enter in a field (the title too) saves. Leaving the page with unsaved changes asks first. Choosing files, lyrics or menus is never a change.
- **Inherited values.** An empty track artist or genre shows the album's value in grey: the track uses it. Clearing a field goes back to it. **No genre**, in the track's ⋯ menu, gives the track no genre at all. The ⋯ menu also has the track's disc number.
- **Artist.** Type to choose among the existing artists; **Create artist** appears only for a name that does not exist yet. **Rename artist** renames the album's artist on all of its albums, trashed ones included, after saying how many.
- **Cover.** Drop an image on the cover, or use **Change cover**: upload an image, choose one of the album's images (only JPEG and PNG are offered, as thumbnails), or remove the cover. The limit is shown in words: JPEG or PNG up to 16 MB when the album has a FLAC track, 20 MB otherwise, and 40 megapixels.
- **Lyrics.** **Add lyrics** takes several `.lrc` files at once and proposes a track for each, by the number and title in the file name; files it cannot place are listed. Confirm to use them. Each track's ⋯ menu can upload lyrics, use one of the album's `.lrc` files, download or remove them. Lyrics files are UTF-8, up to 2 MB.
- **Extra files** (booklets, scans, logs): download by name, delete, or add one with **Save as** (for example `Scans/front.jpg`), up to 256 MB.
- **Menus.** A track's ⋯ menu: disc, No genre, lyrics, **Download original**, **Delete track** (asks first; it cannot be undone). The album's ⋯ menu: **Update in library** (writes the album again into the library folder) and **Move to trash** (asks first; a trashed album shows «This album is in the trash.» with **Restore**, and its back link leads to the **Trash**). Menus open with Enter and close with Esc. Downloads never ask about unsaved changes: the page stays as it is.
- **Errors** are one sentence where they happened, with the technical answer in a closed **Details**. Two tracks with the same number are named and highlighted. If another window changed the album first, Save says «This album was changed in another window.»: **Reload and reapply my changes** loads the new version, puts your changes back on top, unsaved, for you to check and save.
- **Status.** A dot and a word under the fields while the album is waiting for, or going through, an update (checked every two seconds only then), or needs attention: the same words as the Library.

## Import, Activity

Import browses only the music folder mounted at `/import` (the page calls it the music folder); only folders are links, and links, special files and unreadable names are listed but never followed. Choose a folder, then **Import folder**. The import results can be bookmarked as `/import?batch=<id>` and show the album search, files without an album and other warnings, and the albums found. An import that finds no album says so and the album search above explains why. Import limits: **1,000 tracks and 10,000 files per candidate**. The browser retains the request UUID if the POST answer is lost: retry the same submission; using that UUID for a different path returns a conflict. Imports that need attention can be retried from the results with **Use this artist** / **Use this title**; an empty field goes back to the tags and clears a value stored before. The server validates all inputs and rechecks the source on retry. Keep the source unchanged and mounted until the batch completes.

Activity shows what is waiting, in progress or needs attention (imports, album searches and library updates). An entry that needs attention can be retried individually; **Retry all** retries every one of them while leaving running work alone. **Rebuild library folder** confirms before writing every active album again and finishing unfinished trash removals; allow time and disk space. Error codes and exact timestamps are still shown as the API gives them until round 21. Work that is waiting or in progress is never offered an individual retry. Import and Activity poll every two seconds only while work is pending/running; an idle page makes no polling requests. Import results can also be reached from Activity after navigating away (**Import results**).

Every album change is a conditional request (`If-Match`, NOTES.md N-260): the page never overwrites a newer version from another window. Relative paths and image formats are validated by the server. All downloads use entity IDs, never client-supplied filesystem paths.

## Appearance and assets

The interface follows the operating system's light or dark preference; there is no theme switcher. Reduced motion is honoured: the panel opens without sliding, dots stop pulsing and page changes do not animate. In browsers that support cross-document view transitions (Chromium), the cover of the open album moves into place when the editor opens; others simply navigate.

Assets are embedded in the server (`web/`) and served from `/static/`: one stylesheet, three small JavaScript modules (`app.js` for the album page, `queue.js` for Import and Activity, `library.js` for the Library), one small classic script loaded in the page head of every page (`sidebar.js`, the sidebar state, NOTES.md N-272) and the Hanken Grotesk font (two WOFF2 files, SIL Open Font License, `/static/OFL.txt`). There is no frontend build, Node dependency, CDN or image file (the sidebar icons are inline SVG in the page); the page CSP (`default-src 'self'`) blocks everything else, including `data:` images. The fonts are cached for a year (their names carry their version); the stylesheet and scripts are not cached. CSS plus JavaScript stay under 90 KB, uncompressed and without the fonts (owner, NOTES.md N-267): 70,973 bytes after rounds 20 and 20b and their review.

## Tests and review screenshots

The dev/test Docker image includes Chromium solely for real-browser tests; the runtime image does not. Tests use chromedp with real PostgreSQL and ext4 on an ephemeral localhost port (NOTES.md N-203 to N-213, N-243 to N-252, N-256 to N-278). To look at the pages in both themes at laptop and phone widths, the album editor in its states, and the sidebar expanded, collapsed and with a tooltip at laptop width, write review screenshots with:

```sh
scripts/dev.sh env MUSICLIB_UI_SHOTS=/src/tmp/ui-shots go test -count=1 -run 'TestBrowser(Library|Album)Screenshots' ./internal/http/
```

The album shots show the page as loaded, with unsaved changes (also with the sidebar collapsed, at laptop width), after a conflict, with a track menu open, and a trashed album.

They land in `tmp/ui-shots/` (gitignored). Without the variable the test is skipped. The test browser has no CJK fonts, so Japanese titles render as boxes in these screenshots only.
