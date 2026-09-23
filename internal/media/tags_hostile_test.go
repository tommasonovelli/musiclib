package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Hostile input (DESIGN.md §12.1 "input corrotti"): every case runs on the
// release helper and on its ASan/UBSan build, whose findings exit 86 and
// fail the test as media_tool_failed. A refusal must be typed, fast, and
// leave the file exactly as it was: the helper never writes partially
// before a check has passed.

func le32(n uint32) []byte { return binary.LittleEndian.AppendUint32(nil, n) }
func be32(n uint32) []byte { return binary.BigEndian.AppendUint32(nil, n) }

// rawBlock builds a metadata block from raw parts.
func rawBlock(typ byte, parts ...[]byte) flacBlock {
	return flacBlock{typ: typ, data: bytes.Join(parts, nil)}
}

// id3v2Tag is a minimal ID3v2.4 tag with one TIT2 frame.
func id3v2Tag() []byte {
	frame := append([]byte("TIT2\x00\x00\x00\x05\x00\x00\x03"), "abcd"...)
	return append([]byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, byte(len(frame))}, frame...)
}

// id3v1Tag is an ID3v1 tag: "TAG" and 125 bytes of fields.
func id3v1Tag() []byte {
	t := make([]byte, 128)
	copy(t, "TAGtitle")
	return t
}

// manyEntries is a Vorbis comment block with n entries "A=b".
func manyEntries(n int) flacBlock {
	entries := make([]string, n)
	for i := range entries {
		entries[i] = "A=b"
	}
	return vorbisBlock("v", entries...)
}

type hostileFile struct {
	name string
	data []byte
	// inspect is the code Inspect must fail with, "" for success; then
	// opaque is what the inspection must report.
	inspect string
	opaque  []OpaqueField
	// write is the code WriteManagedTags must fail with, "" for success.
	write string
	// noDecode: the pinned ffmpeg refuses to open the file (an invalid
	// PICTURE block is fatal in its FLAC demuxer), so it can never pass
	// AudioDigest at import; the audio frames are compared byte for byte.
	noDecode bool
}

func hostileFiles(t *testing.T, dir string) []hostileFile {
	base := baseFLAC(t, dir, "src.flac", sine3s)
	good := base.with(vorbisBlock("v", "TITLE=x")).bytes()
	// Offset of the second block header: "fLaC", STREAMINFO header and body.
	second := 4 + 4 + len(base.blocks[0].data)
	longBlock := bytes.Clone(good)
	longBlock[second+1], longBlock[second+2], longBlock[second+3] = 0xFF, 0xFF, 0xFF
	var many []flacBlock
	for range 50000 {
		many = append(many, flacBlock{typ: flacPadding})
	}
	mp3 := readFile(t, lame(t, wav16(t, dir, "src.wav", sine3s), dir, "s.mp3", "-b", "128"))
	junk := base
	junk.prefix = []byte("JUNK")
	ixml := []byte("<BWFXML/>")
	opaque := func(key, reason string, removed bool) []OpaqueField {
		return []OpaqueField{{Key: key, Reason: reason, Removed: removed}}
	}
	corrupt := func(name string, data []byte) hostileFile {
		return hostileFile{name: name, data: data, inspect: CodeTagsCorrupt, write: CodeTagsCorrupt}
	}
	mismatch := func(name string, data []byte) hostileFile {
		return hostileFile{name: name, data: data, inspect: CodeTagsFormatMismatch, write: CodeTagsFormatMismatch}
	}
	refused := func(name string, f flacFile, o []OpaqueField) hostileFile {
		return hostileFile{name: name, data: f.bytes(), opaque: o, write: CodeTagsOpaque}
	}
	removed := func(name string, f flacFile, o []OpaqueField, noDecode ...bool) hostileFile {
		return hostileFile{name: name, data: f.bytes(), opaque: o, noDecode: len(noDecode) > 0 && noDecode[0]}
	}
	return []hostileFile{
		// Corrupt structure.
		corrupt("block cut short", good[:second+4+6]),
		corrupt("block longer than the file", longBlock),
		corrupt("vorbis count 0xFFFFFFFF", base.with(rawBlock(flacVorbisComment, le32(1), []byte("v"), le32(0xFFFFFFFF))).bytes()),
		corrupt("vorbis count 50001", base.with(manyEntries(50001)).bytes()),
		corrupt("entry longer than its block", base.with(rawBlock(flacVorbisComment, le32(1), []byte("v"), le32(1), le32(0xFFFF), []byte("A=b"))).bytes()),
		corrupt("vendor longer than its block", base.with(rawBlock(flacVorbisComment, le32(0xFFFF), []byte("v"), le32(0))).bytes()),
		corrupt("comment block under 8 bytes", base.with(rawBlock(flacVorbisComment, le32(0))).bytes()),
		corrupt("first block not STREAMINFO", flacFile{blocks: []flacBlock{vorbisBlock("v", "TITLE=x"), base.blocks[0]}, audio: base.audio}.bytes()),
		corrupt("block type 127", base.with(rawBlock(127, []byte("x"))).bytes()),
		corrupt("empty APPLICATION block", base.with(flacBlock{typ: flacApplication}).bytes()),
		corrupt("50001 blocks", base.with(many...).bytes()),
		corrupt("ID3v2 size not synchsafe", append([]byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, 0x80}, good...)),
		// N-090: the extent of an ID3v2 tag must be the one TagLib strips.
		corrupt("ID3v2 size beyond the end of the file", append([]byte{'I', 'D', '3', 4, 0, 0, 0x7F, 0x7F, 0x7F, 0x7F}, good...)),
		corrupt("ID3v2 size beyond the end, with a footer", append(append([]byte{'I', 'D', '3', 4, 0, 0x10}, synchsafe(len(good)-4)...), good...)),
		corrupt("ID3v2 version 0xFF", append(id3v2Tag()[:3:3], append([]byte{0xFF}, append(id3v2Tag()[4:], good...)...)...)),
		corrupt("ID3v2 revision 0xFF", append(id3v2Tag()[:4:4], append([]byte{0xFF}, append(id3v2Tag()[5:], good...)...)...)),
		corrupt("ID3v1 inside the metadata", flacFile{blocks: base.with(rawBlock(flacApplication, []byte("TEST"), make([]byte, 100), id3v1Tag())).blocks}.bytes()),

		// Not a FLAC stream.
		mismatch("random bytes", garbage(7, 4096)),
		mismatch("an MP3", mp3),
		mismatch("junk before fLaC", junk.bytes()),
		mismatch("empty file", nil),

		// Opaque fields (N-085, N-086): a write refuses the file, unless it
		// removes the field anyway.
		refused("COMMENT with invalid UTF-8", base.with(vorbisBlock("v", "COMMENT=caf\xe9")), opaque("vorbis:COMMENT", "invalid_utf8", false)),
		removed("TITLE with invalid UTF-8", base.with(vorbisBlock("v", "TITLE=caf\xe9", "COMPOSER=c")), opaque("vorbis:TITLE", "invalid_utf8", true)),
		refused("NUL in a value", base.with(vorbisBlock("v", "COMMENT=a\x00b")), opaque("vorbis:COMMENT", "nul_byte", false)),
		removed("NUL in a managed value", base.with(vorbisBlock("v", "ALBUMSORT=a\x00b")), opaque("vorbis:ALBUMSORT", "nul_byte", true)),
		refused("entry without =", base.with(vorbisBlock("v", "TITLE=x", "NOEQUALS")), opaque("vorbis#1", "malformed_entry", false)),
		refused("entry with an empty key", base.with(vorbisBlock("v", "=value")), opaque("vorbis#0", "malformed_entry", false)),
		refused("key with ~", base.with(vorbisBlock("v", "KEY~=v")), opaque("vorbis#0", "invalid_key", false)),
		refused("key with non-ASCII", base.with(vorbisBlock("v", "KÉY=v")), opaque("vorbis#0", "invalid_key", false)),
		refused("key with NUL", base.with(vorbisBlock("v", "TITLE=x", "K\x00Y=v")), opaque("vorbis#1", "invalid_key", false)),
		refused("vendor with invalid UTF-8", base.with(vorbisBlock("\xff\xfe", "TITLE=x")), opaque("vorbis.vendor", "invalid_utf8", false)),
		refused("vendor with NUL", base.with(vorbisBlock("a\x00b", "TITLE=x")), opaque("vorbis.vendor", "nul_byte", false)),
		refused("second Vorbis block", base.with(vorbisBlock("v", "TITLE=x"), vorbisBlock("w", "COMMENT=y")),
			opaque("flac.block#"+strconv.Itoa(len(base.blocks)+1), "duplicate_block", false)),
		refused("APPLICATION riff iXML", base.with(rawBlock(flacApplication, []byte("riffiXML"), le32(uint32(len(ixml))), ixml)),
			opaque("flac.block#"+strconv.Itoa(len(base.blocks)), "foreign_metadata", false)),
		refused("APPLICATION riff bext", base.with(rawBlock(flacApplication, []byte("riffbext"), le32(4), []byte("desc"))),
			opaque("flac.block#"+strconv.Itoa(len(base.blocks)), "foreign_metadata", false)),
		refused("APPLICATION bext", base.with(rawBlock(flacApplication, []byte("bext"), garbage(1, 16))),
			opaque("flac.block#"+strconv.Itoa(len(base.blocks)), "foreign_metadata", false)),
		refused("APPLICATION iXML", base.with(rawBlock(flacApplication, []byte("iXML"), ixml)),
			opaque("flac.block#"+strconv.Itoa(len(base.blocks)), "foreign_metadata", false)),
		removed("APPLICATION riff LIST is not foreign", base.with(rawBlock(flacApplication, []byte("riffLIST"), le32(4), []byte("INFO"))), nil),
		removed("invalid PICTURE block", base.with(rawBlock(flacPicture, garbage(2, 10))),
			opaque("flac.block#"+strconv.Itoa(len(base.blocks)), "invalid_picture", true), true),
		// TagLib's check of the MIME length (pos + length + 24 > size)
		// overflows on this value and passes.
		removed("PICTURE MIME length overflow", base.with(rawBlock(flacPicture, be32(3), be32(0xFFFFFFF0), make([]byte, 40))),
			opaque("flac.block#"+strconv.Itoa(len(base.blocks)), "invalid_picture", true), true),
		removed("METADATA_BLOCK_PICTURE with invalid base64", base.with(vorbisBlock("v", "METADATA_BLOCK_PICTURE=!!!!")),
			opaque("vorbis:METADATA_BLOCK_PICTURE", "invalid_picture", true)),
		removed("COVERART with no data", base.with(vorbisBlock("v", "COVERART=")),
			opaque("vorbis:COVERART", "invalid_picture", true)),
	}
}

func TestTagsHostileFLAC(t *testing.T) {
	dir := t.TempDir()
	files := hostileFiles(t, dir)
	for name, tools := range helpers(t) {
		for _, hf := range files {
			t.Run(name+"/"+hf.name, func(t *testing.T) {
				p := writeFile(t, filepath.Join(t.TempDir(), "f.flac"), hf.data)
				in, err := tools.Inspect(t.Context(), open(t, p), FormatFLAC)
				if hf.inspect != "" {
					wantCode(t, err, hf.inspect)
				} else {
					if err != nil {
						t.Fatalf("Inspect: %v", err)
					}
					if !slices.Equal(in.Opaque, hf.opaque) {
						t.Fatalf("opaque:\n got %+v\nwant %+v", in.Opaque, hf.opaque)
					}
				}
				if hf.write == "" && hf.noDecode {
					if _, err := tools.AudioDigest(t.Context(), open(t, p)); err == nil {
						t.Fatal("the pinned ffmpeg now decodes this file: compare the digests (writeChecked)")
					}
					writeVerified(t, tools, p, fullValues, nil)
					return
				}
				if hf.write == "" {
					writeChecked(t, tools, p, fullValues, nil)
					return
				}
				wantUnchanged(t, p, func() error {
					return tools.WriteManagedTags(t.Context(), openRW(t, p), FormatFLAC, fullValues, nil)
				}, hf.write)
			})
		}
	}
}

// wantUnchanged runs a refused operation and checks its code and that the
// file at p did not change, content, size or modification time.
func wantUnchanged(t testing.TB, p string, op func() error, code string) {
	t.Helper()
	before := readFile(t, p)
	st0, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	wantCode(t, op(), code)
	st1, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, readFile(t, p)) || !st0.ModTime().Equal(st1.ModTime()) {
		t.Fatalf("the refused operation (%s) modified the file", code)
	}
}

// The field-count limit: TagLib keeps no field of a comment with more than
// 50,000, so the reader refuses such a file and the writer never produces
// one.
func TestTagsFLACFieldCountLimit(t *testing.T) {
	dir := t.TempDir()
	f := baseFLAC(t, dir, "src.flac", sine3s).with(manyEntries(50000))
	for name, tools := range helpers(t) {
		t.Run(name, func(t *testing.T) {
			p := writeFLAC(t, t.TempDir(), "limit.flac", f)
			in := inspectFile(t, tools, p)
			if len(in.Unmanaged) != 3 || in.Unmanaged[2].Key != "vorbis:A" || len(in.Unmanaged[2].Values) != 50000 {
				t.Fatalf("the 50,000 fields are not all reported: %d unmanaged keys", len(in.Unmanaged))
			}
			wantUnchanged(t, p, func() error {
				return tools.WriteManagedTags(t.Context(), openRW(t, p), FormatFLAC, TagValues{Title: "x"}, nil)
			}, CodeTagsTooLarge)
			// A write that adds nothing stays at the limit.
			writeChecked(t, tools, p, TagValues{}, nil)
		})
	}
}

// A FLAC metadata block holds at most 16 MiB - 1: a cover or a comment that
// does not fit is refused before anything is written (NOTES.md N-091),
// never dropped silently as TagLib's save would.
func TestTagsFLACTooLarge(t *testing.T) {
	dir := t.TempDir()
	base := baseFLAC(t, dir, "src.flac", sine3s)
	src := writeFLAC(t, dir, "small.flac", base.with(vorbisBlock("v", "TITLE=x")))
	// PNG files padded after IEND: DecodeConfig reads the header only.
	png := pngImage(t, 4, 4, 1)
	padded := func(name string, size int) *coverFile {
		b := append(bytes.Clone(png), make([]byte, size-len(png))...)
		return &coverFile{writeFile(t, filepath.Join(dir, name), b), FormatPNG}
	}
	cover16 := padded("16mib.png", 1<<24)
	coverBlock := padded("block.png", 1<<24-20)
	// A comment block of exactly 0xFFFFFF bytes: vendor length and "v",
	// count, entry length and "COMMENT=": 21 bytes.
	full := writeFLAC(t, dir, "full.flac", base.with(vorbisBlock("v", "COMMENT="+strings.Repeat("a", 0xFFFFFF-21))))

	for name, tools := range helpers(t) {
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				name  string
				src   string
				v     TagValues
				cover *coverFile
			}{
				{"cover of 16 MiB", src, fullValues, cover16},
				{"cover that does not fit a block", src, fullValues, coverBlock},
				{"comment at the limit plus a title", full, TagValues{Title: "x"}, nil},
			} {
				t.Run(tc.name, func(t *testing.T) {
					p := writeFile(t, filepath.Join(t.TempDir(), "f.flac"), readFile(t, tc.src))
					wantUnchanged(t, p, func() error {
						return tools.WriteManagedTags(t.Context(), openRW(t, p), FormatFLAC, tc.v, tc.cover.cover(t))
					}, CodeTagsTooLarge)
				})
			}
		})
	}
}

// rawTags runs the helper with an arbitrary command line and request,
// bypassing the adapter's validation, and maps the failure as the adapter
// does.
func rawTags(ctx context.Context, tools *Tools, args []string, stdin []byte, files ...*os.File) error {
	var out bytes.Buffer
	_, err := tools.run.Run(ctx, Command{
		Path: tools.tags, Args: args, Files: files, Stdin: stdin,
		Stdout: &out, StdoutLimit: tagsOutputLimit, Timeout: InspectTimeout,
	})
	if err != nil {
		return tagsFailure(err, "musiclib-tags")
	}
	return nil
}

// Requests the adapter never sends: the helper validates every one of them
// itself (§8.1: "Input JSON limitato") and writes nothing.
func TestTagsRawRequests(t *testing.T) {
	dir := t.TempDir()
	src := writeFLAC(t, dir, "src.flac", baseFLAC(t, dir, "base.flac", sine3s).with(vorbisBlock("v", "TITLE=x"),
		flacPictureData{typ: 3, mime: "image/png", data: pngImage(t, 2, 2, 1)}.block()))
	const write = `{"format":"flac","tags":{"title":"T","artist":null,"album_artist":null,"album":null,"track":1,` +
		`"track_total":null,"disc":null,"disc_total":null,"date":null,"genre":null,"compilation":false},"cover":null}`
	const cover = `{"mime":"image/png","width":1,"height":1,"depth":24,"colors":0}`
	w := func(old, new string) string {
		if !strings.Contains(write, old) {
			panic("no " + old)
		}
		return strings.Replace(write, old, new, 1)
	}
	inspect := `{"format":"flac"}`
	padTo := func(s string, n int) string { return s + strings.Repeat(" \n\t\r", n)[:n-len(s)] }
	nested := `{"format":"flac","pictures":` + strings.Repeat("[", 8) + "0" + strings.Repeat("]", 8) + "}"

	type rawCase struct {
		name string
		args []string
		body string
		code string // "" = success
	}
	cases := []rawCase{
		{"256 KiB exactly, whitespace padded", []string{"inspect"}, padTo(inspect, 256<<10), ""},
		{"over 256 KiB", []string{"inspect"}, padTo(inspect, 256<<10+1), CodeTagsInvalidRequest},
		{"malformed JSON", []string{"inspect"}, `{"format":`, CodeTagsInvalidRequest},
		{"not an object", []string{"inspect"}, `["flac"]`, CodeTagsInvalidRequest},
		{"trailing data", []string{"inspect"}, inspect + ` x`, CodeTagsInvalidRequest},
		{"two values", []string{"inspect"}, inspect + inspect, CodeTagsInvalidRequest},
		{"unknown key", []string{"inspect"}, `{"format":"flac","extra":1}`, CodeTagsInvalidRequest},
		{"missing key", []string{"inspect"}, `{}`, CodeTagsInvalidRequest},
		{"duplicate key", []string{"inspect"}, `{"format":"flac","format":"flac"}`, CodeTagsInvalidRequest},
		{"format of the wrong type", []string{"inspect"}, `{"format":1}`, CodeTagsInvalidRequest},
		{"invalid UTF-8", []string{"inspect"}, "{\"format\":\"fl\xffac\"}", CodeTagsInvalidRequest},
		{"lone surrogate", []string{"inspect"}, `{"format":"\ud800"}`, CodeTagsInvalidRequest},
		{"unknown format", []string{"inspect"}, `{"format":"ogg"}`, CodeTagsUnsupported},
		{"unknown format of 100 KB", []string{"inspect"}, `{"format":"` + strings.Repeat("é", 50000) + `"}`, CodeTagsUnsupported},
		{"empty format", []string{"inspect"}, `{"format":""}`, CodeTagsUnsupported},
		{"unknown operation", []string{"frobnicate"}, inspect, CodeTagsInvalidRequest},
		{"no operation", nil, "", CodeTagsInvalidRequest},
		{"extra argument", []string{"inspect", "x"}, inspect, CodeTagsInvalidRequest},
		{"path as operation", []string{"/etc/passwd"}, inspect, CodeTagsInvalidRequest},

		{"extract: nesting too deep", []string{"extract-images"}, nested, CodeTagsInvalidRequest},
		{"extract: pictures not an array", []string{"extract-images"}, `{"format":"flac","pictures":"0"}`, CodeTagsInvalidRequest},
		{"extract: no pictures", []string{"extract-images"}, `{"format":"flac","pictures":[]}`, CodeTagsInvalidRequest},
		{"extract: a picture twice", []string{"extract-images"}, `{"format":"flac","pictures":[0,0]}`, CodeTagsInvalidRequest},
		{"extract: negative index", []string{"extract-images"}, `{"format":"flac","pictures":[-1]}`, CodeTagsInvalidRequest},
		{"extract: index as a float", []string{"extract-images"}, `{"format":"flac","pictures":[0.0]}`, CodeTagsInvalidRequest},

		{"write: valid", []string{"write-managed-tags"}, write, ""},
		{"write: track 2^31-1", []string{"write-managed-tags"}, w(`"track":1`, `"track":2147483647`), ""},
		{"write: missing tag", []string{"write-managed-tags"}, w(`"genre":null,`, ``), CodeTagsInvalidRequest},
		{"write: unknown tag", []string{"write-managed-tags"}, w(`"genre":null,`, `"genre":null,"lyrics":null,`), CodeTagsInvalidRequest},
		{"write: missing cover key", []string{"write-managed-tags"}, `{"format":"flac","tags":{}}`, CodeTagsInvalidRequest},
		{"write: tags not an object", []string{"write-managed-tags"}, `{"format":"flac","tags":[],"cover":null}`, CodeTagsInvalidRequest},
		{"write: title of the wrong type", []string{"write-managed-tags"}, w(`"title":"T"`, `"title":1`), CodeTagsInvalidRequest},
		{"write: empty title", []string{"write-managed-tags"}, w(`"title":"T"`, `"title":""`), CodeTagsInvalidRequest},
		{"write: NUL in the title", []string{"write-managed-tags"}, w(`"title":"T"`, `"title":"a\u0000b"`), CodeTagsInvalidRequest},
		{"write: lone surrogate in the title", []string{"write-managed-tags"}, w(`"title":"T"`, `"title":"\udc00"`), CodeTagsInvalidRequest},
		{"write: track as a string", []string{"write-managed-tags"}, w(`"track":1`, `"track":"1"`), CodeTagsInvalidRequest},
		{"write: track 0", []string{"write-managed-tags"}, w(`"track":1`, `"track":0`), CodeTagsInvalidRequest},
		{"write: track -1", []string{"write-managed-tags"}, w(`"track":1`, `"track":-1`), CodeTagsInvalidRequest},
		{"write: track 2^31", []string{"write-managed-tags"}, w(`"track":1`, `"track":2147483648`), CodeTagsInvalidRequest},
		{"write: track beyond int64", []string{"write-managed-tags"}, w(`"track":1`, `"track":99999999999999999999`), CodeTagsInvalidRequest},
		{"write: track as a float", []string{"write-managed-tags"}, w(`"track":1`, `"track":1.5`), CodeTagsInvalidRequest},
		{"write: track with an exponent", []string{"write-managed-tags"}, w(`"track":1`, `"track":1e3`), CodeTagsInvalidRequest},
		{"write: track with a leading zero", []string{"write-managed-tags"}, w(`"track":1`, `"track":01`), CodeTagsInvalidRequest},
		{"write: compilation null", []string{"write-managed-tags"}, w(`"compilation":false`, `"compilation":null`), CodeTagsInvalidRequest},
		{"write: compilation as 1", []string{"write-managed-tags"}, w(`"compilation":false`, `"compilation":1`), CodeTagsInvalidRequest},
		{"write: cover missing a key", []string{"write-managed-tags"}, w(`"cover":null`, `"cover":{"mime":"image/png","width":1,"height":1,"depth":24}`), CodeTagsInvalidRequest},
		{"write: cover with an unknown key", []string{"write-managed-tags"}, w(`"cover":null`, `"cover":{"mime":"image/png","width":1,"height":1,"depth":24,"colors":0,"desc":""}`), CodeTagsInvalidRequest},
		{"write: cover width as a string", []string{"write-managed-tags"}, w(`"cover":null`, strings.Replace(`"cover":`+cover, `"width":1`, `"width":"1"`, 1)), CodeTagsInvalidRequest},
		{"write: cover width 2^32", []string{"write-managed-tags"}, w(`"cover":null`, strings.Replace(`"cover":`+cover, `"width":1`, `"width":4294967296`, 1)), CodeTagsInvalidRequest},
		{"write: empty MIME", []string{"write-managed-tags"}, w(`"cover":null`, strings.Replace(`"cover":`+cover, `image/png`, ``, 1)), CodeTagsInvalidRequest},
		{"write: non-ASCII MIME", []string{"write-managed-tags"}, w(`"cover":null`, strings.Replace(`"cover":`+cover, `image/png`, `imagé/png`, 1)), CodeTagsInvalidRequest},
		{"write: MIME with a newline", []string{"write-managed-tags"}, w(`"cover":null`, strings.Replace(`"cover":`+cover, `image/png`, `image\npng`, 1)), CodeTagsInvalidRequest},
		{"write: MIME of 256 bytes", []string{"write-managed-tags"}, w(`"cover":null`, strings.Replace(`"cover":`+cover, `image/png`, strings.Repeat("x", 256), 1)), CodeTagsInvalidRequest},
		{"write: cover without descriptor 4", []string{"write-managed-tags"}, w(`"cover":null`, `"cover":`+cover), CodeTagsBadDescriptor},
		{"write: unknown format", []string{"write-managed-tags"}, w(`"format":"flac"`, `"format":"wav"`), CodeTagsUnsupported},
	}
	for name, tools := range helpers(t) {
		for _, tc := range cases {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				p := writeFile(t, filepath.Join(t.TempDir(), "f.flac"), readFile(t, src))
				var stdin []byte
				if tc.body != "" {
					stdin = []byte(tc.body)
				}
				run := func() error { return rawTags(t.Context(), tools, tc.args, stdin, openRW(t, p)) }
				if tc.code == "" {
					if err := run(); err != nil {
						t.Fatal(err)
					}
					return
				}
				e := wantCode(t, run(), tc.code)
				if len(e.Msg) > 4200 {
					t.Fatalf("the failure message is %d bytes long", len(e.Msg))
				}
				wantUnchanged(t, p, run, tc.code)
			})
		}
	}
}

// Descriptors that are not what the operation needs are refused without
// blocking, reading or writing (bad_descriptor).
func TestTagsDescriptors(t *testing.T) {
	dir := t.TempDir()
	flac := baseFLAC(t, dir, "base.flac", sine3s).with(vorbisBlock("v", "TITLE=x"),
		flacPictureData{typ: 3, mime: "image/png", data: pngImage(t, 2, 2, 1)}.block())
	fifo := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	osOpen := func(t *testing.T, path string, flag int) *os.File {
		f, err := os.OpenFile(path, flag, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := f.Close(); err != nil {
				t.Error(err)
			}
		})
		return f
	}
	const write = `{"format":"flac","tags":{"title":"T","artist":null,"album_artist":null,"album":null,"track":null,` +
		`"track_total":null,"disc":null,"disc_total":null,"date":null,"genre":null,"compilation":false},"cover":%s}`
	writeWith := func(cover string) string { return strings.Replace(write, "%s", cover, 1) }
	const cover = `{"mime":"image/png","width":2,"height":2,"depth":32,"colors":0}`
	inspect := `{"format":"flac"}`
	extract := `{"format":"flac","pictures":[0]}`

	for name, tools := range helpers(t) {
		t.Run(name, func(t *testing.T) {
			p := writeFLAC(t, t.TempDir(), "f.flac", flac)
			img := writeFile(t, filepath.Join(t.TempDir(), "c.png"), pngImage(t, 2, 2, 1))
			for _, tc := range []struct {
				name  string
				op    string
				body  string
				files func(t *testing.T) []*os.File
			}{
				// O_RDWR opens a FIFO without blocking; the helper must not
				// read it.
				{"FIFO", "inspect", inspect, func(t *testing.T) []*os.File { return []*os.File{osOpen(t, fifo, os.O_RDWR)} }},
				{"directory", "inspect", inspect, func(t *testing.T) []*os.File { return []*os.File{osOpen(t, dir, os.O_RDONLY)} }},
				{"/dev/null", "inspect", inspect, func(t *testing.T) []*os.File { return []*os.File{osOpen(t, "/dev/null", os.O_RDWR)} }},
				{"no descriptor 3", "inspect", inspect, func(*testing.T) []*os.File { return nil }},
				{"write-only for inspect", "inspect", inspect, func(t *testing.T) []*os.File { return []*os.File{osOpen(t, p, os.O_WRONLY)} }},
				{"read-only for a write", "write-managed-tags", writeWith("null"), func(t *testing.T) []*os.File { return []*os.File{open(t, p)} }},
				{"append mode for a write", "write-managed-tags", writeWith("null"), func(t *testing.T) []*os.File {
					return []*os.File{osOpen(t, p, os.O_RDWR|os.O_APPEND)}
				}},
				{"FIFO for a write", "write-managed-tags", writeWith("null"), func(t *testing.T) []*os.File { return []*os.File{osOpen(t, fifo, os.O_RDWR)} }},
				{"cover without descriptor 4", "write-managed-tags", writeWith(cover), func(t *testing.T) []*os.File { return []*os.File{openRW(t, p)} }},
				{"cover is a directory", "write-managed-tags", writeWith(cover), func(t *testing.T) []*os.File {
					return []*os.File{openRW(t, p), osOpen(t, dir, os.O_RDONLY)}
				}},
				{"cover is write-only", "write-managed-tags", writeWith(cover), func(t *testing.T) []*os.File {
					return []*os.File{openRW(t, p), osOpen(t, img, os.O_WRONLY)}
				}},
				{"cover is a FIFO", "write-managed-tags", writeWith(cover), func(t *testing.T) []*os.File {
					return []*os.File{openRW(t, p), osOpen(t, fifo, os.O_RDWR)}
				}},
				{"extract destination not empty", "extract-images", extract, func(t *testing.T) []*os.File {
					d := createEmpty(t, t.TempDir(), "x")
					if _, err := d.WriteString("x"); err != nil {
						t.Fatal(err)
					}
					return []*os.File{open(t, p), d}
				}},
				{"extract destination read-only", "extract-images", extract, func(t *testing.T) []*os.File {
					return []*os.File{open(t, p), open(t, writeFile(t, filepath.Join(t.TempDir(), "x"), nil))}
				}},
				{"extract destination in append mode", "extract-images", extract, func(t *testing.T) []*os.File {
					return []*os.File{open(t, p), osOpen(t, writeFile(t, filepath.Join(t.TempDir(), "x"), nil), os.O_WRONLY|os.O_APPEND)}
				}},
				{"extract destination missing", "extract-images", extract, func(t *testing.T) []*os.File { return []*os.File{open(t, p)} }},
				{"extract destination is a FIFO", "extract-images", extract, func(t *testing.T) []*os.File {
					return []*os.File{open(t, p), osOpen(t, fifo, os.O_RDWR)}
				}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					// A helper that blocked on the FIFO would be cancelled
					// here and fail with media_canceled.
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					files := tc.files(t)
					wantUnchanged(t, p, func() error {
						return rawTags(ctx, tools, []string{tc.op}, []byte(tc.body), files...)
					}, CodeTagsBadDescriptor)
				})
			}
		})
	}
}

// extract-images checks every index before writing anything: a missing
// picture leaves every destination empty.
func TestTagsExtractMissingPicture(t *testing.T) {
	dir := t.TempDir()
	p := writeFLAC(t, dir, "f.flac", baseFLAC(t, dir, "base.flac", sine3s).with(vorbisBlock("v", "TITLE=x"),
		flacPictureData{typ: 3, mime: "image/png", data: pngImage(t, 2, 2, 1)}.block(),
		flacPictureData{typ: 4, mime: "image/png", data: pngImage(t, 2, 2, 2)}.block()))
	for name, tools := range helpers(t) {
		t.Run(name, func(t *testing.T) {
			out := t.TempDir()
			targets := []ImageTarget{{1, createEmpty(t, out, "a")}, {0, createEmpty(t, out, "b")}, {2, createEmpty(t, out, "c")}}
			_, err := tools.ExtractImages(t.Context(), open(t, p), FormatFLAC, targets)
			wantCode(t, err, CodeTagsNoPicture)
			for _, n := range []string{"a", "b", "c"} {
				if b := readFile(t, filepath.Join(out, n)); len(b) != 0 {
					t.Fatalf("destination %s has %d bytes", n, len(b))
				}
			}
		})
	}
	// The adapter's own checks.
	tools := newTools(t)
	for _, targets := range [][]ImageTarget{
		nil,
		{{-1, createEmpty(t, t.TempDir(), "x")}},
		{{0, nil}},
		make([]ImageTarget, maxExtract+1),
	} {
		_, err := tools.ExtractImages(t.Context(), open(t, p), FormatFLAC, targets)
		wantCode(t, err, CodeInvalidArgument)
	}
}

// A write that fails half way (here: the file size limit, EFBIG) is
// media_tags_io, never success. The staging copy may be partially
// rewritten; the caller discards it (§9.1, NOTES.md N-084).
func TestTagsWriteIOError(t *testing.T) {
	dir := t.TempDir()
	f := baseFLAC(t, dir, "base.flac", sine3s).with(vorbisBlock("v", "TITLE=x"))
	img := append(pngImage(t, 4, 4, 1), make([]byte, 64<<10)...)
	for name, helper := range map[string]string{"release": TagsPath, "asan": tagsASanPath} {
		t.Run(name, func(t *testing.T) {
			p := writeFLAC(t, t.TempDir(), "f.flac", f)
			cover := &Cover{File: open(t, writeFile(t, filepath.Join(t.TempDir(), "c.png"), img)), Format: FormatPNG}
			req, files, err := writeRequest(openRW(t, p), FormatFLAC, fullValues, cover)
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			// dash counts ulimit -f in blocks of 512 bytes: the file may not
			// grow past its current size rounded up.
			limit := len(f.bytes())/512 + 1
			script := "trap '' XFSZ; ulimit -f " + strconv.Itoa(limit) + "; exec " + helper + " write-managed-tags"
			_, err = NewRunner(1).Run(t.Context(), Command{
				Path: "/bin/sh", Args: []string{"-c", script}, Files: files, Stdin: body, Timeout: InspectTimeout,
			})
			e := wantCode(t, tagsFailure(err, "musiclib-tags write-managed-tags"), CodeTagsIO)
			if !strings.Contains(e.Msg, "File too large") {
				t.Fatalf("message %q does not name EFBIG", e.Msg)
			}
		})
	}
}

// §8.5: the helper runs under the 30 s limit of inspect/tag/images; a
// helper that hangs is killed (media_timeout), and a cancelled context kills
// it too (media_canceled). The fake helper answers `version` like the real
// one and sleeps on every operation.
func TestTagsTimeoutAndCancel(t *testing.T) {
	dir := t.TempDir()
	fake := writeFile(t, filepath.Join(dir, "musiclib-tags"), []byte(`#!/bin/sh
if [ "$1" = version ]; then
  echo '{"helper":"`+PinnedTagsVersion+`","taglib":"`+PinnedTagLibVersion+`"}'
  exit 0
fi
exec /bin/sleep 60
`))
	if err := os.Chmod(fake, 0o755); err != nil {
		t.Fatal(err)
	}
	flac := writeFLAC(t, dir, "f.flac", baseFLAC(t, dir, "base.flac", sine3s))
	tools, err := NewTools(t.Context(), NewRunner(1), FFmpegPath, FFprobePath, fake)
	if err != nil {
		t.Fatal(err)
	}

	tools.tagsTimeout = 300 * time.Millisecond
	start := time.Now()
	_, err = tools.Inspect(t.Context(), open(t, flac), FormatFLAC)
	wantCode(t, err, CodeTimeout)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the timeout took %v", d)
	}
	err = tools.WriteManagedTags(t.Context(), openRW(t, flac), FormatFLAC, fullValues, nil)
	wantCode(t, err, CodeTimeout)

	tools.tagsTimeout = 0 // the real 30 s
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(200*time.Millisecond, cancel)
	start = time.Now()
	_, err = tools.ExtractImages(ctx, open(t, flac), FormatFLAC, []ImageTarget{{0, createEmpty(t, t.TempDir(), "x")}})
	wantCode(t, err, CodeCanceled)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the cancellation took %v", d)
	}
}

// The adapter's decoding of the helper's answers: a typed failure needs
// exit status 3 and exactly one {"code","message"} object with a known
// code; anything else keeps the Runner's code, whatever stderr says (§8.5:
// stderr never declares success).
func TestTagsFailureMapping(t *testing.T) {
	dir := t.TempDir()
	flac := writeFLAC(t, dir, "f.flac", baseFLAC(t, dir, "base.flac", sine3s))
	for _, tc := range []struct {
		name   string
		script string
		code   string
	}{
		{"typed failure", `echo '{"code":"corrupt","message":"m"}' >&2; exit 3`, CodeTagsCorrupt},
		{"typed failure, other status", `echo '{"code":"corrupt","message":"m"}' >&2; exit 1`, CodeToolFailed},
		{"sanitizer status", `echo '{"code":"corrupt","message":"m"}' >&2; exit 86`, CodeToolFailed},
		{"unknown code", `echo '{"code":"oops","message":"m"}' >&2; exit 3`, CodeToolFailed},
		{"extra field", `echo '{"code":"corrupt","message":"m","x":1}' >&2; exit 3`, CodeToolFailed},
		{"two objects", `echo '{"code":"corrupt","message":"m"}{"code":"io","message":"m"}' >&2; exit 3`, CodeToolFailed},
		{"not JSON", `echo 'Segmentation fault' >&2; exit 3`, CodeToolFailed},
		{"killed", `kill -9 $$`, CodeToolFailed},
		{"success with an unknown field", `cat >/dev/null; echo '{"format":"flac","managed":{},"x":1}'`, CodeOutputInvalid},
		// Only the UTF-8 check catches this one: encoding/json would decode
		// the title as "caf�".
		{"success with invalid UTF-8", `cat >/dev/null; printf '{"format":"flac","managed":{"title":["caf\377"]}}'`, CodeOutputInvalid},
		{"success with trailing data", `cat >/dev/null; echo '{"format":"flac"} {}'`, CodeOutputInvalid},
		{"success of another format", `cat >/dev/null; echo '{"format":"mp3"}'`, CodeOutputInvalid},
		{"success, pictures out of order", `cat >/dev/null; echo '{"format":"flac","pictures":[{"index":1}]}'`, CodeOutputInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := writeFile(t, filepath.Join(t.TempDir(), "musiclib-tags"), []byte("#!/bin/sh\n"+tc.script+"\n"))
			if err := os.Chmod(fake, 0o755); err != nil {
				t.Fatal(err)
			}
			tools := &Tools{run: NewRunner(1), tags: fake}
			_, err := tools.Inspect(t.Context(), open(t, flac), FormatFLAC)
			e := wantCode(t, err, tc.code)
			if tc.code == CodeTagsCorrupt && e.Msg != "m" {
				t.Fatalf("message %q", e.Msg)
			}
		})
	}
}
