package importer

import (
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"

	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
	"musiclib/internal/media"
	"musiclib/internal/names"
)

// The artists chosen automatically (§7.3). They are ordinary artists rows
// (§4.1), not special identities.
const (
	VariousArtists = "Various Artists"
	UnknownArtist  = "Unknown Artist"
)

// trackTags is one track's managed fields, as Inspect read them from the
// verified copy, with its source path relative to the candidate.
type trackTags struct {
	Path string
	Tags media.ManagedTags
}

// albumMeta is the initial metadata of §7.3, before the commit normalizes
// and validates it once more.
type albumMeta struct {
	Artist      string
	Title       string
	Year        *int
	Genre       *string
	Compilation bool
	// Tracks are in the order of the input.
	Tracks   []trackMeta
	Warnings []jobs.Warning
}

// trackMeta is one track's metadata: Artist nil inherits the album artist,
// Genre nil inherits the album genre and "" is explicitly none (§4.1).
type trackMeta struct {
	Path   string
	Disc   int
	No     int
	Title  string
	Artist *string
	Genre  *string
}

// readTrack holds one track's managed texts, normalized (§5.2); "" is an
// absent or empty field.
type readTrack struct {
	path                                     string
	title, artist, albumArtist, album, genre string
	disc                                     int
	track                                    int
	trackOK                                  bool
	year                                     int
	compilation                              bool
}

// inferMetadata is the table of §7.3, a pure function of the tracks' tags,
// the name of the candidate's directory (dirName, "" when the candidate is
// /import itself) and the user's overrides. tracks must be sorted by
// pathNaturalCompare. It does no I/O; every order it uses is explicit.
func inferMetadata(dirName string, tracks []trackTags, ov jobs.Overrides) (albumMeta, error) {
	var m albumMeta
	rs := make([]readTrack, len(tracks))
	for i, t := range tracks {
		var err error
		if rs[i], err = readTags(t); err != nil {
			return albumMeta{}, err
		}
	}
	var err error
	if m.Title, err = albumTitle(dirName, rs, ov.Title); err != nil {
		return albumMeta{}, err
	}
	auto := false
	if m.Artist, auto, err = albumArtist(rs, ov.Artist); err != nil {
		return albumMeta{}, err
	}
	m.Compilation = auto || slices.ContainsFunc(rs, func(r readTrack) bool { return r.compilation })
	m.Tracks = make([]trackMeta, len(rs))
	for i, r := range rs {
		t := trackMeta{Path: r.path, Disc: r.disc, Title: r.title}
		if t.Title == "" {
			if t.Title, err = basenameTitle(r.path); err != nil {
				return albumMeta{}, err
			}
		}
		if r.artist != "" && r.artist != m.Artist {
			t.Artist = &r.artist
		}
		m.Tracks[i] = t
	}
	if m.Warnings, err = numberTracks(rs, m.Tracks); err != nil {
		return albumMeta{}, err
	}
	var w *jobs.Warning
	m.Year, w = albumYear(rs)
	if w != nil {
		m.Warnings = append(m.Warnings, *w)
	}
	m.Genre = albumGenre(rs, m.Tracks)
	return m, nil
}

// readTags normalizes one track's managed fields. Multi-valued text is one
// string, its values joined with "; " in their original order (§7.3). A
// value that is not a valid metadata text (§5.2: a control character,
// more than 1,024 characters) is CodeInvalidTag, naming file and field.
func readTags(t trackTags) (readTrack, error) {
	r := readTrack{path: t.Path}
	for _, f := range []struct {
		name string
		dst  *string
		src  []string
	}{
		{"title", &r.title, t.Tags.Title}, {"artist", &r.artist, t.Tags.Artist},
		{"album artist", &r.albumArtist, t.Tags.AlbumArtist}, {"album", &r.album, t.Tags.Album},
		{"genre", &r.genre, t.Tags.Genre},
	} {
		v, err := names.NormalizeText(media.JoinValues(f.src))
		if err != nil {
			return readTrack{}, &Error{Code: CodeInvalidTag, Path: t.Path,
				Message: fmt.Sprintf("%q: the %s tag is not a valid text", t.Path, f.name), Err: err}
		}
		*f.dst = v
	}
	// Disc: the tag when positive, otherwise 1 (§7.3: there are no disc
	// directories in Phase 2). A number beyond the schema is an error, never
	// truncated.
	r.disc = 1
	if n, ok := media.TagNumber(t.Tags.Disc); ok && n > 0 {
		if n > catalog.MaxDisc {
			return readTrack{}, &Error{Code: catalog.CodeInvalidDisc, Path: t.Path,
				Message: fmt.Sprintf("%q: disc %s is outside 1..%d", t.Path, quoteNumber(n), catalog.MaxDisc)}
		}
		r.disc = n
	}
	if n, ok := media.TagNumber(t.Tags.Track); ok && n > 0 {
		if n > catalog.MaxTrackNumber {
			return readTrack{}, &Error{Code: catalog.CodeInvalidTrackNumber, Path: t.Path,
				Message: fmt.Sprintf("%q: track %s is outside 1..%d", t.Path, quoteNumber(n), catalog.MaxTrackNumber)}
		}
		r.track, r.trackOK = n, true
	}
	r.year = tagYear(t.Tags.Date)
	r.compilation, _ = media.TagBool(t.Tags.Compilation)
	return r, nil
}

// quoteNumber prints a parsed number; TagNumber reports anything too large
// as MaxInt32.
func quoteNumber(n int) string {
	if n >= 1<<31-1 {
		return "too large"
	}
	return strconv.Itoa(n)
}

// tagYear is the year of a DATE field, 0 when it has none that is valid: one
// value that starts with four ASCII digits not followed by a digit ("1959",
// "1959-08-17", "2001/05"), in 1..9999 (NOTES.md N-116).
func tagYear(values []string) int {
	if len(values) != 1 {
		return 0
	}
	s := strings.TrimSpace(values[0])
	if len(s) < 4 || (len(s) > 4 && isDigit(s[4])) {
		return 0
	}
	y := 0
	for i := range 4 {
		if !isDigit(s[i]) {
			return 0
		}
		y = y*10 + int(s[i]-'0')
	}
	if y < catalog.MinYear || y > catalog.MaxYear {
		return 0
	}
	return y
}

// albumTitle is §7.3's album: the explicit title; otherwise the one
// non-empty album tag; otherwise the candidate's directory name. Different
// non-empty album tags are CodeMixedAlbum (§7.2), which only an explicit
// title resolves.
func albumTitle(dirName string, rs []readTrack, override *string) (string, error) {
	if override != nil {
		return requiredText(*override, "the album title override")
	}
	albums := distinct(rs, func(r readTrack) string { return r.album })
	switch {
	case len(albums) > 1:
		return "", &Error{Code: CodeMixedAlbum,
			Message: fmt.Sprintf("the tracks have %d different album tags (%s): give the album a title to import it as one album", len(albums), quoteList(albums))}
	case len(albums) == 1:
		return albums[0], nil
	case dirName == "":
		return "", errorf(CodeAlbumTitleMissing, "no album tag and no directory name: give the album a title")
	}
	return requiredText(dirName, "the directory name")
}

// albumArtist is §7.3's album artist: the explicit artist; otherwise the one
// non-empty album artist tag (several are CodeAmbiguousAlbumArtist, which
// only an explicit artist resolves); otherwise the one non-empty artist of
// the tracks; Various Artists if they are several (auto is then true);
// Unknown Artist if there is none.
func albumArtist(rs []readTrack, override *string) (name string, auto bool, err error) {
	if override != nil {
		name, err = requiredText(*override, "the artist override")
		return name, false, err
	}
	aas := distinct(rs, func(r readTrack) string { return r.albumArtist })
	switch {
	case len(aas) > 1:
		return "", false, &Error{Code: CodeAmbiguousAlbumArtist,
			Message: fmt.Sprintf("the tracks have %d different album artists (%s): choose the artist to import the album", len(aas), quoteList(aas))}
	case len(aas) == 1:
		return aas[0], false, nil
	}
	artists := distinct(rs, func(r readTrack) string { return r.artist })
	switch len(artists) {
	case 0:
		return UnknownArtist, false, nil
	case 1:
		return artists[0], false, nil
	}
	return VariousArtists, true, nil
}

// basenameTitle is the fallback title of a track (§7.3): its file name
// without the extension, or the whole name if that leaves nothing.
func basenameTitle(p string) (string, error) {
	base := path.Base(p)
	if t, err := names.NormalizeRequiredText(strings.TrimSuffix(base, path.Ext(base))); err == nil {
		return t, nil
	}
	t, err := names.NormalizeRequiredText(base)
	if err != nil {
		return "", &Error{Code: CodeInvalidTag, Path: p,
			Message: fmt.Sprintf("%q has no title tag and its name is not a valid title", p), Err: err}
	}
	return t, nil
}

// numberTracks sets the track numbers (§7.3), disc by disc: the tags when
// every track of the disc has a positive number and they are distinct;
// otherwise the whole disc is numbered 1..n in the natural order of the
// basenames, ties by the full path, with a warning.
func numberTracks(rs []readTrack, ts []trackMeta) ([]jobs.Warning, error) {
	discs := map[int][]int{}
	for i, r := range rs {
		discs[r.disc] = append(discs[r.disc], i)
	}
	var order []int
	for d := range discs {
		order = append(order, d)
	}
	slices.Sort(order)
	var ws []jobs.Warning
	for _, d := range order {
		idx := discs[d]
		seen := map[int]bool{}
		ok := true
		for _, i := range idx {
			if !rs[i].trackOK || seen[rs[i].track] {
				ok = false
				break
			}
			seen[rs[i].track] = true
		}
		if ok {
			for _, i := range idx {
				ts[i].No = rs[i].track
			}
			continue
		}
		slices.SortFunc(idx, func(a, b int) int { return pathNaturalCompare(rs[a].path, rs[b].path) })
		if len(idx) > catalog.MaxTrackNumber {
			return nil, &Error{Code: catalog.CodeInvalidTrackNumber,
				Message: fmt.Sprintf("disc %d has %d tracks, more than the %d track numbers", d, len(idx), catalog.MaxTrackNumber)}
		}
		for n, i := range idx {
			ts[i].No = n + 1
		}
		ws = append(ws, jobs.Warning{Code: jobs.WarnTracksRenumbered,
			Message: fmt.Sprintf("disc %d: the track numbers are missing, not positive or repeated; the %d tracks are numbered by file name", d, len(idx))})
	}
	return ws, nil
}

// albumYear is §7.3's year: the most frequent valid value, a tie going to
// the smallest; different valid values give a warning.
func albumYear(rs []readTrack) (*int, *jobs.Warning) {
	count := map[int]int{}
	for _, r := range rs {
		if r.year != 0 {
			count[r.year]++
		}
	}
	if len(count) == 0 {
		return nil, nil
	}
	var years []int
	for y := range count {
		years = append(years, y)
	}
	slices.Sort(years)
	best := years[0]
	for _, y := range years {
		if count[y] > count[best] {
			best = y
		}
	}
	if len(years) == 1 {
		return &best, nil
	}
	list := make([]string, len(years))
	for i, y := range years {
		list[i] = fmt.Sprintf("%d (%d tracks)", y, count[y])
	}
	return &best, &jobs.Warning{Code: jobs.WarnYearDiscordant,
		Message: fmt.Sprintf("the tracks have different years: %s; the album year is %d", strings.Join(list, ", "), best)}
}

// albumGenre is §7.3's genre: the most frequent non-empty value, a tie going
// to the smallest in the byte order of the normalized (NFC) text (NOTES.md
// N-004). Every track whose genre differs keeps it as its own override,
// "" (explicitly none) included; with no album genre every track inherits.
func albumGenre(rs []readTrack, ts []trackMeta) *string {
	count := map[string]int{}
	for _, r := range rs {
		if r.genre != "" {
			count[r.genre]++
		}
	}
	if len(count) == 0 {
		return nil
	}
	genres := make([]string, 0, len(count))
	for g := range count {
		genres = append(genres, g)
	}
	slices.Sort(genres)
	best := genres[0]
	for _, g := range genres {
		if count[g] > count[best] {
			best = g
		}
	}
	for i, r := range rs {
		if r.genre != best {
			g := r.genre
			ts[i].Genre = &g
		}
	}
	return &best
}

// distinct returns the different non-empty values of a field, sorted by
// bytes.
func distinct(rs []readTrack, field func(readTrack) string) []string {
	var out []string
	for _, r := range rs {
		if v := field(r); v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	slices.Sort(out)
	return out
}

func requiredText(s, what string) (string, error) {
	v, err := names.NormalizeRequiredText(s)
	if err != nil {
		return "", &Error{Code: names.Code(err), Message: what + " is not a valid text", Err: err}
	}
	return v, nil
}

// quoteList quotes at most five values, for messages.
func quoteList(vs []string) string {
	q := make([]string, 0, 5)
	for i, v := range vs {
		if i == 5 {
			q = append(q, "...")
			break
		}
		q = append(q, strconv.Quote(v))
	}
	return strings.Join(q, ", ")
}
