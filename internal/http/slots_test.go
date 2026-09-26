package http

import (
	"context"
	"errors"
	"io"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"musiclib/internal/catalog"
	"musiclib/internal/failpoint"
)

// §6.1: "Le richieste HTTP di upload sono limitate a due copie
// simultanee" (NOTES.md N-199). A third upload waits, reading nothing,
// until a copy slot is free or its request ends.

// slotHooks is a failpoint hook that reports each upload reaching
// upload_waiting and upload_copying, and holds every upload at
// upload_copying until the test lets one go.
type slotHooks struct {
	waiting chan struct{}
	copying chan struct{}
	release chan struct{}
}

func newSlotHooks(e *env) *slotHooks {
	h := &slotHooks{waiting: make(chan struct{}, 16), copying: make(chan struct{}, 16), release: make(chan struct{}, 16)}
	// A failed test must not leave uploads held: closing release lets
	// every one go before the server's own cleanup waits for them.
	e.t.Cleanup(func() { close(h.release) })
	e.hooks.Set(func(p failpoint.Point) error {
		switch p.Name {
		case "upload_waiting":
			h.waiting <- struct{}{}
		case "upload_copying":
			h.copying <- struct{}{}
			<-h.release
		}
		return nil
	})
	return h
}

func wait(t *testing.T, c chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(30 * time.Second):
		t.Fatalf("no %s within 30s", what)
	}
}

// none checks that nothing arrives on c for a while: the negative half of
// the test, after the upload is known to wait at the semaphore.
func none(t *testing.T, c chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
		t.Fatalf("%s while two uploads hold the slots", what)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestUploadSlots(t *testing.T) {
	for _, kind := range []string{"attachment", "cover", "lyrics"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t)
			h := newSlotHooks(e)
			albums := make([]realAlbum, 3)
			for i := range albums {
				albums[i] = e.seedReal("Artist", "Album "+string(rune('A'+i)), catalog.FormatMP3)
			}
			requestFor := func(a realAlbum) req {
				_, tag := e.album(a.id)
				switch kind {
				case "cover":
					return upload("PUT", albumPath(a.id)+"/cover", tag, jpegImage(t, 8, 8))
				case "lyrics":
					return upload("PUT", albumPath(a.id)+"/tracks/"+a.tracks[1].String()+"/lyrics", tag, []byte("[00:01.00]x\n"))
				}
				return upload("POST", albumPath(a.id)+"/attachments?path=extra.bin", tag, randomBytes(1000))
			}
			var wg sync.WaitGroup
			statuses := make([]int, 3)
			send := func(i int, r req) {
				wg.Add(1)
				go func() {
					defer wg.Done()
					statuses[i] = e.do(r).status
				}()
			}
			// Two attachment uploads take the two slots.
			for i := range 2 {
				send(i, upload("POST", albumPath(albums[i].id)+"/attachments?path=held.bin", func() string {
					_, tag := e.album(albums[i].id)
					return tag
				}(), randomBytes(1000)))
				wait(t, h.waiting, "upload_waiting")
				wait(t, h.copying, "upload_copying")
			}
			// The third arrives at the semaphore and waits: no copy starts,
			// nothing is reserved.
			send(2, requestFor(albums[2]))
			wait(t, h.waiting, "upload_waiting of the third")
			none(t, h.copying, "a third copy started")
			if n := len(e.api.uploads); n != MaxConcurrentUploads {
				t.Fatalf("%d slots taken, want %d", n, MaxConcurrentUploads)
			}
			if e.budget.Reserved() != 0 {
				t.Fatalf("%d bytes reserved while every upload waits before its body", e.budget.Reserved())
			}
			// One slot freed: the third copies.
			h.release <- struct{}{}
			wait(t, h.copying, "upload_copying of the third")
			h.release <- struct{}{}
			h.release <- struct{}{}
			wg.Wait()
			want := nethttp.StatusCreated
			if kind != "attachment" {
				want = nethttp.StatusOK
			}
			if statuses[0] != nethttp.StatusCreated || statuses[1] != nethttp.StatusCreated || statuses[2] != want {
				t.Fatalf("statuses %v", statuses)
			}
			if n := len(e.api.uploads); n != 0 || e.budget.Reserved() != 0 {
				t.Fatalf("after the uploads: %d slots taken, %d bytes reserved", n, e.budget.Reserved())
			}
		})
	}
}

// The wait is bounded by the request's context: an upload whose request
// ends while it waits (the server closing it at shutdown, or a client
// disconnect the server has noticed) leaves without a slot and without
// reading its body: 400 upload_incomplete.
func TestUploadSlotContext(t *testing.T) {
	e := newEnv(t)
	h := &handlers{api: e.api}
	for range MaxConcurrentUploads {
		e.api.uploads <- struct{}{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequestWithContext(ctx, "POST", "/api/albums/x/attachments?path=a", strings.NewReader("never read"))
	done := make(chan *Error, 1)
	go func() {
		release, e, err := h.uploadSlot(r)
		if release != nil || err != nil {
			t.Errorf("uploadSlot took a slot: %v", err)
		}
		done <- e
	}()
	select {
	case e := <-done:
		t.Fatalf("uploadSlot returned %v with every slot taken", e)
	case <-time.After(300 * time.Millisecond):
	}
	cancel()
	select {
	case e := <-done:
		if e == nil || e.Status != nethttp.StatusBadRequest || e.Code != CodeUploadIncomplete {
			t.Fatalf("the ended request: %+v", e)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the upload still waits after its request ended")
	}
	if n := len(e.api.uploads); n != MaxConcurrentUploads {
		t.Fatalf("%d slots taken, want the %d held", n, MaxConcurrentUploads)
	}
}

// A client that goes away while its upload waits: net/http notices a
// closed HTTP/1.1 connection only once the body has been read, so the
// upload keeps its place (N-199). When its turn comes, its body cannot be
// read to its end: 400 upload_incomplete, nothing pinned, reserved or
// saved, and the slot comes back.
func TestUploadSlotClientGone(t *testing.T) {
	e := newEnv(t)
	h := newSlotHooks(e)
	var albums []realAlbum
	for _, title := range []string{"A", "B", "C"} {
		albums = append(albums, e.seedReal("Artist", title, catalog.FormatFLAC))
	}
	var wg sync.WaitGroup
	for i := range 2 {
		_, tag := e.album(albums[i].id)
		r := upload("POST", albumPath(albums[i].id)+"/attachments?path=held.bin", tag, randomBytes(1000))
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.must(r, nethttp.StatusCreated)
		}()
		wait(t, h.waiting, "upload_waiting")
		wait(t, h.copying, "upload_copying")
	}
	pinned, temps := e.blobFiles()
	// The third sends its head, and none of its body, then goes away.
	ctx, cancel := context.WithCancel(context.Background())
	_, tag := e.album(albums[2].id)
	pr, pw := io.Pipe()
	hr, err := nethttp.NewRequestWithContext(ctx, "POST", e.srv.URL+albumPath(albums[2].id)+"/attachments?path=late.bin", pr)
	if err != nil {
		t.Fatal(err)
	}
	hr.ContentLength = 1000
	hr.Host, hr.Header = testHost, nethttp.Header{"Content-Type": {UploadMediaType}, RequestHeader: {"1"}, "If-Match": {tag}}
	done := make(chan error, 1)
	go func() {
		res, err := e.srv.Client().Do(hr)
		if err == nil {
			err = errors.Join(res.Body.Close(), errors.New("answered"))
		}
		done <- err
	}()
	wait(t, h.waiting, "upload_waiting of the third")
	cancel()
	pw.CloseWithError(context.Canceled)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("the abandoned upload: %v", err)
	}
	none(t, h.copying, "a third copy started")
	// Its turn: it copies nothing, and gives the slot back.
	h.release <- struct{}{}
	wait(t, h.copying, "upload_copying of the abandoned upload")
	h.release <- struct{}{}
	h.release <- struct{}{}
	wg.Wait()
	deadline := time.Now().Add(30 * time.Second)
	for len(e.api.uploads) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after, tempsAfter := e.blobFiles(); after != pinned+2 || tempsAfter != temps || len(e.api.uploads) != 0 || e.budget.Reserved() != 0 {
		t.Fatalf("pinned %d then %d, temporaries %d then %d; slots %d; reserved %d", pinned, after, temps, tempsAfter,
			len(e.api.uploads), e.budget.Reserved())
	}
	if e.count(`SELECT count(*) FROM attachments WHERE rel_path = 'late.bin'`) != 0 {
		t.Fatal("the abandoned upload was saved")
	}
}

// Eight concurrent uploads of the three kinds: never more than two copies
// at once, and two at once while there are more waiting.
func TestUploadSlotsBound(t *testing.T) {
	e := newEnv(t)
	var active, peak atomic.Int32
	e.hooks.Set(func(p failpoint.Point) error {
		switch p.Name {
		case "upload_copying":
			n := active.Add(1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			// Stay long enough for a second upload to join.
			for deadline := time.Now().Add(200 * time.Millisecond); active.Load() < 2 && time.Now().Before(deadline); {
				time.Sleep(time.Millisecond)
			}
		case "upload_pinned":
			active.Add(-1)
		}
		return nil
	})
	var reqs []req
	for i := range 8 {
		a := e.seedReal("Artist", "Album "+string(rune('A'+i)), catalog.FormatMP3)
		_, tag := e.album(a.id)
		switch i % 3 {
		case 0:
			reqs = append(reqs, upload("POST", albumPath(a.id)+"/attachments?path=x.bin", tag, randomBytes(4000)))
		case 1:
			reqs = append(reqs, upload("PUT", albumPath(a.id)+"/cover", tag, jpegImage(t, 16, 16)))
		default:
			reqs = append(reqs, upload("PUT", albumPath(a.id)+"/tracks/"+a.tracks[0].String()+"/lyrics", tag, []byte("[00:01.00]y\n")))
		}
	}
	var wg sync.WaitGroup
	statuses := make([]int, len(reqs))
	for i, r := range reqs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i] = e.do(r).status
		}()
	}
	wg.Wait()
	for i, s := range statuses {
		if s != nethttp.StatusOK && s != nethttp.StatusCreated {
			t.Fatalf("upload %d: %d", i, s)
		}
	}
	if p := peak.Load(); p != MaxConcurrentUploads {
		t.Fatalf("at most %d copies at once, want exactly %d", p, MaxConcurrentUploads)
	}
}
