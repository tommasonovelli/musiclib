package media

import (
	"bytes"
	"encoding/binary"
	"testing"
	"unicode/utf16"
)

// An independent codec of the MP3 tags for the tests: it builds ID3v2.2,
// ID3v2.3 and ID3v2.4 tags, APE tags and ID3v1 tags byte by byte from the
// specifications, and parses what the helper writes (ID3v2.4 without
// unsynchronisation, APE, ID3v1). It shares no code with the helper, so
// the helper's reader and writer are checked against a second reading of
// the formats. The audio comes from the pinned LAME (DESIGN.md §12.1).

// id3Frame is a frame to put in a tag: its identifier (3 characters for
// ID3v2.2), its two flag bytes (ID3v2.3/2.4) and its body as stored.
type id3Frame struct {
	id    string
	flags [2]byte
	body  []byte
}

func frame(id string, body []byte) id3Frame { return id3Frame{id: id, body: body} }

// id3Tag builds a tag: version major (2, 3 or 4), tag flags, frames, padding.
// For ID3v2.3 with the unsynchronisation flag the body is unsynchronised
// here; ID3v2.4 frame sizes are synchsafe.
func id3Tag(major byte, flags byte, padding int, frames ...id3Frame) []byte {
	var body []byte
	for _, f := range frames {
		switch major {
		case 2:
			n := len(f.body)
			body = append(body, f.id...)
			body = append(body, byte(n>>16), byte(n>>8), byte(n))
		case 3:
			body = append(body, f.id...)
			body = binary.BigEndian.AppendUint32(body, uint32(len(f.body)))
			body = append(body, f.flags[:]...)
		default:
			body = append(body, f.id...)
			body = append(body, synchsafe(len(f.body))...)
			body = append(body, f.flags[:]...)
		}
		body = append(body, f.body...)
	}
	if major <= 3 && flags&0x80 != 0 {
		body = unsynchronise(body)
	}
	body = append(body, make([]byte, padding)...)
	out := append([]byte{'I', 'D', '3', major, 0, flags}, synchsafe(len(body))...)
	return append(out, body...)
}

// unsynchronise inserts a zero byte after every 0xFF that is followed by a
// byte of 0xE0 or more, or by a zero, or ends the data (ID3v2.4 structure
// 6.1).
func unsynchronise(b []byte) []byte {
	var out []byte
	for i, c := range b {
		out = append(out, c)
		if c == 0xFF && (i+1 == len(b) || b[i+1] >= 0xE0 || b[i+1] == 0) {
			out = append(out, 0)
		}
	}
	return out
}

// Text encodings (ID3v2.4 frames 4.2).
const (
	encLatin1  = 0
	encUTF16   = 1
	encUTF16BE = 2
	encUTF8    = 3
)

// encode encodes s in enc, with a byte order mark for UTF-16 (little
// endian unless big is set).
func encode(enc byte, s string, big bool) []byte {
	switch enc {
	case encLatin1:
		var out []byte
		for _, r := range s {
			if r > 0xFF {
				panic("not Latin-1: " + s)
			}
			out = append(out, byte(r))
		}
		return out
	case encUTF16, encUTF16BE:
		var out []byte
		order := binary.AppendByteOrder(binary.LittleEndian)
		if enc == encUTF16BE || big {
			order = binary.BigEndian
		}
		if enc == encUTF16 {
			out = order.AppendUint16(out, 0xFEFF)
		}
		for _, u := range utf16.Encode([]rune(s)) {
			out = order.AppendUint16(out, u)
		}
		return out
	}
	return []byte(s)
}

func terminator(enc byte) []byte {
	if enc == encUTF16 || enc == encUTF16BE {
		return []byte{0, 0}
	}
	return []byte{0}
}

// textBody is a text frame with its strings separated by terminators.
func textBody(enc byte, values ...string) []byte {
	out := []byte{enc}
	for i, v := range values {
		if i > 0 {
			out = append(out, terminator(enc)...)
		}
		out = append(out, encode(enc, v, false)...)
	}
	return out
}

// txxxBody is a TXXX frame: description and values.
func txxxBody(enc byte, desc string, values ...string) []byte {
	out := append([]byte{enc}, encode(enc, desc, false)...)
	out = append(out, terminator(enc)...)
	return append(out, textBody(enc, values...)[1:]...)
}

// commBody is a COMM (or USLT) frame: language, description, text.
func commBody(enc byte, lang, desc, text string) []byte {
	out := append([]byte{enc}, lang...)
	out = append(out, encode(enc, desc, false)...)
	out = append(out, terminator(enc)...)
	return append(out, encode(enc, text, false)...)
}

// apicBody is an APIC frame in Latin-1.
func apicBody(mime string, typ byte, desc string, data []byte) []byte {
	out := append([]byte{encLatin1}, mime...)
	out = append(out, 0, typ)
	out = append(out, desc...)
	out = append(out, 0)
	return append(out, data...)
}

// ownerBody is a UFID or PRIV frame: a Latin-1 owner and binary data.
func ownerBody(owner string, data []byte) []byte {
	return append(append([]byte(owner), 0), data...)
}

// apeItem is an APE item as stored.
func apeItem(key string, flags uint32, value []byte) []byte {
	out := binary.LittleEndian.AppendUint32(nil, uint32(len(value)))
	out = binary.LittleEndian.AppendUint32(out, flags)
	out = append(append(out, key...), 0)
	return append(out, value...)
}

// apeTag builds an APE tag (version 2000 with header, 1000 without).
func apeTag(header bool, items ...[]byte) []byte {
	body := bytes.Join(items, nil)
	version, flags := uint32(1000), uint32(0)
	if header {
		version, flags = 2000, 1<<31
	}
	hf := func(isHeader bool) []byte {
		f := flags
		if isHeader {
			f |= 1 << 29
		}
		out := []byte("APETAGEX")
		out = binary.LittleEndian.AppendUint32(out, version)
		out = binary.LittleEndian.AppendUint32(out, uint32(len(body)+32))
		out = binary.LittleEndian.AppendUint32(out, uint32(len(items)))
		out = binary.LittleEndian.AppendUint32(out, f)
		return append(out, make([]byte, 8)...)
	}
	var out []byte
	if header {
		out = hf(true)
	}
	return append(append(out, body...), hf(false)...)
}

// id3v1 builds an ID3v1 tag; track > 0 makes it ID3v1.1 (comment of 28
// bytes). Text is Latin-1.
func id3v1(title, artist, album, year, comment string, track, genre byte) []byte {
	field := func(s string, n int) []byte {
		b := encode(encLatin1, s, false)
		return append(b, make([]byte, n-len(b))...)
	}
	out := append([]byte("TAG"), field(title, 30)...)
	out = append(out, field(artist, 30)...)
	out = append(out, field(album, 30)...)
	out = append(out, field(year, 4)...)
	if track > 0 {
		out = append(out, field(comment, 28)...)
		out = append(out, 0, track)
	} else {
		out = append(out, field(comment, 30)...)
	}
	return append(out, genre)
}

// mp3Parts is what the test parser finds in a file the helper wrote.
type mp3Parts struct {
	major   byte
	tagSize int
	frames  []id3Frame // ID3v2.4, not unsynchronised
	padding []byte
	audio   []byte
	ape     []byte   // the whole APE tag, header included
	items   [][]byte // the APE items as stored
	id3v1   []byte
}

// parseWritten parses an MP3 as the helper writes it: an optional ID3v2.4
// tag without unsynchronisation or extended header, the audio, an optional
// APE tag (with its header when flagged), an optional ID3v1 tag.
func parseWritten(t testing.TB, b []byte) mp3Parts {
	t.Helper()
	var p mp3Parts
	pos := 0
	if bytes.HasPrefix(b, []byte("ID3")) {
		p.major = b[3]
		size := unsynchsafe(t, b[6:10])
		if b[5] != 0 {
			t.Fatalf("written ID3v2 tag with flags %#x", b[5])
		}
		p.tagSize = 10 + size
		body := b[10:p.tagSize]
		for len(body) > 0 && body[0] != 0 {
			n := unsynchsafe(t, body[4:8])
			p.frames = append(p.frames, id3Frame{id: string(body[:4]), flags: [2]byte{body[8], body[9]}, body: bytes.Clone(body[10 : 10+n])})
			body = body[10+n:]
		}
		p.padding = body
		pos = p.tagSize
	}
	end := len(b)
	if end-pos >= 128 && string(b[end-128:end-125]) == "TAG" {
		p.id3v1 = b[end-128:]
		end -= 128
	}
	if end-pos >= 32 && string(b[end-32:end-24]) == "APETAGEX" {
		f := b[end-32:]
		size := int(binary.LittleEndian.Uint32(f[12:16]))
		count := int(binary.LittleEndian.Uint32(f[16:20]))
		complete := size
		if binary.LittleEndian.Uint32(f[20:24])&(1<<31) != 0 {
			complete += 32
		}
		p.ape = b[end-complete : end]
		items := b[end-size : end-32]
		for range count {
			n := int(binary.LittleEndian.Uint32(items[0:4]))
			k := bytes.IndexByte(items[8:], 0)
			l := 8 + k + 1 + n
			p.items = append(p.items, items[:l])
			items = items[l:]
		}
		end -= complete
	}
	p.audio = b[pos:end]
	return p
}

func unsynchsafe(t testing.TB, b []byte) int {
	t.Helper()
	n := 0
	for _, c := range b {
		if c&0x80 != 0 {
			t.Fatalf("size %x is not synchsafe", b)
		}
		n = n<<7 | int(c)
	}
	return n
}

// frameIDs lists the identifiers of frames, in order.
func frameIDs(frames []id3Frame) []string {
	var out []string
	for _, f := range frames {
		out = append(out, f.id)
	}
	return out
}
