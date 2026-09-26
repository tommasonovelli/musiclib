package catalog

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"musiclib/internal/names"
	"musiclib/internal/store"
)

// §10.2 GET /api/albums: "ricerca per titolo/artista, filtro
// artista/cestino, paginazione 50 max 200" (NOTES.md N-190, N-191).

// AlbumFilter selects a page of album summaries.
type AlbumFilter struct {
	// Query is the search text as the user typed it: normalized here as a
	// metadata text (§5.2), then compared with the one comparison key of
	// §5.2 (names.Key: NFC of the case folding) as a substring of the
	// album's title or of its artist's name. Empty (after the trim) is no
	// search.
	Query string
	// ArtistID keeps the albums of one artist; uuid.Nil keeps every
	// artist's.
	ArtistID uuid.UUID
	// Trashed lists the trash instead of the active albums (§10.3: "il
	// cestino è un filtro della libreria").
	Trashed bool
	// After is the cursor the page starts after; nil is the first page.
	After *AlbumCursor
	// Limit is 1..MaxPageSize.
	Limit int
}

// AlbumCursor is a position in the library's order: the artist's folder
// key, the album's folder key, the album id, compared byte-wise. Keys are
// the normalized comparison keys of §5.2, so the order is the same
// case-insensitive order whatever the database's locale, and the id
// breaks the ties (two albums with the same artist and title, one in the
// trash). A cursor keeps its place when the album it came from is
// renamed or deleted since.
type AlbumCursor struct {
	ArtistKey string
	TitleKey  string
	ID        uuid.UUID
}

// AlbumSummary is an album in a list: the desired album's own fields, its
// artist, its cover, its revision; no tracks or attachments (GetAlbum has
// them) and no processing state (GetAlbumStatus has it, §10.1).
type AlbumSummary struct {
	ID          uuid.UUID
	Revision    int64
	ArtistID    uuid.UUID
	ArtistName  string
	Title       string
	Year        *int32
	Genre       *string
	Compilation bool
	Trashed     bool
	Cover       *BlobRef
	// Processing state is read with the page, not by fetching every album separately.
	PublishedPath     *string
	PublishedRevision int64
	PublishedRenderer *string
	JobState          *string
	JobErrorCode      *string
	JobErrorMessage   *string
	// Cursor is this album's position in the order.
	Cursor AlbumCursor
}

// AlbumPage is one page of summaries and the cursor of the next page (nil
// on the last page).
type AlbumPage struct {
	Albums []AlbumSummary
	Next   *AlbumCursor
}

// searchBatch is how many rows a search reads at a time while it filters
// by text: the rows are read in the library's order, in one snapshot,
// until the page is full.
const searchBatch = 500

// ListAlbums is §10.2 GET /api/albums, one page in one REPEATABLE READ
// snapshot. The artist and trash filters and the order are SQL; the text
// search is applied here with the normalization of internal/names (§5.2),
// never with SQL's own case mapping, and is exact for any length of
// title (a folder key may be truncated, §5.2).
func (s *Service) ListAlbums(ctx context.Context, f AlbumFilter) (AlbumPage, error) {
	if f.Limit < 1 || f.Limit > MaxPageSize {
		return AlbumPage{}, errorf(CodeInvalidArgument, "a page holds 1 to %d albums, not %d", MaxPageSize, f.Limit)
	}
	q, err := names.NormalizeText(f.Query)
	if err != nil {
		return AlbumPage{}, textError("search text", err)
	}
	needle := ""
	if q != "" {
		needle = names.Key(q)
	}
	batch := f.Limit + 1
	if needle != "" {
		batch = max(batch, searchBatch)
	}
	var page AlbumPage
	err = store.InSnapshotTx(ctx, s.db, func(qs *store.Queries) error {
		page = AlbumPage{}
		p := store.ListAlbumSummariesParams{
			Trashed: f.Trashed, ByArtist: f.ArtistID != uuid.Nil, ArtistID: f.ArtistID, First: f.After == nil,
			Lim: int32(batch),
		}
		if f.After != nil {
			p.AfterArtist, p.AfterTitle, p.AfterID = f.After.ArtistKey, f.After.TitleKey, f.After.ID
		}
		var found []AlbumSummary
		for {
			rows, err := qs.ListAlbumSummaries(ctx, p)
			if err != nil {
				return dbErr("listing the albums", err)
			}
			for _, r := range rows {
				if needle != "" && !matches(needle, r.Title, r.ArtistName) {
					continue
				}
				found = append(found, albumSummary(r))
				if len(found) > f.Limit {
					break
				}
			}
			if len(found) > f.Limit || len(rows) < batch {
				break
			}
			last := rows[len(rows)-1]
			p.First, p.AfterArtist, p.AfterTitle, p.AfterID = false, last.ArtistKey, last.TitleKey, last.ID
		}
		if len(found) > f.Limit {
			found = found[:f.Limit]
			next := found[f.Limit-1].Cursor
			page.Next = &next
		}
		page.Albums = found
		return nil
	})
	return page, err
}

// matches is the search of §10.2 by title or artist: needle, already a
// comparison key, is a substring of the key of either text. The key is
// NFC of the full case folding (§5.2), so case and the NFC/NFD forms do
// not matter; accents do, since §5.2 has no accent folding (N-191).
func matches(needle, title, artist string) bool {
	return strings.Contains(names.Key(title), needle) || strings.Contains(names.Key(artist), needle)
}

func albumSummary(r store.ListAlbumSummariesRow) AlbumSummary {
	a := AlbumSummary{
		ID: r.ID, Revision: r.Revision, ArtistID: r.ArtistID, ArtistName: r.ArtistName, Title: r.Title,
		Year: r.Year, Genre: r.Genre, Compilation: r.Compilation, Trashed: r.Trashed,
		PublishedPath: r.PublishedPath, PublishedRevision: r.PublishedRevision,
		PublishedRenderer: r.PublishedRenderer, JobState: r.JobState,
		JobErrorCode: r.JobErrorCode, JobErrorMessage: r.JobErrorMessage,
		Cursor: AlbumCursor{ArtistKey: r.ArtistKey, TitleKey: r.TitleKey, ID: r.ID},
	}
	if r.CoverHash != nil {
		a.Cover = &BlobRef{Hash: *r.CoverHash, Size: deref(r.CoverSize), Format: deref(r.CoverFormat)}
	}
	return a
}
