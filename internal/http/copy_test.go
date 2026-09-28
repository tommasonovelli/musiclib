package http

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/jobs"
	"musiclib/web"
)

// The interface speaks English (owner, 2026-09-27; NOTES.md N-256). These
// guards fail if the Italian copy of round 19 comes back: a short explicit
// list of its words, matched as whole words in any letter case, in the
// embedded templates and modules and in the pages the server renders.
var italianCopy = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(libreria|importa|importare|attività|cestino|sistemare|aggiornato|attesa|aggiornamento|mostra|modifica|nessun|nessuna|cerca|contenuto|artisti|artista|tutti|vuota|vuoto|riprova|disco|album di|la tua|il tuo|gli originali|lavoro in corso|non ha risposto|sezioni)(?:[^\p{L}]|$)|lang="it"`)

func italianIn(s string) []string {
	var found []string
	for _, m := range italianCopy.FindAllStringSubmatch(s, -1) {
		found = append(found, strings.TrimSpace(m[0]))
	}
	return found
}

func TestNoItalianCopyInAssets(t *testing.T) {
	checked := 0
	for _, pattern := range []string{"*.html", "*.js"} {
		names, err := fs.Glob(web.Assets, pattern)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			b, err := web.Assets.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			checked++
			if found := italianIn(string(b)); len(found) != 0 {
				t.Errorf("%s: Italian copy %q", name, found)
			}
		}
	}
	// layout, library, album, import, activity; app.js, library.js,
	// queue.js, sidebar.js.
	if checked < 9 {
		t.Fatalf("only %d files checked", checked)
	}
	layout, err := web.Assets.ReadFile("layout.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(layout), `<html lang="en">`) {
		t.Error(`the layout is not lang="en"`)
	}
}

// The guard itself: each word of the list is caught, English is not.
func TestItalianGuardCatches(t *testing.T) {
	for _, s := range []string{`<a href="/">Libreria</a>`, `>Importa musica<`, "Attività", "Da sistemare", "In attesa", "`Disco ${disc}`", "'Modifica album'", `<html lang="it">`, "LA TUA LIBRERIA È VUOTA."} {
		if len(italianIn(s)) == 0 {
			t.Errorf("not caught: %s", s)
		}
	}
	for _, s := range []string{"Library", "Import music", "Important", "Needs attention", "Disc 2", "Edit album", "Albums by Miles Davis.", "Searching", "Discography", `<html lang="en">`} {
		if found := italianIn(s); len(found) != 0 {
			t.Errorf("false positive in %q: %q", s, found)
		}
	}
}

// Every page renders without Italian copy, the titles and status words from
// pages.go included: the album page too, active with a status word and
// trashed.
func TestPagesSpeakEnglish(t *testing.T) {
	e := pageEnv(t)
	failed := e.seed("Miles Davis", "Kind of Blue")
	e.failRender(failed)
	e.seed("Bill Evans", "Sunday at the Village Vanguard")
	running := e.seed("Chet Baker", "Chet")
	e.runRender(running)
	gone := e.seed("Bill Evans", "Portrait")
	_, etag := e.album(gone)
	e.must(req{method: "DELETE", path: "/api/albums/" + gone.String(), ifMatch: etag}, 200)
	// The Import results and Activity with failures: their sentences come
	// from queuepages.go, not from a template (N-295).
	batch := e.newImport("Jazz")
	e.batchJob(batch, "Jazz/A", "failed", "mixed_album", "two album tags", uuid.Nil)
	e.batchJob(batch, "Jazz/B", "failed", "corrupt_audio", "damaged", uuid.Nil)
	e.batchJob(batch, "Jazz/C", "failed", "media_timeout", "slow", uuid.Nil)
	e.batchJob(batch, "Jazz/D", "done", "", "", failed)
	e.batchJob(batch, "Jazz/E", "skipped", "", "", running)
	e.files("Jazz/Kind of Blue/01.flac", "Jazz/Kind of Blue/cover.jpg")
	results := "/import?batch=" + batch.String()
	for _, path := range []string{"/", "/?q=miles", "/?q=zzz", "/?artist=" + e.artistOf(failed).String(), "/?artist=" + uuid.New().String(), "/?fix=true", "/?trash=true", "/import", "/activity",
		"/import?path=Jazz", "/import?path=Jazz%2FKind+of+Blue", "/import?path=Gone", "/import?batch=" + uuid.New().String(),
		results, results + "&tab=imported", results + "&tab=present",
		"/albums/" + failed.String(), "/albums/" + running.String(), "/albums/" + gone.String()} {
		status, _, body := pageRequest(t, e, path, testHost)
		if status != 200 {
			t.Fatalf("%s: %d", path, status)
		}
		if found := italianIn(body); len(found) != 0 {
			t.Errorf("%s: Italian copy %q", path, found)
		}
	}
	// Every sentence of the code table and its fallbacks, whether or not a
	// page above showed it.
	for c, p := range problems {
		if found := italianIn(p.Sentence); len(found) != 0 {
			t.Errorf("%s: Italian copy %q", c, found)
		}
	}
	for _, s := range []string{jobProblem(jobs.KindScan, nil).Sentence, jobProblem(jobs.KindImport, nil).Sentence, renderProblem(nil)} {
		if found := italianIn(s); len(found) != 0 {
			t.Errorf("%q: Italian copy %q", s, found)
		}
	}
	empty := pageEnv(t)
	if _, _, body := pageRequest(t, empty, "/", testHost); len(italianIn(body)) != 0 || !strings.Contains(body, "Your library is empty.") {
		t.Errorf("empty library: %q", italianIn(body))
	}
}
