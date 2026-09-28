package publish

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/failpoint"
	"musiclib/internal/render"
)

// durations reads the duration_ms of each track blob of an album, by track
// number: -1 for NULL.
func (m *mediaEnv) durations(album uuid.UUID) (hashes []string, ms []int64) {
	m.t.Helper()
	rows, err := m.db.Query(context.Background(), `SELECT t.blob_hash, coalesce(b.duration_ms, -1)
		FROM tracks t JOIN blobs b ON b.hash = t.blob_hash WHERE t.album_id = $1 ORDER BY t.disc, t.no`, album)
	if err != nil {
		m.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var h string
		var d int64
		if err := rows.Scan(&h, &d); err != nil {
			m.t.Fatal(err)
		}
		hashes, ms = append(hashes, h), append(ms, d)
	}
	if err := rows.Err(); err != nil {
		m.t.Fatal(err)
	}
	return hashes, ms
}

// rerender forces the album's render (§10.2 POST /render) and runs it.
func (m *mediaEnv) rerender(album uuid.UUID) {
	m.t.Helper()
	if _, _, err := m.cat.RequestRender(context.Background(), album, m.album(album).Revision); err != nil {
		m.t.Fatal(err)
	}
	m.runPool(1)
	if _, ok := m.renderJob(album); ok {
		m.t.Fatalf("the render of %s did not complete", album)
	}
}

// audioTree is the published album without its receipt, whose build id
// changes with every build (§9.2).
func (m *mediaEnv) audioTree(album uuid.UUID) map[string]string {
	m.t.Helper()
	tree := m.tree("library/" + *m.album(album).PublishedPath)
	delete(tree, render.ReceiptName)
	return tree
}

// N-301: a render records a track's duration while it is unknown, from the
// verified copy of the original, and never overwrites a known one; a probe
// that fails leaves it unknown and the render publishes as usual; a
// duration already known costs no probe; the output is the same whatever
// the durations.
func TestRenderRecordsUnknownDurations(t *testing.T) {
	m := newMediaEnv(t)
	writeAlbum(t, m.src, "k", "Miles Davis", "Kind of Blue", 3)
	m.importAll()
	m.runPool(1)
	id := m.albumID("Kind of Blue")
	hashes, imported := m.durations(id)
	for i, d := range imported {
		if d != 400 {
			t.Fatalf("track %d: the import recorded %d ms, want 400", i+1, d)
		}
	}
	tree := m.audioTree(id)
	revision := m.album(id).Revision

	// Every probe of a duration goes through the failpoint: count them.
	var probes atomic.Int32
	var failing atomic.Bool
	m.bfp.Set(func(p failpoint.Point) error {
		if p.Name != "duration" {
			return nil
		}
		probes.Add(1)
		if failing.Load() {
			return errors.New("injected: the probe failed")
		}
		return nil
	})

	// Unknown, known with another value, known as imported.
	m.exec(`UPDATE blobs SET duration_ms = NULL WHERE hash = $1`, hashes[0])
	m.exec(`UPDATE blobs SET duration_ms = 123 WHERE hash = $1`, hashes[1])
	m.rerender(id)
	if _, got := m.durations(id); fmt.Sprint(got) != "[400 123 400]" {
		t.Fatalf("after the render: %v, want [400 123 400] (filled, kept, kept)", got)
	}
	if probes.Load() != 1 {
		t.Fatalf("%d probes, want 1: only the unknown duration is read", probes.Load())
	}
	// The duration is no output: no new revision, the same files.
	if m.album(id).Revision != revision || m.album(id).PublishedRevision != revision {
		t.Fatal("a recorded duration changed the album's revision")
	}
	m.wantTree("library/"+*m.album(id).PublishedPath, withReceipt(m, id, tree))

	// A failing probe: the render publishes, the duration stays unknown.
	m.exec(`UPDATE blobs SET duration_ms = NULL WHERE hash = $1`, hashes[2])
	failing.Store(true)
	probes.Store(0)
	m.rerender(id)
	if _, got := m.durations(id); fmt.Sprint(got) != "[400 123 -1]" {
		t.Fatalf("after a failed probe: %v, want [400 123 -1]", got)
	}
	if probes.Load() != 1 || m.count(`SELECT count(*) FROM jobs WHERE state = 'failed'`) != 0 {
		t.Fatalf("%d probes, or a failed job", probes.Load())
	}
	m.wantTree("library/"+*m.album(id).PublishedPath, withReceipt(m, id, tree))

	// The next render reads it; after that, none is read.
	failing.Store(false)
	probes.Store(0)
	m.rerender(id)
	m.rerender(id)
	if _, got := m.durations(id); fmt.Sprint(got) != "[400 123 400]" || probes.Load() != 1 {
		t.Fatalf("durations %v after %d probes, want [400 123 400] after 1", got, probes.Load())
	}
	m.wantWorkClean()
}

// withReceipt is tree plus the album's current receipt, as m.tree reads it.
func withReceipt(m *mediaEnv, album uuid.UUID, tree map[string]string) map[string]string {
	out := map[string]string{render.ReceiptName: m.tree("library/" + *m.album(album).PublishedPath)[render.ReceiptName]}
	for k, v := range tree {
		out[k] = v
	}
	return out
}
