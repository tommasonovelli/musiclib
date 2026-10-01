package http

import (
	"errors"
	"fmt"
	nethttp "net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/internal/fsops"
	"musiclib/internal/importer"
	"musiclib/internal/jobs"
	"musiclib/internal/names"
)

// The Import and Activity pages (round 21, webui-principles «Importa» and
// «Attività»; NOTES.md N-286 to N-290). Both are rendered by the server,
// so that they read, browse and switch tabs without JavaScript; queue.js
// adds the actions and keeps them current by adopting the page's own rows
// (N-291). The words follow the glossary: a music folder and folders, an
// import and its results, an album found; never a job, a batch or a code.

// pageTime is a moment as the queue views show it (N-290): ISO 8601 for
// <time datetime>, the relative words, the full date for its title.
type pageTime struct {
	ISO, Rel, Full string
}

// fullDate is the server's full date: UTC, which it says, since the
// server does not know the reader's time zone; queue.js rewrites it in the
// browser's own.
const fullDate = "2 January 2006 at 15:04 UTC"

func timeOf(t, now time.Time) pageTime {
	u := t.UTC()
	return pageTime{ISO: u.Format(time.RFC3339), Rel: relativeTime(t, now), Full: u.Format(fullDate)}
}

// relativeTime is the time since t in words, as queue.js writes it too:
// "just now" under a minute (and for a moment in the future: the clocks
// of the database and the server may differ a little), then minutes,
// hours and days ago, then the date.
func relativeTime(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return count(int(d/time.Minute), "minute", "minutes") + " ago"
	case d < 24*time.Hour:
		return count(int(d/time.Hour), "hour", "hours") + " ago"
	case d < 30*24*time.Hour:
		return count(int(d/(24*time.Hour)), "day", "days") + " ago"
	}
	return t.UTC().Format("2 January 2006")
}

// count is "1 track", "2 tracks".
func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// problem is how a failed scan or import is shown (N-287): one plain
// sentence that says what happened and what to do, and the one fix the
// page offers. Fix is "title" or "artist" (the retry with that override of
// §7.3), "retry" (trying again can work as the files are), or "" (only a
// change to the files can help, and the sentence says which).
type problem struct {
	Sentence, Fix string
}

const (
	fixTitle  = "title"
	fixArtist = "artist"
	fixRetry  = "retry"
	againTail = ", then import the folder again."
)

// problems maps the error codes of a failed scan or import (the importer's,
// the catalog's and the names codes it stores, §7) to their problem. Any
// other code is jobProblem's fallback: a failure of the machine rather than
// of the files (a tool, the disk, the database), which a retry can mend.
var problems = map[string]problem{
	importer.CodeMixedAlbum:           {"The tracks have different album names. Give the album one title.", fixTitle},
	importer.CodeAlbumTitleMissing:    {"The album has no name. Give it a title.", fixTitle},
	catalog.CodeAlbumFolderConflict:   {"The artist already has an album with this title: open it and add the tracks there, or give this one another title.", fixTitle},
	catalog.CodePathReserved:          {"Another album is using this name in the library folder. Give this one another title.", fixTitle},
	importer.CodeAmbiguousAlbumArtist: {"The tracks name different album artists. Choose the album’s artist.", fixArtist},
	catalog.CodeArtistFolderConflict:  {"An artist with almost the same name is already in your library. Type the name as it is there, or another one.", fixArtist},

	importer.CodeAmbiguousCandidate: {"This folder has tracks, and more tracks in its subfolders. Import the subfolders one by one.", ""},
	importer.CodeNotACandidate:      {"There are no tracks directly in this folder. Import its subfolders instead.", ""},
	importer.CodeNoValidCandidate:   {"No albums found. Put each album’s tracks in a folder of its own" + againTail, ""},
	importer.CodeDuplicateDisc:      {"Two disc folders have the same number. Rename one" + againTail, ""},
	importer.CodeCorruptAudio:       {"A track is damaged and can’t be played. Replace it" + againTail, ""},
	importer.CodeUnsupportedAudio:   {"A track is in a format that can’t be imported. Convert it to FLAC, MP3 or M4A" + againTail, ""},
	importer.CodeUnrenderableTag:    {"A track has a tag that can’t be kept as it is. Fix the track’s tags" + againTail, ""},
	importer.CodeInvalidTag:         {"A track has an empty tag or one with an invisible character. Fix the track’s tags" + againTail, ""},
	catalog.CodeTooManyFiles:        {"The album has more than 1,000 tracks or 10,000 files. Split it" + againTail, ""},
	catalog.CodeInvalidDisc:         {"A disc number is out of range. Fix the track’s tags" + againTail, ""},
	catalog.CodeInvalidTrackNumber:  {"A track number is out of range. Fix the track’s tags" + againTail, ""},
	catalog.CodeLyricsAssociation:   {"A lyrics file matches more than one track. Rename it" + againTail, ""},
	importer.CodeSourceRejected:     {"The folder holds a link or a special file, which can’t be imported. Remove it" + againTail, ""},
	importer.CodeSourceNotDirectory: {"This is a file, not a folder. Import the folder that holds it.", ""},

	importer.CodeSourceNotFound:    {"The folder isn’t there any more. Check that the music folder is connected, then retry.", fixRetry},
	importer.CodeSourceNotReadable: {"The folder can’t be read. Check its permissions, then retry.", fixRetry},
	importer.CodeSourceChanged:     {"The folder changed during the import. Retry once nothing else is changing it.", fixRetry},
	importer.CodeInsufficientSpace: {"There isn’t enough disk space. Free some space, then retry.", fixRetry},
}

// jobProblem is the problem of a failed scan or import.
func jobProblem(kind jobs.Kind, code *string) problem {
	if code != nil {
		if p, ok := problems[*code]; ok {
			return p
		}
	}
	if kind == jobs.KindScan {
		return problem{"Looking for albums didn’t finish. Try again.", fixRetry}
	}
	return problem{"The import didn’t finish. Try again.", fixRetry}
}

// renderProblem is the sentence of a failed render: the album could not be
// written to the library folder. Activity's one action for it is «Retry
// all»; the album's own page has «Update in library» (N-263).
func renderProblem(code *string) string {
	if code != nil && *code == importer.CodeInsufficientSpace {
		return "There isn’t enough disk space. Free some space, then retry."
	}
	return "The album couldn’t be written to the library folder. Try again."
}

// details is the technical answer behind a sentence, for the closed
// «Details» (N-261): the code and the stored message, database text never
// shown (jobMessage, N-150).
func details(code, message *string) string {
	if code == nil {
		return ""
	}
	if m := jobMessage(code, message); m != nil && *m != "" {
		return *code + ": " + *m
	}
	return *code
}

// folderName is the last segment of a path relative to /import, and
// «Music folder» for /import itself; folderWhere is where it is: its
// parent's path, or «Music folder» at the top.
func folderName(rel string) string {
	if rel == "" {
		return "Music folder"
	}
	return path.Base(rel)
}

func folderWhere(rel string) string {
	if dir := path.Dir(rel); rel != "" && dir != "." {
		return dir
	}
	return "Music folder"
}

// jobVersion changes whenever a row's content can: a retry gives a new
// ticket, every transition a new state or time (N-291).
func jobVersion(ticket int64, state jobs.State, updated time.Time) string {
	return fmt.Sprintf("%d.%s.%d", ticket, state, updated.UnixNano())
}

// ---- Import ---------------------------------------------------------------

type importView struct {
	Browse  *browseView
	Results *resultsView
	// Missing: the import asked for is not there (never made, or past the
	// 90 days of §6.4).
	Missing bool
}

// browseView is the music folder, Finder-style (N-286): the open folder's
// path as a breadcrumb, its folders as rows with what each holds, its
// files summed up, and the entries the scan never follows or opens.
type browseView struct {
	Path string
	// Name is the open folder in the button's words.
	Name    string
	Crumbs  []crumb
	Files   string
	Folders []folderRow
	Skipped []skippedRow
	// Problem is set when the folder cannot be listed.
	Problem *pageProblem
	// Empty: nothing at all in the folder; Root: the folder is /import.
	Empty, Root bool
	Recent      []recentRow
}

type crumb struct{ Name, URL string }

type folderRow struct{ Name, URL, Summary string }

type skippedRow struct{ Name, Word string }

type pageProblem struct{ Sentence, Details string }

type recentRow struct {
	URL, Name, Where, Summary string
	Time                      pageTime
	Active                    bool
}

// resultsView is an import: while it runs, one progress row; its albums
// found in three tabs (N-286).
type resultsView struct {
	ID, Name, Where string
	Started         pageTime
	Active          bool
	Scanning        bool
	// Step is the album being imported, of Total; Done have an outcome.
	Done, Step, Total int
	// Skipped are the files no album took, and the entries the scan
	// refused (§7.2): a path and why.
	Skipped []string
	Tabs    []resultTab
}

type resultTab struct {
	Key, Label, URL, Empty string
	Count                  int
	Selected               bool
	Rows                   []resultRow
}

type resultRow struct {
	Key, Version       string
	Title, Note, Trash string
	URL, AlbumID       string
	Cover              bool
	Initials           string
	// A row that needs attention: its problem, the stored overrides the
	// fix starts from, the technical details, and the job to act on.
	Problem       *problem
	Artist, Album string
	Details       string
	Notes         []string
}

// The tabs of the results, in their order.
const (
	tabAttention = "attention"
	tabImported  = "imported"
	tabPresent   = "present"
)

// entryWords are the words of the entries listed but never followed.
var entryWords = map[string]string{
	importer.EntrySymlink:     "Link, not followed",
	importer.EntrySpecial:     "Special file, skipped",
	importer.EntryInvalidName: "Unreadable name, skipped",
}

func importURL(key, value string) string {
	if value == "" && key == "path" {
		return "/import"
	}
	return "/import?" + url.Values{key: {value}}.Encode()
}

func (a *API) importPage(w nethttp.ResponseWriter, r *nethttp.Request, b Backend) {
	q, e := queryParams(r, "path", "batch", "tab")
	var batch uuid.UUID
	if e == nil {
		batch, e = queryID(q, "batch")
	}
	tab, hasTab := q["tab"]
	if e == nil && hasTab && tab != tabAttention && tab != tabImported && tab != tabPresent {
		e = invalidField("tab", "must be attention, imported or present")
	}
	if e != nil {
		a.writeError(w, e)
		return
	}
	now := time.Now()
	d := pageData{Title: "Import", Nav: "import", Queue: true, Import: &importView{}}
	if batch == uuid.Nil {
		v, err := a.browse(r, b, q["path"], now)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		d.Import.Browse = v
		a.renderPage(w, r, b.Catalog, "import.html", d)
		return
	}
	rep, err := b.Catalog.GetImportReport(r.Context(), batch)
	if catalog.Code(err) == catalog.CodeImportBatchNotFound {
		d.Import.Missing = true
		a.renderPage(w, r, b.Catalog, "import.html", d)
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	v, err := results(r, b.Catalog, rep, tab, now)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	d.Import.Results, d.Active = v, v.Active
	a.renderPage(w, r, b.Catalog, "import.html", d)
}

// browse lists the folder rel of the music folder, and each of its folders
// one level down for their summaries: names and types only (§7.1, §10.4).
// A folder that cannot be listed is a sentence on the page, not an error.
func (a *API) browse(r *nethttp.Request, b Backend, rel string, now time.Time) (*browseView, error) {
	v := &browseView{Path: rel, Root: rel == "", Name: "the music folder"}
	if !v.Root {
		v.Name = path.Base(rel)
	}
	v.Crumbs = []crumb{{Name: "Music folder", URL: "/import"}}
	if segs, err := names.SplitRelPathOrRoot(rel); err == nil {
		for i, s := range segs {
			v.Crumbs = append(v.Crumbs, crumb{Name: s, URL: importURL("path", strings.Join(segs[:i+1], "/"))})
		}
	}
	v.Crumbs[len(v.Crumbs)-1].URL = ""
	recent, err := b.Catalog.ListRecentImports(r.Context())
	if err != nil {
		return nil, err
	}
	for _, ri := range recent {
		v.Recent = append(v.Recent, recentRow{URL: importURL("batch", ri.ID.String()), Name: folderName(ri.RootRel),
			Where: folderWhere(ri.RootRel), Summary: recentSummary(ri), Time: timeOf(ri.CreatedAt, now), Active: ri.Active > 0})
	}
	ents, err := importer.Browse(b.Source, rel)
	if err != nil {
		p, ok := browseProblem(rel, err)
		if !ok {
			return nil, err
		}
		v.Problem = p
		return v, nil
	}
	importer.SortForDisplay(ents)
	var files []importer.SourceEntry
	for _, en := range ents {
		name := importer.DisplayName(en.Name)
		switch en.Type {
		case importer.EntryDirectory:
			child := name
			if rel != "" {
				child = rel + "/" + name
			}
			row := folderRow{Name: name, URL: importURL("path", child), Summary: "Can’t be read"}
			if sub, err := importer.Browse(b.Source, child); err == nil {
				row.Summary = summaryWords(importer.Tally(sub), true)
			} else if _, ok := browseProblem(child, err); !ok {
				return nil, err
			}
			v.Folders = append(v.Folders, row)
		case importer.EntryFile:
			files = append(files, en)
		default:
			v.Skipped = append(v.Skipped, skippedRow{Name: name, Word: entryWords[en.Type]})
		}
	}
	v.Files = summaryWords(importer.Tally(files), false)
	v.Empty = len(ents) == 0
	return v, nil
}

// browseProblem is the sentence of a folder that cannot be listed: the
// source codes of importer.Browse and an invalid path. ok is false for
// anything else, a failure of the server.
func browseProblem(rel string, err error) (*pageProblem, bool) {
	code := importer.Code(err)
	var ne *names.Error
	var ie *importer.Error
	if !errors.As(err, &ie) && !errors.As(err, &ne) {
		return nil, false
	}
	sentence := "This folder can’t be opened."
	switch code {
	case importer.CodeSourceNotFound:
		sentence = "This folder isn’t there any more."
		if rel == "" {
			sentence = "Your music folder isn’t connected. Connect it, then reload this page."
		}
	case importer.CodeSourceNotReadable, fsops.CodePermission:
		sentence = "This folder can’t be read. Check its permissions."
	case importer.CodeSourceRejected:
		sentence = "This is a link, and links aren’t followed."
	case importer.CodeSourceNotDirectory:
		sentence = "This is a file, not a folder."
	}
	return &pageProblem{Sentence: sentence, Details: err.Error()}, true
}

// summaryWords is what a folder holds in words: «4 folders, 14 tracks,
// 2 images». withFolders counts its folders and skipped entries too (a
// row of the list); without, only the open folder's own files.
func summaryWords(s importer.Summary, withFolders bool) string {
	var parts []string
	add := func(n int, one, many string) {
		if n > 0 {
			parts = append(parts, count(n, one, many))
		}
	}
	if withFolders {
		add(s.Folders, "folder", "folders")
	}
	add(s.Tracks, "track", "tracks")
	add(s.Images, "image", "images")
	add(s.Others, "other file", "other files")
	if withFolders {
		add(s.Skipped, "skipped", "skipped")
		if len(parts) == 0 {
			return "Empty"
		}
	}
	return strings.Join(parts, ", ")
}

func recentSummary(ri catalog.RecentImport) string {
	if ri.Active > 0 {
		return "Importing…"
	}
	var parts []string
	if ri.Imported > 0 {
		parts = append(parts, fmt.Sprintf("%d imported", ri.Imported))
	}
	if ri.Present > 0 {
		parts = append(parts, fmt.Sprintf("%d already there", ri.Present))
	}
	if ri.Attention > 0 {
		parts = append(parts, count(ri.Attention, "needs attention", "need attention"))
	}
	if len(parts) == 0 {
		return "Nothing imported"
	}
	return strings.Join(parts, ", ")
}

// results builds the results of an import (N-286): the progress while it
// runs, and its albums found in three tabs. A failed scan or import that
// no longer needs attention (dismissed or superseded, N-285) is left out.
// The open tab is tab, or the first that is not empty.
func results(r *nethttp.Request, c *catalog.Service, rep catalog.ImportReport, tab string, now time.Time) (*resultsView, error) {
	state := rep.State()
	v := &resultsView{ID: rep.Batch.ID.String(), Name: folderName(rep.Batch.RootRel), Where: folderWhere(rep.Batch.RootRel),
		Started: timeOf(rep.Batch.CreatedAt, now), Active: state != catalog.BatchCompleted, Scanning: state == catalog.BatchScanning,
		Total: len(rep.Imports)}
	var albums []uuid.UUID
	for _, j := range rep.Imports {
		if j.State.Terminal() {
			v.Done++
		}
		if j.ResultAlbumID != nil {
			albums = append(albums, *j.ResultAlbumID)
		}
	}
	v.Step = min(v.Done+1, v.Total)
	cards, err := c.AlbumCards(r.Context(), albums)
	if err != nil {
		return nil, err
	}
	for _, w := range rep.Scan.Warnings {
		why := "not in any album"
		if w.Code == jobs.WarnRejectedEntry {
			why = "a link or a special file"
		}
		where := w.Path
		if where == "" {
			where = w.Message
		}
		v.Skipped = append(v.Skipped, where+": "+why)
	}
	tabs := map[string]*resultTab{
		tabAttention: {Key: tabAttention, Label: "Needs attention", Empty: "Nothing needs attention."},
		tabImported:  {Key: tabImported, Label: "Imported", Empty: "No albums imported."},
		tabPresent:   {Key: tabPresent, Label: "Already there", Empty: "None of these albums was already there."},
	}
	for _, j := range append([]catalog.JobView{rep.Scan}, rep.Imports...) {
		rel := rep.Batch.RootRel
		if j.SourceRel != nil {
			rel = *j.SourceRel
		}
		row := resultRow{Key: j.ID.String(), Version: jobVersion(j.Ticket, j.State, j.UpdatedAt),
			Title: folderName(rel), Note: folderWhere(rel), Initials: initials(folderName(rel))}
		var t *resultTab
		switch {
		case j.State == jobs.StateFailed && j.Attention:
			t = tabs[tabAttention]
			p := jobProblem(j.Kind, j.ErrorCode)
			row.Problem, row.Details = &p, details(j.ErrorCode, j.ErrorMessage)
			if j.Overrides.Artist != nil {
				row.Artist = *j.Overrides.Artist
			}
			if j.Overrides.Title != nil {
				row.Album = *j.Overrides.Title
			}
		case j.Kind == jobs.KindImport && (j.State == jobs.StateDone || j.State == jobs.StateSkipped):
			t = tabs[tabImported]
			if j.State == jobs.StateSkipped {
				t = tabs[tabPresent]
			}
			if j.ResultAlbumID != nil {
				if card, ok := cards[*j.ResultAlbumID]; ok {
					row.Title, row.Note, row.Initials, row.Cover = card.Title, card.ArtistName, initials(card.Title), card.Cover
					row.AlbumID, row.URL = card.ID.String(), "/albums/"+card.ID.String()
					if card.Trashed {
						row.Trash = "It’s in the trash: open it to restore it."
					}
				}
			}
			for _, w := range j.Warnings {
				row.Notes = append(row.Notes, w.Message)
			}
		default:
			continue
		}
		t.Rows = append(t.Rows, row)
		t.Count++
	}
	if tab == "" {
		tab = tabAttention
		for _, k := range []string{tabAttention, tabImported, tabPresent} {
			if tabs[k].Count > 0 {
				tab = k
				break
			}
		}
	}
	for _, k := range []string{tabAttention, tabImported, tabPresent} {
		t := tabs[k]
		t.URL = "/import?" + url.Values{"batch": {v.ID}, "tab": {k}}.Encode()
		t.Selected = k == tab
		v.Tabs = append(v.Tabs, *t)
	}
	return v, nil
}

// ---- Activity ---------------------------------------------------------------

// activityRows is how many jobs of each state Activity lists: a rebuild
// of the whole library waits in thousands, and a page of that size polled
// every two seconds would be the heaviest thing the UI does (N-289).
const activityRows = 100

type activityView struct {
	Groups []activityGroup
	// Empty: nothing in progress, waiting or needing attention.
	Empty bool
	// Rebuild says how many albums «Rebuild the library folder» queues.
	Rebuild string
}

type activityGroup struct {
	Key, Title string
	Rows       []activityRow
	Total      int
	// More is how many are not listed.
	More int
}

type activityRow struct {
	Key, Version string
	Title, Note  string
	URL, AlbumID string
	Initials     string
	Cover        bool
	TimeWord     string
	Time         pageTime
	Sentence     string
	Details      string
	Dismissable  bool
}

func (a *API) activityPage(w nethttp.ResponseWriter, r *nethttp.Request, c *catalog.Service) {
	if _, e := queryParams(r); e != nil {
		a.writeError(w, e)
		return
	}
	act, err := c.ListActivity(r.Context(), activityRows)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	n, err := c.CountRenderAll(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	now := time.Now()
	v := &activityView{Rebuild: count(int(n), "album is", "albums are") + " written to the library folder again. This takes time and disk space."}
	for _, g := range []struct {
		key, title string
		group      catalog.ActivityGroup
	}{{"running", "In progress", act.Running}, {"pending", "Waiting", act.Pending}, {"failed", "Needs attention", act.Failed}} {
		ag := activityGroup{Key: g.key, Title: g.title, Total: g.group.Total, More: g.group.Total - len(g.group.Jobs)}
		for _, j := range g.group.Jobs {
			ag.Rows = append(ag.Rows, activityRowOf(j, now))
		}
		v.Groups = append(v.Groups, ag)
	}
	v.Empty = act.Running.Total+act.Pending.Total+act.Failed.Total == 0
	d := pageData{Title: "Activity", Nav: "activity", Queue: true, Activity: v,
		Active: act.Running.Total+act.Pending.Total > 0}
	a.renderPage(w, r, c, "activity.html", d)
}

// activityRowOf is a job as a row of Activity: the album's name and cover,
// or the folder of a scan or an import, never the kind of work (N-289).
func activityRowOf(j catalog.ActivityJob, now time.Time) activityRow {
	row := activityRow{Key: j.ID.String(), Version: jobVersion(j.Ticket, j.State, j.UpdatedAt),
		Title: folderName(j.Folder), Note: folderWhere(j.Folder), Initials: initials(folderName(j.Folder))}
	if j.BatchID != nil {
		row.URL = importURL("batch", j.BatchID.String())
	}
	if al := j.Album; al != nil {
		row.Title, row.Note, row.Initials, row.Cover = al.Title, al.ArtistName, initials(al.Title), al.Cover
		row.AlbumID, row.URL = al.ID.String(), "/albums/"+al.ID.String()
	}
	switch j.State {
	case jobs.StateRunning:
		row.TimeWord, row.Time = "Started", timeOf(j.UpdatedAt, now)
	case jobs.StatePending:
		row.TimeWord, row.Time = "Added", timeOf(j.QueuedAt, now)
	default:
		row.Time = timeOf(j.UpdatedAt, now)
		row.Details = details(j.ErrorCode, j.ErrorMessage)
		if j.Kind == jobs.KindRender {
			row.Sentence = renderProblem(j.ErrorCode)
		} else {
			row.Sentence, row.Dismissable = jobProblem(j.Kind, j.ErrorCode).Sentence, true
		}
	}
	return row
}
