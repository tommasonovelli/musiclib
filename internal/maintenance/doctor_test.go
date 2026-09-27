package maintenance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"musiclib/internal/blobstore"
	"musiclib/internal/fsops"
	"musiclib/internal/render"
	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
	"musiclib/internal/volume"
)

func doctorFixture(t *testing.T) (*volume.Volume, *pgxpool.Pool, string, func(bool) Report) {
	t.Helper()
	db := pgtest.New(t)
	dir := t.TempDir()
	v, err := volume.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := v.Close(); err != nil {
			t.Error(err)
		}
	})
	id, err := v.Identify(t.Context(), db)
	if err != nil || id == uuid.Nil {
		t.Fatalf("identify: %v", err)
	}
	if err := v.OpenLayout(); err != nil {
		t.Fatal(err)
	}
	bs, err := blobstore.New(v.Originals(), v.Work())
	if err != nil {
		t.Fatal(err)
	}
	b, err := bs.Put(t.Context(), bytes.NewReader([]byte("original")))
	if err != nil {
		t.Fatal(err)
	}
	album, artist, build := store.NewID(), store.NewID(), store.NewID()
	path := "Artist/Album"
	content := []byte("rendered")
	sum := sha256.Sum256(content)
	rec := render.Receipt{AlbumID: album, BuildID: build, AlbumRevision: 1, RenderVersion: render.Version, Files: []render.ReceiptFile{{RelativePath: "01 - Track.flac", Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])}}}
	encoded, err := rec.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Library().MkdirAllSync(path, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{path + "/01 - Track.flac": content, path + "/" + render.ReceiptName: encoded} {
		f, err := v.Library().CreateExclusive(name, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := fsops.SyncAndClose(f); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO blobs(hash,size,created_at) VALUES($1,$2,now())`, []any{b.SHA256, b.Size}},
		{`INSERT INTO artists(id,name,folder_key,revision) VALUES($1,'Artist','artist',1)`, []any{artist}},
		{`INSERT INTO albums(id,artist_id,title,folder_key,revision,published_path,published_revision,published_renderer,published_build,published_receipt_hash) VALUES($1,$2,'Album','album',1,$3,1,$4,$5,$6)`, []any{album, artist, path, render.Version, build, render.ReceiptHash(encoded)}},
		{`INSERT INTO tracks(id,album_id,disc,no,title,blob_hash,source_path) VALUES($1,$2,1,1,'Track',$3,'Track.flac')`, []any{store.NewID(), album, b.SHA256}},
		{`INSERT INTO path_claims(path_key,path,album_id) VALUES('artist/album',$1,$2)`, []any{path, album}},
	} {
		if _, err := db.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatalf("fixture SQL: %v", err)
		}
	}
	check := func(deep bool) Report {
		t.Helper()
		r, err := Doctor(context.Background(), db, v, deep)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	return v, db, dir, check
}

func TestDoctorFindings(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		change     func(*testing.T, *volume.Volume, string)
	}{
		{"missing blob", "doctor_blob_damaged", func(t *testing.T, v *volume.Volume, _ string) {
			entries, err := v.Originals().ReadDir("")
			if err != nil {
				t.Fatal(err)
			}
			// The fixture has one original, in exactly two shards.
			first := entries[0].Name
			second, err := v.Originals().ReadDir(first)
			if err != nil {
				t.Fatal(err)
			}
			last, err := v.Originals().ReadDir(first + "/" + second[0].Name)
			if err != nil {
				t.Fatal(err)
			}
			if err := v.Originals().Remove(first + "/" + second[0].Name + "/" + last[0].Name); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing output", "doctor_output_missing", func(t *testing.T, v *volume.Volume, _ string) {
			if err := v.Library().Remove("Artist/Album/01 - Track.flac"); err != nil {
				t.Fatal(err)
			}
		}},
		{"empty extra directory", "doctor_output_extra", func(t *testing.T, v *volume.Volume, _ string) {
			if err := v.Library().MkdirAllSync("unexpected/empty", 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"extra output", "doctor_output_extra", func(t *testing.T, v *volume.Volume, _ string) {
			f, err := v.Library().CreateExclusive("Artist/Album/extra.txt", 0644)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
		}},
		{"damaged receipt", "doctor_receipt_hash", func(t *testing.T, v *volume.Volume, _ string) {
			f, err := v.Library().OpenFile("Artist/Album/.musiclib.json", os.O_WRONLY|os.O_TRUNC, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Write([]byte("{}")); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
		}},
		{"unsafe symlink", "doctor_output_type", func(t *testing.T, _ *volume.Volume, dir string) {
			if err := os.Symlink("/etc/passwd", dir+"/library/Artist/Album/unsafe"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, _, dir, check := doctorFixture(t)
			tc.change(t, v, dir)
			if r := check(false); !hasFinding(r, tc.code) {
				t.Fatalf("missing %s: %+v", tc.code, r)
			}
		})
	}
}

func TestDoctorActiveAlbumWithoutTracks(t *testing.T) {
	_, db, _, check := doctorFixture(t)
	if _, err := db.Exec(t.Context(), `DELETE FROM tracks`); err != nil {
		t.Fatal(err)
	}
	if r := check(false); !hasFinding(r, "doctor_album_no_tracks") {
		t.Fatalf("missing structural finding: %+v", r)
	}
}

func TestDoctorUnreferencedOriginal(t *testing.T) {
	v, _, _, check := doctorFixture(t)
	bs, err := blobstore.New(v.Originals(), v.Work())
	if err != nil {
		t.Fatal(err)
	}
	b, err := bs.Put(t.Context(), strings.NewReader("not in the catalog"))
	if err != nil {
		t.Fatal(err)
	}
	if r := check(true); r.HasErrors() || !hasFinding(r, "doctor_unreferenced_blob") {
		t.Fatalf("unexpected findings for %s: %+v", b.SHA256, r)
	}
}

func TestDoctorBlobSizeAndClaims(t *testing.T) {
	v, db, _, check := doctorFixture(t)
	if _, err := db.Exec(t.Context(), `UPDATE blobs SET size=size+1`); err != nil {
		t.Fatal(err)
	}
	if r := check(false); !hasFinding(r, "doctor_blob_damaged") {
		t.Fatalf("wrong blob size: %+v", r)
	}
	if _, err := db.Exec(t.Context(), `UPDATE blobs SET size=size-1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(t.Context(), `DELETE FROM path_claims`); err != nil {
		t.Fatal(err)
	}
	if r := check(false); !hasFinding(r, "doctor_claim_missing") {
		t.Fatalf("missing claim: %+v", r)
	}
	// The diagnostic must not recreate the reservation or modify the output.
	var n int
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM path_claims`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("doctor wrote claims: %d, %v", n, err)
	}
	if _, err := v.Library().Stat("Artist/Album/.musiclib.json"); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorCatalogAndJournalAreReadOnly(t *testing.T) {
	v, db, _, check := doctorFixture(t)
	// A prepared journal is an operational warning, not permission to run
	// recovery in a read-only diagnostic.
	var album uuid.UUID
	if err := db.QueryRow(t.Context(), `SELECT id FROM albums`).Scan(&album); err != nil {
		t.Fatal(err)
	}
	build := store.NewID()
	if _, err := db.Exec(t.Context(), `INSERT INTO publication(id,album_id,ticket,revision,renderer,build_id,old_path,old_build,new_path,receipt_hash) VALUES(1,$1,1,1,$2,$3,'Artist/Album',(SELECT published_build FROM albums WHERE id=$1),'Artist/Album',(SELECT published_receipt_hash FROM albums WHERE id=$1))`, album, render.Version, build); err != nil {
		t.Fatal(err)
	}
	before, err := v.Library().Stat("Artist/Album/.musiclib.json")
	if err != nil {
		t.Fatal(err)
	}
	if r := check(true); !hasFinding(r, "doctor_journal_pending") {
		t.Fatalf("missing journal warning: %+v", r)
	}
	after, err := v.Library().Stat("Artist/Album/.musiclib.json")
	if err != nil {
		t.Fatal(err)
	}
	if before.Ino != after.Ino || before.MTime != after.MTime {
		t.Fatal("doctor changed the receipt")
	}
	var count int
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM publication`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("doctor resolved journal: %d, %v", count, err)
	}
	if _, err := db.Exec(t.Context(), `DELETE FROM publication`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(t.Context(), `INSERT INTO jobs(id,kind,album_id,requested,state,queued_at,updated_at) VALUES($1,'render',$2,nextval('job_ticket'),'pending',now(),now())`, store.NewID(), album); err != nil {
		t.Fatal(err)
	}
	if r := check(false); r.HasErrors() || !hasFinding(r, "doctor_pending_work") {
		t.Fatalf("pending render: %+v", r)
	}
}

// A rename interrupted after INSTALL has moved the old path to work and
// installed the new path; neither output path is stable until recovery.
func TestDoctorRenameJournalIsPendingNotDamage(t *testing.T) {
	v, db, _, check := doctorFixture(t)
	var album, oldBuild uuid.UUID
	if err := db.QueryRow(t.Context(), `SELECT id,published_build FROM albums`).Scan(&album, &oldBuild); err != nil {
		t.Fatal(err)
	}
	build := store.NewID()
	if _, err := db.Exec(t.Context(), `INSERT INTO publication(id,album_id,ticket,revision,renderer,build_id,old_path,old_build,new_path,receipt_hash) VALUES(1,$1,1,1,$2,$3,'Artist/Album',$4,'Artist/New',(SELECT published_receipt_hash FROM albums WHERE id=$1))`, album, render.Version, build, oldBuild); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(t.Context(), `INSERT INTO path_claims(path_key,path,album_id) VALUES('artist/new','Artist/New',$1)`, album); err != nil {
		t.Fatal(err)
	}
	if err := v.Library().MkdirAllSync("Artist/New", 0755); err != nil {
		t.Fatal(err)
	}
	if err := fsops.RenameNoReplace(v.Library(), "Artist/Album/"+render.ReceiptName, v.Library(), "Artist/New/"+render.ReceiptName); err != nil {
		t.Fatal(err)
	}
	if r := check(true); r.HasErrors() || !hasFinding(r, "doctor_journal_pending") {
		t.Fatalf("rename journal treated as damage: %+v", r)
	}
	var count int
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM publication`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("journal changed: %d %v", count, err)
	}
}

func TestDoctorBlobSameSizeDamageNeedsDeep(t *testing.T) {
	v, _, dir, check := doctorFixture(t)
	first, err := v.Originals().ReadDir("")
	if err != nil {
		t.Fatal(err)
	}
	second, err := v.Originals().ReadDir(first[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	shard := first[0].Name + "/" + second[0].Name
	entries, err := v.Originals().ReadDir(shard)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate external damage: the application itself never writes pinned originals.
	if err := os.Chmod(dir+"/originals/"+shard+"/"+entries[0].Name, 0644); err != nil {
		t.Fatal(err)
	}
	f, err := v.Originals().OpenFile(shard+"/"+entries[0].Name, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("tampered")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if r := check(false); r.HasErrors() {
		t.Fatalf("normal doctor unexpectedly hashed original: %+v", r)
	}
	if r := check(true); !hasFinding(r, "doctor_blob_hash") {
		t.Fatalf("deep doctor missed damaged original: %+v", r)
	}
}

func hasFinding(r Report, code string) bool {
	for _, f := range r.Findings {
		if f.Code == code {
			return true
		}
	}
	return false
}

func TestDoctorDetectsUnchangedSizeAndMtimeDamage(t *testing.T) {
	v, _, dir, check := doctorFixture(t)
	if r := check(true); len(r.Findings) != 0 {
		t.Fatalf("clean library: %+v", r)
	}
	rel := "Artist/Album/01 - Track.flac"
	fi, err := v.Library().Stat(rel)
	if err != nil {
		t.Fatal(err)
	}
	f, err := v.Library().OpenFile(rel, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("tampered")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// An operator can restore the timestamp on a regular output file; only
	// the receipt's cryptographic hash, not stat, detects the alteration.
	if err := os.Chtimes(dir+"/library/"+rel, fi.MTime, fi.MTime); err != nil {
		t.Fatal(err)
	}
	if r := check(false); r.HasErrors() {
		t.Fatalf("normal doctor incorrectly hashed output: %+v", r)
	}
	if r := check(true); !hasFinding(r, "doctor_output_hash") {
		t.Fatalf("deep doctor missed damage: %+v", r)
	}
}
