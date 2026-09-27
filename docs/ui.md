# Browser interface

Open `http://127.0.0.1:8080/` after starting the app (see [Docker](docker.md)). Use the exact host and port in `PUBLIC_ORIGIN`; a different Host is rejected. The interface is in Italian (NOTES.md N-243); the album, import and activity pages keep some English text until the redesign rounds 20 and 21.

## Layout

A sidebar on the left leads to **Libreria**, **Importa**, **Attività** and **Cestino** (the Library filtered to trashed albums). Below it, **Da sistemare** shows the number of albums whose last update failed and opens the Library filtered to them, trashed ones included; the number is computed when a page loads and is not refreshed in the background. The dot next to Attività appears while work is running on the Import and Activity pages. On a screen narrower than 52rem the sidebar becomes a row at the top, and Da sistemare shows as a red dot with its number only when it is not zero.

## Library

Albums appear as a grid of their covers, with title, artist and year underneath. An album without a cover shows a grey square with the initials of its title. A red dot on a cover means the album needs fixing (see Attività); a grey dot means it is waiting to be updated; an orange pulsing dot means it is being updated; an up-to-date album shows nothing. The word under the artist says the same thing in text.

The search at the top right stays in view while you scroll, with a thin line under it once the covers pass beneath; it filters as you type, by title or artist; matching artists are offered above the results, and choosing one shows only that artist's albums. More albums load as you scroll. Without JavaScript the search is an ordinary form and a **Mostra altri** link loads the next page.

Clicking a cover (or pressing Enter or Space on it) opens the album under its row: large cover, title, artist, genre and year, tracks (a track's own artist on a second line) and status, and **Modifica album**, which opens the editor. The panel takes its colours from the cover, with text contrast of at least 4.5:1. Esc, a second click, or another cover closes it. Arrow keys move between covers; Home and End go to the first and last. Without JavaScript a cover is a link to the album page.

## Album, Import, Activity

Album pages show metadata and downloads without JavaScript; editing requires JavaScript.

Import browses only `/import`; only listed directories are links. Choose a relative directory, then start a batch. A batch report can be bookmarked as `/import?batch=<id>` and shows the scan outcome, unassigned files, rejected entries and candidate jobs. A completed batch with no candidates shows the scan explanation. Import limits: **1,000 tracks and 10,000 files per candidate**. The browser retains the request UUID if the POST answer is lost: retry the same submission; using that UUID for a different path returns a conflict. Failed imports can be retried from the report with album artist/title overrides; blank override fields clear previously stored values. The server validates all inputs and rechecks the source on retry. Keep the source unchanged and mounted until the batch completes.

Activity shows pending, running and failed jobs. A failed job can be retried individually; **Retry failed** retries every failed job while leaving running work alone. **Render all** confirms before queuing every active album and unfinished trash removal; allow time and disk space. A pending/running job is never offered an individual retry. Import and Activity poll every two seconds only while work is pending/running; an idle page makes no polling requests. An import report can also be reached from Activity after navigating away.

The album editor saves all metadata in a single conditional PUT. Cover, attachments, lyrics, track removal, trash, restore and manual rendering are separate conditional commands. Reload after a 412 only **after** copying your edits: the page preserves the form and never adopts the winner's ETag automatically. Track and attachment deletion cannot be undone; a trashed album can be restored. Editing a file while metadata is unsaved asks before discarding those edits. A pending/running album is polled every two seconds until idle. Only the action that applies is offered: **Move to trash** for an active album, **Restore album** for a trashed one.

Cover uploads accept JPEG/PNG up to 20 MiB and 40 megapixels; they must additionally fit all track formats (FLAC: 16,777,173 bytes JPEG / 16,777,174 PNG). Attachment uploads are limited to 256 MiB; UTF-8 LRC uploads to 2 MiB. Relative paths and image formats are validated by the server. All downloads use entity IDs, never client-supplied filesystem paths.

## Appearance and assets

The interface follows the operating system's light or dark preference; there is no theme switcher. Reduced motion is honoured: the panel opens without sliding, dots stop pulsing and page changes do not animate. In browsers that support cross-document view transitions (Chromium), the cover of the open album moves into place when the editor opens; others simply navigate.

Assets are embedded in the server (`web/`) and served from `/static/`: one stylesheet, three small JavaScript modules (`app.js`, `queue.js`, `library.js`) and the Hanken Grotesk font (two WOFF2 files, SIL Open Font License, `/static/OFL.txt`). There is no frontend build, Node dependency, CDN or image file; the page CSP (`default-src 'self'`) blocks everything else, including `data:` images. The fonts are cached for a year (their names carry their version); the stylesheet and modules are not cached. CSS plus JavaScript stay under 60 KB (NOTES.md N-251).

## Tests and review screenshots

The dev/test Docker image includes Chromium solely for real-browser tests; the runtime image does not. Tests use chromedp with real PostgreSQL and ext4 on an ephemeral localhost port (NOTES.md N-203 to N-213, N-243 to N-252). To look at the Library in both themes at laptop and phone widths, write review screenshots with:

```sh
scripts/dev.sh env MUSICLIB_UI_SHOTS=/src/tmp/ui-shots go test -count=1 -run TestBrowserLibraryScreenshots ./internal/http/
```

They land in `tmp/ui-shots/` (gitignored). Without the variable the test is skipped. The test browser has no CJK fonts, so Japanese titles render as boxes in these screenshots only.
