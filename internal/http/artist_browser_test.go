package http

import (
	"testing"
)

// The owner's scenario (round 22, NOTES.md N-297, N-298) in a real
// Chromium: a name typed and staged with «Create artist», then the old
// artist chosen again, creates nothing; a new artist saved exists; the
// album moved back leaves it without albums, and it is gone. A save that
// fails, for a conflict or an invalid value, leaves no artist behind. At no
// point does GET /api/artists list an artist without albums.
func TestBrowserNoOrphanArtist(t *testing.T) {
	e, root := browserEnv(t)
	id := e.seedAlbum("Miles Davis", "Kind of Blue", kindOfBlue()[:2], nil)
	e.seedAlbum("Miles Davis", "Milestones", kindOfBlue()[:1], nil)
	e.seedAlbum("Bill Evans", "Portrait in Jazz", kindOfBlue()[:1], nil)
	named := func(name string) int {
		return e.count(`SELECT count(*) FROM artists WHERE name = $1`, name)
	}
	tab, problems := albumTab(t, root, e.srv.URL+"/albums/"+id.String())

	// Type a new name, stage it, then pick the old artist back: nothing to
	// save, nothing created.
	js(t, tab, `set($('#artist-name'),'Cannonball Adderley');$('#create-artist').click();return ''`)
	if got := js(t, tab, `return [$('#new-artist').hidden, $('#changes').textContent].join('|')`); got != "false|1 change" {
		t.Fatalf("staged: %s", got)
	}
	js(t, tab, `set($('#artist-name'),'Miles Davis');return ''`)
	if got := js(t, tab, `return [$('[name=new_artist]').value, $('#new-artist').hidden, $('#create-artist').hidden, $('#changes').textContent, $('#save').disabled].join('|')`); got != "|true|true||true" {
		t.Fatalf("the old artist back: %s", got)
	}
	// Another edit saves with the old artist only.
	js(t, tab, `set($('#album-year'),'1960');$('#save').click();return ''`)
	browserWait(t, tab, `document.querySelector('#changes').textContent === 'Saved'`)
	if body, _ := e.album(id); body["year"] != float64(1960) || body["artist_name"] != "Miles Davis" || named("Cannonball Adderley") != 0 {
		t.Fatalf("saved with the old artist: %v %v, Cannonball %d", body["year"], body["artist_name"], named("Cannonball Adderley"))
	}
	e.wantNoOrphan()

	// Staged, then another name typed: the staging goes, «Create» is back,
	// and the album's own artist is what the form holds again, so there is
	// nothing to save (N-307).
	js(t, tab, `set($('#artist-name'),'Cannonball Adderley');$('#create-artist').click();set($('#artist-name'),'Cannonball');return ''`)
	if got := js(t, tab, `return [$('[name=new_artist]').value, $('#create-artist').hidden, $('#new-artist').hidden, $('[name=artist_id]').value, $('#save').disabled].join('|')`); got != "|false|true|"+e.artistOf(id).String()+"|true" {
		t.Fatalf("another name after staging: %s", got)
	}

	// A save refused by the server leaves no artist: a conflict (another
	// window saved meanwhile), then two tracks with one number.
	js(t, tab, `set($('#artist-name'),'Wynton Kelly');$('#create-artist').click();return ''`)
	a, tag := e.album(id)
	b := putBody(a)
	b["genre"] = "Cool"
	e.must(req{method: "PUT", path: "/api/albums/" + id.String(), body: b, ifMatch: tag}, ok)
	js(t, tab, `$('#save').click();return ''`)
	browserWait(t, tab, `!document.querySelector('#save-notice').hidden`)
	if named("Wynton Kelly") != 0 {
		t.Fatal("a save refused with 412 created its new artist")
	}
	js(t, tab, `$('#save-notice button').click();return ''`)
	browserWait(t, tab, `document.querySelector('#album-genre').value === 'Cool' && document.querySelector('#save-notice').hidden`)
	if got := js(t, tab, `return [$('#artist-name').value, $('[name=new_artist]').value, $('#new-artist').hidden].join('|')`); got != "Wynton Kelly|Wynton Kelly|false" {
		t.Fatalf("the staged artist after «Reload and reapply»: %s", got)
	}
	js(t, tab, `set(row(1).querySelector('[name=no]'),'1');$('#save').click();return ''`)
	browserWait(t, tab, `!document.querySelector('#save-notice').hidden && document.querySelector('.is-wrong')`)
	if named("Wynton Kelly") != 0 {
		t.Fatal("a save refused with 422 created its new artist")
	}
	e.wantNoOrphan()

	// Type a new name, save: it exists, with the album.
	js(t, tab, `set(row(1).querySelector('[name=no]'),'2');set($('#artist-name'),'Cannonball Adderley');$('#create-artist').click();$('#save').click();return ''`)
	browserWait(t, tab, `document.querySelector('#changes').textContent === 'Saved'`)
	if body, _ := e.album(id); body["artist_name"] != "Cannonball Adderley" || named("Cannonball Adderley") != 1 || named("Wynton Kelly") != 0 {
		t.Fatalf("saved with the new artist: %v", body["artist_name"])
	}
	if got := js(t, tab, `return [[...$$('#artists option')].map(o=>o.value).join(','), $('#new-artist').hidden, $('#rename-artist').hidden].join('|')`); got != "Bill Evans,Cannonball Adderley,Miles Davis|true|false" {
		t.Fatalf("after the save: %s", got)
	}
	e.wantNoOrphan()

	// Move the album back: Cannonball Adderley has no album left, and goes.
	js(t, tab, `set($('#artist-name'),'miles davis');$('#save').click();return ''`)
	browserWait(t, tab, `document.querySelector('#changes').textContent === 'Saved' && document.querySelectorAll('#artists option').length === 2`)
	if body, _ := e.album(id); body["artist_name"] != "Miles Davis" || named("Cannonball Adderley") != 0 {
		t.Fatalf("moved back: %v, Cannonball %d", body["artist_name"], named("Cannonball Adderley"))
	}
	e.wantNoOrphan()

	// A list that is out of date: another window created the staged name
	// with its own album. The save says so, and the page takes the fresh
	// list, where the name is now the existing artist, chosen.
	js(t, tab, `set($('#artist-name'),'Paul Chambers');$('#create-artist').click();return ''`)
	pc := e.seedAlbum("Paul Chambers", "Bass on Top", kindOfBlue()[:1], nil)
	js(t, tab, `$('#save').click();return ''`)
	browserWait(t, tab, `!document.querySelector('#save-notice').hidden && document.querySelector('[name=new_artist]').value === ''`)
	if got := js(t, tab, `return [$('#save-notice p').textContent, $('[name=artist_id]').value, $('#create-artist').hidden, $('#changes').textContent].join('|')`); got != "This artist already exists: choose it from the list.|"+e.artistOf(pc).String()+"|true|1 change" {
		t.Fatalf("a stale list: %s", got)
	}
	js(t, tab, `$('#save').click();return ''`)
	browserWait(t, tab, `document.querySelector('#changes').textContent === 'Saved'`)
	if e.artistOf(id) != e.artistOf(pc) || named("Paul Chambers") != 1 {
		t.Fatal("the album did not join the existing artist")
	}
	e.wantNoOrphan()
	noProblems(t, tab, problems)
}
