package http

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	"github.com/google/uuid"
)

// The album editor's behaviour in a real Chromium (round 20, NOTES.md
// N-256 to N-267): saving in place, the Save bar, inherited values, the
// artist, the conflict, the cover, lyrics, the trash, menus, errors and
// escaping. Every check reads the real server and PostgreSQL afterwards.

// editorJS are the page helpers of the tests' scripts: set gives a field
// a value and the events typing would fire.
const editorJS = `const $=s=>document.querySelector(s),$$=s=>[...document.querySelectorAll(s)],row=i=>$$('[data-track]')[i],` +
	`set=(e,v)=>{e.value=v;e.dispatchEvent(new Event('input',{bubbles:true}));e.dispatchEvent(new Event('change',{bubbles:true}))};`

// js runs statements with the helpers and returns what they return.
func js(t *testing.T, tab context.Context, body string) string {
	t.Helper()
	return browserEval(t, tab, `(()=>{`+editorJS+body+`})()`)
}

// typeAt types text at the end of a text field with real key events (a
// user activation, which the beforeunload prompt needs).
func typeAt(t *testing.T, tab context.Context, selector, text string) {
	t.Helper()
	browserEval(t, tab, `(e=>{e.focus();e.setSelectionRange(e.value.length,e.value.length);return ''})(document.querySelector(`+"`"+selector+"`"+`))`)
	if err := chromedp.Run(tab, chromedp.KeyEvent(text)); err != nil {
		t.Fatal(err)
	}
}

// scriptProblems are the console problems that are not the browser's own
// report of an HTTP error status, which the error tests provoke on purpose.
func scriptProblems(p []string) []string {
	var out []string
	for _, s := range p {
		if !strings.Contains(s, "Failed to load resource") {
			out = append(out, s)
		}
	}
	return out
}

func noProblems(t *testing.T, tab context.Context, problems func() []string) {
	t.Helper()
	if got := browserEval(t, tab, `JSON.stringify(window.cspViolations)`); got != "[]" {
		t.Fatalf("CSP violations: %s", got)
	}
	if p := scriptProblems(problems()); len(p) != 0 {
		t.Fatalf("console: %q", p)
	}
}

func trackOf(t *testing.T, e *env, id uuid.UUID, i int) map[string]any {
	t.Helper()
	body, _ := e.album(id)
	return tracksOf(body)[i]
}

// Editing and saving without a reload: the Save bar counts the real edits,
// «Saved» shows for two seconds, the ETag comes from the answer, a second
// save needs no reload, and leaving with unsaved edits asks first.
func TestBrowserAlbumSaveInPlace(t *testing.T) {
	if _, err := os.Stat("/usr/bin/chromium"); err != nil {
		t.Fatal("the gate must provide Chromium: ", err)
	}
	e, root := browserEnv(t)
	id := e.seedAlbum("Miles Davis", "Kind of Blue", kindOfBlue(), nil)
	url := e.srv.URL + "/albums/" + id.String()
	tab, problems := albumTab(t, root, url)
	dialogs := make(chan page.DialogType, 4)
	chromedp.ListenTarget(tab, func(ev any) {
		if d, ok := ev.(*page.EventJavascriptDialogOpening); ok {
			dialogs <- d.Type
			// Stay on the page (chromedp answers dialogs from a goroutine).
			go func() { _ = chromedp.Run(tab, page.HandleJavaScriptDialog(false)) }()
		}
	})
	js(t, tab, `window.marker='kept';return ''`)
	if got := js(t, tab, `return [$('#changes').textContent, $('#save').disabled, getComputedStyle($('#savebar')).visibility].join('|')`); got != "|true|hidden" {
		t.Fatalf("clean page: %s", got)
	}
	typeAt(t, tab, "#album-title", " (Mono)")
	browserWait(t, tab, `document.querySelector('#changes').textContent === '1 change' && document.querySelector('#savebar.is-on') && !document.querySelector('#save').disabled`)
	// A second field counts; putting a value back uncounts it.
	js(t, tab, `set(row(2).querySelector('[name=title]'), 'Blue');return ''`)
	browserWait(t, tab, `document.querySelector('#changes').textContent === '2 changes'`)
	js(t, tab, `set(row(2).querySelector('[name=title]'), 'Blue in Green');return ''`)
	browserWait(t, tab, `document.querySelector('#changes').textContent === '1 change'`)

	// A download is not leaving: no prompt, the edit stays (N-280). The
	// click runs after the evaluation returns, as a prompt would block it.
	js(t, tab, `row(0).querySelector('.dots').click();setTimeout(()=>row(0).querySelector('.menu a[href$="/original"]').click());return ''`)
	time.Sleep(time.Second)
	select {
	case typ := <-dialogs:
		t.Fatalf("a download asked to leave: %s", typ)
	default:
	}
	js(t, tab, `$('.menu:popover-open')?.hidePopover();return ''`)

	// Leaving asks first; staying keeps the edit.
	browserEval(t, tab, `location.href='/';''`)
	select {
	case typ := <-dialogs:
		if typ != page.DialogTypeBeforeunload {
			t.Fatalf("dialog %s", typ)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no beforeunload prompt with unsaved edits")
	}
	if got := js(t, tab, `return [location.pathname, window.marker, $('#album-title').value].join('|')`); got != "/albums/"+id.String()+"|kept|Kind of Blue (Mono)" {
		t.Fatalf("after staying: %s", got)
	}

	js(t, tab, `$('#save').click();return ''`)
	browserWait(t, tab, `document.querySelector('#changes').textContent === 'Saved' && document.querySelector('#savebar.is-on') !== null`)
	body, etag := e.album(id)
	if body["title"] != "Kind of Blue (Mono)" {
		t.Fatalf("saved title: %v", body["title"])
	}
	if got := js(t, tab, `return [location.pathname, window.marker, $('#editor').dataset.etag, $('#save').disabled, $$('.notice:not([hidden])').length].join('|')`); got != "/albums/"+id.String()+"|kept|"+etag+"|true|0" {
		t.Fatalf("after save (no reload, the answer's ETag): %s, want %s", got, etag)
	}
	// «Saved» goes after two seconds, and the bar with it.
	browserWait(t, tab, `document.querySelector('#changes').textContent === '' && !document.querySelector('#savebar.is-on')`)

	// A second save with the new ETag: Enter in the title, no 412.
	typeAt(t, tab, "#album-title", "!")
	browserWait(t, tab, `document.querySelector('#changes').textContent === '1 change'`)
	if err := chromedp.Run(tab, chromedp.KeyEvent(kb.Enter)); err != nil {
		t.Fatal(err)
	}
	browserWait(t, tab, `document.querySelector('#changes').textContent === 'Saved'`)
	if body, _ := e.album(id); body["title"] != "Kind of Blue (Mono)!" {
		t.Fatalf("second save: %v", body["title"])
	}
	if got := js(t, tab, `return [window.marker, $('#album-title').value, $$('.notice:not([hidden])').length].join('|')`); got != "kept|Kind of Blue (Mono)!|0" {
		t.Fatalf("after the second save: %s", got)
	}

	// A clean page leaves without asking.
	browserEval(t, tab, `location.href='/';''`)
	browserWait(t, tab, `location.pathname === '/'`)
	select {
	case typ := <-dialogs:
		t.Fatalf("clean page asked: %s", typ)
	default:
	}
	noProblems(t, tab, problems)
}

// Inherited values: the album's value in graphite in an empty field,
// clearing goes back to inheriting, «No genre» is an explicit choice.
func TestBrowserAlbumInheritedValues(t *testing.T) {
	e, root := browserEnv(t)
	id := e.seedAlbum("Miles Davis", "Kind of Blue", kindOfBlue(), nil)
	tab, problems := albumTab(t, root, e.srv.URL+"/albums/"+id.String())
	placeholders := `return $$('[data-track]').map(r=>r.querySelector('[name=artist]').value+'/'+r.querySelector('[name=artist]').placeholder+'/'+r.querySelector('[name=genre]').value+'/'+r.querySelector('[name=genre]').placeholder).join('|')`
	if got := js(t, tab, placeholders); got != "/Miles Davis//Jazz|Miles Davis & Wynton Kelly/Miles Davis//Jazz|/Miles Davis//Jazz|/Miles Davis/Modal jazz/Jazz|/Miles Davis//Jazz|/Miles Davis//No genre" {
		t.Fatalf("server placeholders: %s", got)
	}
	// The graphite is the placeholder's colour, the value's is ink.
	if got := js(t, tab, `const i=row(0).querySelector('[name=artist]');return getComputedStyle(i,'::placeholder').color+'|'+getComputedStyle(i).color`); got != "rgb(110, 110, 115)|rgb(29, 29, 31)" {
		t.Fatalf("placeholder colours: %s", got)
	}
	js(t, tab, `set($('#album-genre'),'Bebop');set(row(1).querySelector('[name=artist]'),'');set(row(3).querySelector('[name=genre]'),'');set(row(2).querySelector('[name=genre]'),'Rock');return ''`)
	// «No genre» from the track's menu, with the keyboard-free click.
	js(t, tab, `row(0).querySelector('.dots').click();return ''`)
	browserWait(t, tab, `document.querySelector('[data-track] .menu:popover-open') !== null`)
	js(t, tab, `row(0).querySelector('[name=nogenre]').click();row(5).querySelector('[name=nogenre]').click();return ''`)
	if got := js(t, tab, placeholders+`+'#'+$('#changes').textContent`); got != "/Miles Davis//No genre|/Miles Davis//Bebop|/Miles Davis/Rock/Bebop|/Miles Davis//Bebop|/Miles Davis//Bebop|/Miles Davis//Bebop#6 changes" {
		t.Fatalf("local placeholders: %s", got)
	}
	// Typing a genre drops «No genre».
	if got := js(t, tab, `set(row(0).querySelector('[name=genre]'),'X');return String(row(0).querySelector('[name=nogenre]').checked)`); got != "false" {
		t.Fatalf("typing kept «No genre»: %s", got)
	}
	js(t, tab, `set(row(0).querySelector('[name=genre]'),'');row(0).querySelector('[name=nogenre]').click();$('#save').click();return ''`)
	browserWait(t, tab, `document.querySelector('#changes').textContent === 'Saved'`)
	body, _ := e.album(id)
	var got []string
	for _, tr := range tracksOf(body) {
		got = append(got, strings.Join([]string{strOrNull(tr["artist"]), strOrNull(tr["genre"])}, "/"))
	}
	if body["genre"] != "Bebop" || strings.Join(got, "|") != "null/|null/null|null/Rock|null/null|null/null|null/null" {
		t.Fatalf("saved: %v %s", body["genre"], strings.Join(got, "|"))
	}
	if got := js(t, tab, placeholders+`+'#'+$('#changes').textContent+row(0).querySelector('[name=nogenre]').checked`); got != "/Miles Davis//No genre|/Miles Davis//Bebop|/Miles Davis/Rock/Bebop|/Miles Davis//Bebop|/Miles Davis//Bebop|/Miles Davis//Bebop#Savedtrue" {
		t.Fatalf("server placeholders after the save: %s", got)
	}
	noProblems(t, tab, problems)
}

func strOrNull(v any) string {
	if v == nil {
		return "null"
	}
	return v.(string)
}

// The artist field: an existing artist is chosen by its name, «Create
// artist» shows only without an exact match, and «Rename artist» renames it
// on all of its albums, trashed ones included, after saying how many.
func TestBrowserAlbumArtistPickerAndRename(t *testing.T) {
	e, root := browserEnv(t)
	id := e.seedAlbum("Miles Davis", "Kind of Blue", kindOfBlue()[:2], nil)
	other := e.seedAlbum("Miles Davis", "Milestones", kindOfBlue()[:1], nil)
	_, etag := e.album(other)
	e.must(req{method: "DELETE", path: "/api/albums/" + other.String(), ifMatch: etag}, 200)
	evans := e.seedAlbum("Bill Evans", "Portrait in Jazz", kindOfBlue()[:1], nil)
	tab, problems := albumTab(t, root, e.srv.URL+"/albums/"+id.String())

	// Rename: the dialog says how many albums change.
	js(t, tab, `$('#rename-artist').click();return ''`)
	browserWait(t, tab, `document.querySelector('#ask').open`)
	if got := js(t, tab, `return [$('#ask-text').textContent, $('#ask-input').value, $('#ask-ok').textContent].join('|')`); got != "The new name applies to 2 albums.|Miles Davis|Rename artist" {
		t.Fatalf("rename dialog: %s", got)
	}
	js(t, tab, `$('#ask-input').value='Miles Dewey Davis';$('#ask-ok').click();return ''`)
	browserWait(t, tab, `document.querySelector('#artist-name').value === 'Miles Dewey Davis' && document.querySelector('[data-track] [name=artist]').placeholder === 'Miles Dewey Davis'`)
	for _, a := range []uuid.UUID{id, other} {
		if body, _ := e.album(a); body["artist_name"] != "Miles Dewey Davis" {
			t.Fatalf("%s not renamed: %v", a, body["artist_name"])
		}
	}
	// The page took the rename's revision: the next save is no conflict.
	js(t, tab, `set($('#album-year'),'1960');$('#save').click();return ''`)
	browserWait(t, tab, `document.querySelector('#changes').textContent === 'Saved'`)
	if body, _ := e.album(id); body["year"] != float64(1960) {
		t.Fatalf("save after rename: %v", body["year"])
	}

	// An existing artist, typed in another case, is chosen: no «Create».
	js(t, tab, `set($('#artist-name'),'bill evans');return ''`)
	// «Rename artist» renames the saved artist only: hidden meanwhile.
	if got := js(t, tab, `return [$('[name=artist_id]').value, $('#create-artist').hidden, $('#artist-name').value, $('#changes').textContent, $('#rename-artist').hidden].join('|')`); got != e.artistOf(evans).String()+"|true|Bill Evans|1 change|true" {
		t.Fatalf("existing artist: %s", got)
	}
	// A new name: «Create artist», then the album is saved with it.
	js(t, tab, `set($('#artist-name'),'Cannonball Adderley');return ''`)
	if got := js(t, tab, `return [$('#create-artist').hidden, getComputedStyle($('#rename-artist')).display].join('|')`); got != "false|none" {
		t.Fatalf("new name: %s", got)
	}
	js(t, tab, `$('#create-artist').click();return ''`)
	browserWait(t, tab, `document.querySelector('#create-artist').hidden && [...document.querySelectorAll('#artists option')].some(o => o.value === 'Cannonball Adderley')`)
	js(t, tab, `$('#save').click();return ''`)
	browserWait(t, tab, `document.querySelector('#changes').textContent === 'Saved'`)
	if body, _ := e.album(id); body["artist_name"] != "Cannonball Adderley" {
		t.Fatalf("new artist: %v", body["artist_name"])
	}
	// A name that matches nothing and was not created is not saved.
	js(t, tab, `set($('#artist-name'),'Nobody');set($('#album-year'),'1961');$('#save').click();return ''`)
	browserWait(t, tab, `!document.querySelector('#head-notice').hidden`)
	if got := js(t, tab, `return $('#head-notice p').textContent+'|'+$('#head-notice details').hidden`); got != "Choose an artist from the list, or create it.|true" {
		t.Fatalf("unknown artist: %s", got)
	}
	noProblems(t, tab, problems)
}

// Two windows: the second meets the 412, keeps its edits, reloads the
// album, re-applies them on top, and saves.
func TestBrowserAlbumConflictReapply(t *testing.T) {
	e, root := browserEnv(t)
	id := e.seedAlbum("Miles Davis", "Kind of Blue", kindOfBlue(), nil)
	url := e.srv.URL + "/albums/" + id.String()
	tab1, _ := albumTab(t, root, url)
	tab2, problems := albumTab(t, root, url)
	js(t, tab2, `window.marker='kept';set(row(1).querySelector('[name=title]'),'Loser track');set($('#album-genre'),'Cool');return ''`)
	js(t, tab1, `set($('#album-title'),'Winner');$('#save').click();return ''`)
	browserWait(t, tab1, `document.querySelector('#changes').textContent === 'Saved'`)
	js(t, tab2, `$('#save').click();return ''`)
	browserWait(t, tab2, `!document.querySelector('#save-notice').hidden`)
	if got := js(t, tab2, `const n=$('#save-notice');return [n.querySelector('p').textContent, n.querySelector('button').textContent, n.querySelector('details').open, n.querySelector('summary').textContent, n.querySelector('pre').textContent.includes('precondition_failed'), $('#changes').textContent].join('|')`); got != "This album was changed in another window.|Reload and reapply my changes|false|Details|true|2 changes" {
		t.Fatalf("conflict: %s", got)
	}
	if body, _ := e.album(id); body["title"] != "Winner" || body["genre"] != "Jazz" {
		t.Fatalf("the 412 overwrote: %v", body)
	}
	js(t, tab2, `$('#save-notice button').click();return ''`)
	browserWait(t, tab2, `document.querySelector('#album-title').value === 'Winner' && document.querySelector('#save-notice').hidden`)
	if got := js(t, tab2, `return [window.marker, row(1).querySelector('[name=title]').value, $('#album-genre').value, $('#changes').textContent].join('|')`); got != "kept|Loser track|Cool|2 changes" {
		t.Fatalf("reapplied: %s", got)
	}
	js(t, tab2, `$('#save').click();return ''`)
	browserWait(t, tab2, `document.querySelector('#changes').textContent === 'Saved'`)
	body, _ := e.album(id)
	if body["title"] != "Winner" || body["genre"] != "Cool" || tracksOf(body)[1]["title"] != "Loser track" {
		t.Fatalf("after reapply: %v", body)
	}
	// A change saved by another window while this page reloads after its own
	// save: the page shows it, but keeps its save's ETag (N-260), so its next
	// save still meets the 412 instead of passing on a revision it never saw.
	api := "/api/albums/" + id.String()
	js(t, tab2, `window.sneak=true;const f=window.fetch;window.fetch=async(u,o)=>{if(window.sneak&&String(u)===location.pathname){window.sneak=false;`+
		`const a=await(await f('`+api+`')).json();await f('`+api+`',{method:'PUT',headers:{'X-Musiclib-Request':'1','If-Match':a.etag,'Content-Type':'application/json'},`+
		`body:JSON.stringify({artist_id:a.artist_id,title:a.title,year:a.year,genre:'Sneaky',compilation:a.compilation,tracks:a.tracks.map(t=>({id:t.id,disc:t.disc,no:t.no,title:t.title,artist:t.artist,genre:t.genre}))})})}return f(u,o)};`+
		`set($('#album-year'),'1970');$('#save').click();return ''`)
	browserWait(t, tab2, `document.querySelector('#changes').textContent === 'Saved' && document.querySelector('#album-genre').value === 'Sneaky'`)
	js(t, tab2, `set($('#album-title'),'Late');$('#save').click();return ''`)
	browserWait(t, tab2, `!document.querySelector('#save-notice').hidden`)
	if body, _ := e.album(id); body["title"] != "Winner" || body["genre"] != "Sneaky" || body["year"] != float64(1970) {
		t.Fatalf("a revision the page never saw was overwritten: %v", body)
	}
	noProblems(t, tab2, problems)
}

// The cover: the picker offers only images, as thumbnails; remove, drop,
// upload.
func TestBrowserAlbumCover(t *testing.T) {
	e, root := browserEnv(t)
	id := e.seedAlbum("Miles Davis", "Kind of Blue", kindOfBlue()[:2], map[string][]byte{
		"Scans/Booklet.pdf": []byte("%PDF-1.7\n"), "Scans/back.jpg": jpegImage(t, 24, 24), "notes.txt": []byte("notes"),
	})
	front := pngImage(t, 20, 20)
	_, etag := e.album(id)
	etag = e.must(upload("POST", "/api/albums/"+id.String()+"/attachments?path=Scans/front.png", etag, front), 201).body["etag"].(string)
	e.must(upload("POST", "/api/albums/"+id.String()+"/attachments?path=Scans/readme.md", etag, []byte("# hi")), 201)
	tab, problems := albumTab(t, root, e.srv.URL+"/albums/"+id.String())
	js(t, tab, `$('[popovertarget=cover-menu]').click();return ''`)
	browserWait(t, tab, `document.querySelector('#cover-menu:popover-open') !== null`)
	if got := js(t, tab, `return [...$('#cover-menu').children].filter(n=>n.checkVisibility()&&n.textContent).map(n=>n.textContent).join('|')`); got != "Upload an image|JPEG or PNG, up to 16 MB and 40 megapixels|Choose from the album’s images|Remove cover" {
		t.Fatalf("cover menu: %s", got)
	}
	js(t, tab, `$('[data-open=cover-picker]').click();return ''`)
	browserWait(t, tab, `document.querySelector('#cover-picker').open && [...document.querySelectorAll('.thumbs img')].every(i => i.complete && i.naturalWidth > 0)`)
	if got := js(t, tab, `return [$('#cover-menu').matches(':popover-open'), $$('.thumbs .file-name').map(n=>n.textContent).sort().join(',')].join('|')`); got != "false|Scans/back.jpg,Scans/front.png,cover.jpg" {
		t.Fatalf("picker: %s", got)
	}
	js(t, tab, `[...$$('.thumbs button')].find(b=>b.textContent==='Scans/front.png').click();return ''`)
	coverIs := func(want []byte) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for body, _ := e.album(id); coverOf(body)["hash"] != sha(want); body, _ = e.album(id) {
			if time.Now().After(deadline) {
				t.Fatalf("cover: %v", coverOf(body))
			}
			time.Sleep(100 * time.Millisecond)
		}
		browserWait(t, tab, `document.querySelector('#editor').dataset.etag === `+"`"+e.must(req{method: "GET", path: "/api/albums/" + id.String()}, 200).header.Get("ETag")+"`"+` && document.querySelector('img.cover')?.complete`)
	}
	coverIs(front)
	// Remove, after a confirmation that names the action.
	js(t, tab, `$('[popovertarget=cover-menu]').click();[...$$('#cover-menu button')].find(b=>b.textContent==='Remove cover').click();return ''`)
	browserWait(t, tab, `document.querySelector('#ask').open`)
	if got := js(t, tab, `return $('#ask-text').textContent+'|'+$('#ask-ok').textContent`); got != "Remove the cover?|Remove cover" {
		t.Fatalf("remove dialog: %s", got)
	}
	js(t, tab, `$('#ask-ok').click();return ''`)
	browserWait(t, tab, `document.querySelector('.cover-none') !== null`)
	// Drop an image on the cover.
	dropped := pngImage(t, 30, 30)
	js(t, tab, `const b=Uint8Array.from(atob('`+base64.StdEncoding.EncodeToString(dropped)+`'),c=>c.charCodeAt(0)),d=new DataTransfer();d.items.add(new File([b],'drop.png',{type:'image/png'}));`+
		`const box=$('#cover-drop');box.dispatchEvent(new DragEvent('dragover',{dataTransfer:d,bubbles:true,cancelable:true}));window.over=box.classList.contains('is-drop');box.dispatchEvent(new DragEvent('drop',{dataTransfer:d,bubbles:true,cancelable:true}));return ''`)
	coverIs(dropped)
	if browserEval(t, tab, `String(window.over)`) != "true" {
		t.Fatal("dragover did not mark the cover")
	}
	// Upload through the menu's file input.
	file := filepath.Join(t.TempDir(), "up.jpg")
	up := jpegImage(t, 18, 18)
	if err := os.WriteFile(file, up, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(tab, chromedp.SetUploadFiles("#cover-file", []string{file}, chromedp.NodeReady)); err != nil {
		t.Fatal(err)
	}
	coverIs(up)
	noProblems(t, tab, problems)
}

// Several .lrc files matched to the tracks by name, confirmed, assigned;
// the lyrics controls are never an edit; a track's own lyrics menu.
func TestBrowserAlbumLyrics(t *testing.T) {
	e, root := browserEnv(t)
	id := e.seedAlbum("Miles Davis", "Kind of Blue", kindOfBlue(), map[string][]byte{"Lyrics/extra.lrc": []byte("[00:01.00]extra\n")})
	tab, problems := albumTab(t, root, e.srv.URL+"/albums/"+id.String())
	// An unsaved edit survives the lyrics changes and stays the only one.
	js(t, tab, `set($('#album-title'),'Kind of Blue (Lyrics)');return ''`)
	dir := t.TempDir()
	var files []string
	for _, name := range []string{"01 So What.lrc", "2-01 Flamenco Sketches (Alternate Take).lrc", "05.lrc", "Blue in Green.lrc", "03 - Blue in Green.lrc", "unknown.lrc"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("[00:01.00]"+name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		files = append(files, p)
	}
	if err := chromedp.Run(tab, chromedp.SetUploadFiles("#lrc-files", files, chromedp.NodeReady)); err != nil {
		t.Fatal(err)
	}
	browserWait(t, tab, `document.querySelector('#lrc-dialog').open`)
	if got := js(t, tab, `return $$('#lrc-list label').map(l=>l.textContent).join('|')+'#'+$('#lrc-lost').textContent+'#'+$('#changes').textContent`); got != "01 So What.lrcSo What|2-01 Flamenco Sketches (Alternate Take).lrcFlamenco Sketches (Alternate Take)|05.lrcFlamenco Sketches#No track for Blue in Green.lrc, 03 - Blue in Green.lrc, unknown.lrc.#1 change" {
		t.Fatalf("proposals: %s", got)
	}
	// Leave one out, then confirm.
	js(t, tab, `$$('#lrc-list input')[2].click();$('#lrc-ok').click();return ''`)
	browserWait(t, tab, `document.querySelectorAll('.t-lyrics').length === 2 && !document.querySelector('#lrc-dialog').open`)
	for i, name := range map[int]string{0: "01 So What.lrc", 5: "2-01 Flamenco Sketches (Alternate Take).lrc"} {
		if got := trackOf(t, e, id, i)["lyrics_hash"]; got != sha([]byte("[00:01.00]"+name+"\n")) {
			t.Fatalf("track %d lyrics: %v", i, got)
		}
	}
	if trackOf(t, e, id, 4)["lyrics_hash"] != nil || trackOf(t, e, id, 2)["lyrics_hash"] != nil {
		t.Fatal("unchecked or unmatched file assigned")
	}
	if got := js(t, tab, `return [$('#album-title').value, $('#changes').textContent].join('|')`); got != "Kind of Blue (Lyrics)|1 change" {
		t.Fatalf("lyrics marked the page: %s", got)
	}
	// A track's menu: use a lyrics file of the album, then remove it.
	js(t, tab, `row(2).querySelector('.dots').click();[...row(2).querySelectorAll('.menu button')].find(b=>b.textContent==='Use a lyrics file of the album').click();return ''`)
	browserWait(t, tab, `document.querySelector('#lyrics-picker').open`)
	js(t, tab, `[...$$('#lyrics-picker .picks button')].find(b=>b.textContent==='Lyrics/extra.lrc').click();return ''`)
	browserWait(t, tab, `document.querySelectorAll('[data-track]')[2].querySelector('.t-lyrics') !== null`)
	js(t, tab, `row(2).querySelector('.dots').click();[...row(2).querySelectorAll('.menu button')].find(b=>b.textContent==='Remove lyrics').click();return ''`)
	browserWait(t, tab, `document.querySelector('#ask').open && document.querySelector('#ask-text').textContent === 'Remove the lyrics of “Blue in Green”?'`)
	js(t, tab, `$('#ask-ok').click();return ''`)
	browserWait(t, tab, `document.querySelectorAll('[data-track]')[2].querySelector('.t-lyrics') === null`)
	if got := js(t, tab, `return [$('#album-title').value, $('#changes').textContent].join('|')`); got != "Kind of Blue (Lyrics)|1 change" {
		t.Fatalf("lyrics menu marked the page: %s", got)
	}
	noProblems(t, tab, problems)
}

// Bulk lyrics stop at the first refused file: the ones before it stay
// assigned, and its sentence survives the page's refresh (N-264, N-282).
func TestBrowserAlbumLyricsStopAtFailure(t *testing.T) {
	e, root := browserEnv(t)
	id := e.seedAlbum("Miles Davis", "Kind of Blue", kindOfBlue(), nil)
	tab, problems := albumTab(t, root, e.srv.URL+"/albums/"+id.String())
	dir := t.TempDir()
	var files []string
	for name, body := range map[string][]byte{"01 So What.lrc": []byte("[00:01.00]ok\n"), "02 Freddie Freeloader.lrc": {0xff, 0xfe, 0x00, 0x41}} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, body, 0o600); err != nil {
			t.Fatal(err)
		}
		files = append(files, p)
	}
	if files[0] > files[1] {
		files[0], files[1] = files[1], files[0]
	}
	if err := chromedp.Run(tab, chromedp.SetUploadFiles("#lrc-files", files, chromedp.NodeReady)); err != nil {
		t.Fatal(err)
	}
	browserWait(t, tab, `document.querySelector('#lrc-dialog').open && document.querySelectorAll('#lrc-list li').length === 2`)
	js(t, tab, `$('#lrc-ok').click();return ''`)
	// One .t-lyrics comes only from the refreshed server page.
	browserWait(t, tab, `document.querySelectorAll('.t-lyrics').length === 1 && !document.querySelector('#tracks-notice').hidden`)
	if got := js(t, tab, `return [$('#tracks-notice p').textContent, $('#tracks-notice details').open, $$('[data-track]').findIndex(r=>r.querySelector('.t-lyrics'))].join('|')`); got != "Lyrics files must be UTF-8: save it as UTF-8.|false|0" {
		t.Fatalf("after the refused file: %s", got)
	}
	if trackOf(t, e, id, 0)["lyrics_hash"] == nil || trackOf(t, e, id, 1)["lyrics_hash"] != nil {
		t.Fatal("the lyrics before the refused file were not kept, or the refused one was")
	}
	noProblems(t, tab, problems)
}

// A number before the title picks among that number's tracks before the
// whole name is read as a title (N-264).
func TestBrowserAlbumLyricsNumberFirst(t *testing.T) {
	e, root := browserEnv(t)
	id := e.seedAlbum("Miles Davis", "Numbers", []fixtureTrack{{disc: 1, no: 1, title: "Blue"}, {disc: 1, no: 2, title: "1 Blue"}}, nil)
	tab, problems := albumTab(t, root, e.srv.URL+"/albums/"+id.String())
	file := filepath.Join(t.TempDir(), "1 Blue.lrc")
	if err := os.WriteFile(file, []byte("[00:01.00]x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(tab, chromedp.SetUploadFiles("#lrc-files", []string{file}, chromedp.NodeReady)); err != nil {
		t.Fatal(err)
	}
	browserWait(t, tab, `document.querySelector('#lrc-dialog').open`)
	if got := js(t, tab, `const l=$$('#lrc-list label').map(l=>l.textContent).join('|');$('#lrc-dialog').close();return l`); got != "1 Blue.lrcBlue" {
		t.Fatalf("proposal: %s", got)
	}
	noProblems(t, tab, problems)
}

// The trash from the album's ⋯ menu with the keyboard, the band and
// Restore; a track's menu with the keyboard; a deletion cancelled with Esc.
func TestBrowserAlbumTrashAndMenus(t *testing.T) {
	e, root := browserEnv(t)
	id := e.seedAlbum("Miles Davis", "Kind of Blue", kindOfBlue()[:2], nil)
	tab, problems := albumTab(t, root, e.srv.URL+"/albums/"+id.String())
	key := func(k string) { browserKey(t, tab, k) }
	js(t, tab, `$('.album-head > .dots').focus();return ''`)
	key(kb.Enter)
	browserWait(t, tab, `document.querySelector('#album-menu:popover-open') !== null`)
	key(kb.Escape)
	browserWait(t, tab, `document.querySelector('#album-menu:popover-open') === null && document.activeElement.matches('.album-head > .dots')`)
	key(kb.Enter)
	key("\t")
	if got := js(t, tab, `return document.activeElement.textContent`); got != "Update in library" {
		t.Fatalf("first menu item: %q", got)
	}
	key("\t")
	key(kb.Enter)
	browserWait(t, tab, `document.querySelector('#ask').open`)
	if got := js(t, tab, `return $('#ask-text').textContent+'|'+$('#ask-ok').textContent`); got != "Move “Kind of Blue” to the trash? You can restore it.|Move to trash" {
		t.Fatalf("trash dialog: %s", got)
	}
	key(kb.Escape)
	browserWait(t, tab, `!document.querySelector('#ask').open`)
	time.Sleep(300 * time.Millisecond)
	if body, _ := e.album(id); body["trashed"] != false {
		t.Fatal("a cancelled trash moved the album")
	}
	js(t, tab, `$('.album-head > .dots').click();[...$$('#album-menu button')].find(b=>b.textContent==='Move to trash').click();return ''`)
	browserWait(t, tab, `document.querySelector('#ask').open`)
	js(t, tab, `$('#ask-ok').click();return ''`)
	browserWait(t, tab, `document.querySelector('.band')?.textContent === 'This album is in the trash.Restore'`)
	// The back link follows the album into the Trash (N-281).
	if got := js(t, tab, `return [$('.nav a[aria-current=page]').textContent, $('#editor').dataset.trashed, $$('#album-menu button').map(b=>b.textContent).join(','), $$('.backlink').length, $('.backlink').textContent, $('.backlink').getAttribute('href')].join('|')`); got != "Trash|true|Update in library|1|Trash|/?trash=true" {
		t.Fatalf("trashed page: %s", got)
	}
	if body, _ := e.album(id); body["trashed"] != true {
		t.Fatal("not trashed")
	}
	js(t, tab, `$('.band button').click();return ''`)
	browserWait(t, tab, `document.querySelector('.band') === null && document.querySelector('#editor').dataset.trashed === 'false' && document.querySelector('.backlink').textContent === 'Library' && document.querySelector('.backlink').getAttribute('href') === '/'`)
	if body, _ := e.album(id); body["trashed"] != false {
		t.Fatal("not restored")
	}
	// A track's menu from the keyboard; Delete track asks, Esc cancels.
	js(t, tab, `row(1).querySelector('.dots').focus();return ''`)
	key(kb.Enter)
	browserWait(t, tab, `document.querySelectorAll('[data-track]')[1].querySelector('.menu:popover-open') !== null`)
	js(t, tab, `[...row(1).querySelectorAll('.menu button')].find(b=>b.textContent==='Delete track').focus();return ''`)
	key(kb.Enter)
	browserWait(t, tab, `document.querySelector('#ask').open && document.querySelector('#ask-text').textContent === 'Delete “Freddie Freeloader”? This can’t be undone.'`)
	key(kb.Escape)
	time.Sleep(300 * time.Millisecond)
	if n := len(tracksOf(func() map[string]any { b, _ := e.album(id); return b }())); n != 2 {
		t.Fatalf("a cancelled deletion removed a track: %d", n)
	}
	js(t, tab, `row(1).querySelector('.dots').click();[...row(1).querySelectorAll('.menu button')].find(b=>b.textContent==='Delete track').click();return ''`)
	browserWait(t, tab, `document.querySelector('#ask').open`)
	js(t, tab, `$('#ask-ok').click();return ''`)
	browserWait(t, tab, `document.querySelectorAll('[data-track]').length === 1`)
	noProblems(t, tab, problems)
}

// Errors: one sentence near the place, «Details» closed; a duplicate
// number names both tracks and marks their rows.
func TestBrowserAlbumErrors(t *testing.T) {
	e, root := browserEnv(t)
	id := e.seedAlbum("Miles Davis", "Kind of Blue", kindOfBlue(), nil)
	tab, problems := albumTab(t, root, e.srv.URL+"/albums/"+id.String())
	// An invalid Disc in a closed ⋯ menu: Save opens the menu on it instead
	// of silently doing nothing (N-279); nothing is sent.
	js(t, tab, `set(row(0).querySelector('[name=disc]'),'100');$('#save').click();return ''`)
	browserWait(t, tab, `document.querySelectorAll('[data-track]')[0].querySelector('.menu:popover-open') !== null && document.activeElement.name === 'disc'`)
	if body, _ := e.album(id); tracksOf(body)[0]["disc"] != float64(1) {
		t.Fatalf("an invalid disc was sent: %v", tracksOf(body)[0]["disc"])
	}
	js(t, tab, `set(row(0).querySelector('[name=disc]'),'1');$('.menu:popover-open').hidePopover();return ''`)
	browserWait(t, tab, `document.querySelector('#changes').textContent === ''`)
	js(t, tab, `set(row(1).querySelector('[name=no]'),'1');$('#save').click();return ''`)
	browserWait(t, tab, `!document.querySelector('#save-notice').hidden`)
	if got := js(t, tab, `const n=$('#save-notice');return [n.querySelector('p').textContent, n.querySelector('details').open, n.querySelector('summary').textContent, n.querySelector('pre').textContent.includes('duplicate_track_number'), $$('.is-wrong').map(r=>r.querySelector('[name=title]').value).join(','), document.activeElement===n].join('|')`); got != "So What and Freddie Freeloader both have number 1 on disc 1.|false|Details|true|So What,Freddie Freeloader|true" {
		t.Fatalf("duplicate number: %s", got)
	}
	js(t, tab, `set(row(1).querySelector('[name=no]'),'2');$('#save').click();return ''`)
	browserWait(t, tab, `document.querySelector('#save-notice').hidden && document.querySelectorAll('.is-wrong').length === 0`)
	// An extra file whose name is taken: the sentence sits in its section.
	file := filepath.Join(t.TempDir(), "cover.jpg")
	if err := os.WriteFile(file, jpegImage(t, 8, 8), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(tab, chromedp.SetUploadFiles("#extra-file", []string{file}, chromedp.NodeReady)); err != nil {
		t.Fatal(err)
	}
	browserWait(t, tab, `!document.querySelector('#extra-row').hidden && document.querySelector('#extra-path').value === 'cover.jpg'`)
	js(t, tab, `$('#extra-add').click();return ''`)
	browserWait(t, tab, `!document.querySelector('#extras-notice').hidden`)
	if got := js(t, tab, `return $('#extras-notice p').textContent+'|'+$('#extras-notice details').open`); got != "An extra file has this name: change “Save as”.|false" {
		t.Fatalf("collision: %s", got)
	}
	js(t, tab, `$('#extra-path').value='Scans/cover copy.jpg';$('#extra-add').click();return ''`)
	browserWait(t, tab, `[...document.querySelectorAll('.files .file-name')].some(a => a.textContent === 'Scans/cover copy.jpg') && document.querySelector('#extras-notice').hidden`)
	noProblems(t, tab, problems)
}

// Catalog text stays text everywhere on the page, and without JavaScript
// the album reads as it is, with its downloads.
func TestBrowserAlbumEscapingAndNoScript(t *testing.T) {
	e, root := browserEnv(t)
	hostile := `<script>" & '`
	id := e.seedAlbum(`Art <b>&"`, hostile, []fixtureTrack{{disc: 1, no: 1, title: hostile, artist: ptr(`<i>"&`)}, {disc: 1, no: 2, title: "Plain"}}, map[string][]byte{"x.txt": []byte("x")})
	e.exec(`UPDATE attachments SET rel_path = $1 WHERE album_id = $2 AND rel_path = 'x.txt'`, `Scans/<script>" &.txt`, id)
	tab, problems := albumTab(t, root, e.srv.URL+"/albums/"+id.String())
	if got := js(t, tab, `return JSON.stringify([$('#album-title').value, $('#artist-name').value, row(0).querySelector('[name=title]').value, row(0).querySelector('[name=artist]').value, $$('.files .file-name').map(a=>a.textContent).sort(), document.title, $$('main script, main b, main i').length])`); got != `["<script>\" & '","Art <b>&\"","<script>\" & '","<i>\"&",["Scans/<script>\" &.txt","cover.jpg"],"<script>\" & ' — musiclib",0]` {
		t.Fatalf("escaping: %s", got)
	}
	js(t, tab, `row(0).querySelector('.dots').click();[...row(0).querySelectorAll('.menu button')].find(b=>b.textContent==='Delete track').click();return ''`)
	browserWait(t, tab, `document.querySelector('#ask').open`)
	if got := js(t, tab, `const t=$('#ask-text').textContent;$('#ask').close();return t`); got != `Delete “<script>" & '”? This can’t be undone.` {
		t.Fatalf("dialog text: %s", got)
	}
	noProblems(t, tab, problems)

	plain, _ := browserTab(t, root)
	if err := chromedp.Run(plain, emulation.SetScriptExecutionDisabled(true)); err != nil {
		t.Fatal(err)
	}
	browserGo(t, plain, e.srv.URL+"/albums/"+id.String())
	if got := browserEval(t, plain, `JSON.stringify([document.querySelector('#album-title').readOnly, document.querySelector('#album-title').value, [...document.querySelectorAll('#metadata input:not([type=hidden])')].every(i=>i.readOnly||i.disabled||i.type==='file'), getComputedStyle(document.querySelector('#savebar')).visibility, [...document.querySelectorAll('[data-method]')].filter(b=>b.checkVisibility()).length, document.querySelectorAll('a[href$="/original"]').length, document.querySelector('[popovertarget=cover-menu]').checkVisibility()])`); got != `[true,"<script>\" & '",true,"hidden",0,2,false]` {
		t.Fatalf("no-JS page: %s", got)
	}
	// The track menu still opens, declaratively, with the downloads.
	browserEval(t, plain, `document.querySelector('[data-track] .dots').click();''`)
	if got := browserEval(t, plain, `[...document.querySelector('[data-track] .menu').children].filter(n=>n.checkVisibility()).map(n=>n.textContent).join('|')`); got != "Download original" {
		t.Fatalf("no-JS menu: %s", got)
	}
}
