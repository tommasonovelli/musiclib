package media

import "strings"

// id3v1Genres is the number of genres in TagLib 2.3.2's ID3v1 list
// (ID3v1::genre: 0 to 191), the list by which the helper reads a TCON
// number.
const id3v1Genres = 192

// MP3GenreWritable reports whether an MP3 can hold genre as its genre: an
// ID3v2.4 TCON frame that reads back as exactly genre, by the helper's
// reader and by TagLib (NOTES.md N-152, owner decision N-162). A value that a
// reader would take for ID3v1 genre references cannot:
//   - a leading "(" with a ")" after it: "(13)", "(RX)", "(Rock)" (TagLib
//     consumes every leading parenthesis and keeps nothing of "(Rock)");
//   - a leading "((", the escape of a literal "(";
//   - one to three digits naming a genre of the ID3v1 list: "13", "101".
//
// The empty genre is written as no frame at all. The rule is the helper's
// (newFrames and decodeGenre in native/musiclib-tags/src/mp3.cpp), which
// refuses such a write with media_tags_invalid_request;
// TestMP3GenreWritableIsTheHelpersRule pins the two together. The API
// refuses such a genre for an album with an MP3 track, and the importer
// warns about one read from an MP3.
func MP3GenreWritable(genre string) bool {
	if strings.HasPrefix(genre, "(") && strings.Contains(genre, ")") || strings.HasPrefix(genre, "((") {
		return false
	}
	if len(genre) >= 1 && len(genre) <= 3 {
		n := 0
		for i := 0; i < len(genre); i++ {
			if genre[i] < '0' || genre[i] > '9' {
				return true
			}
			n = n*10 + int(genre[i]-'0')
		}
		return n >= id3v1Genres
	}
	return true
}
