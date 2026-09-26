# Browser interface

Open `http://127.0.0.1:8080/` after starting the app (see [Docker](docker.md)). Use the exact host and port in `PUBLIC_ORIGIN`; a different Host is rejected. Library search and filtering work as plain GETs, including without JavaScript. Album pages show metadata and downloads without JavaScript; editing requires JavaScript. Import and Activity are linked placeholder pages until the next UI round.

The album editor saves all metadata in a single conditional PUT. Cover, attachments, lyrics, track removal, trash, restore and manual rendering are separate conditional commands. Reload after a 412 only **after** copying your edits: the page preserves the form and never adopts the winner's ETag automatically. Track and attachment deletion cannot be undone; a trashed album can be restored. Editing a file while metadata is unsaved asks before discarding those edits. A pending/running album is polled every two seconds until idle.

Cover uploads accept JPEG/PNG up to 20 MiB and 40 megapixels; they must additionally fit all track formats (FLAC: 16,777,173 bytes JPEG / 16,777,174 PNG). Attachment uploads are limited to 256 MiB; UTF-8 LRC uploads to 2 MiB. Relative paths and image formats are validated by the server. All downloads use entity IDs, never client-supplied filesystem paths.

Assets are embedded in the server (`web/`); there is no frontend build or Node dependency. The dev/test Docker image includes Chromium solely for real-browser tests; the runtime image does not. Tests use chromedp with real PostgreSQL and ext4. See NOTES.md N-203–N-206 for the browser pin and remaining build reproducibility risk.
