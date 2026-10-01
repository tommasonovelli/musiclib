package media

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"slices"
	"strconv"
)

// The MP3 decode window (NOTES.md N-154).
//
// FFmpeg's mp3 demuxer does not know where the trailing tags of an MP3 are:
// its parser drops a trailing ID3v1 or APE tag only when it happens to be the
// whole remainder at the end, and an APE tag larger than 1/16 of the file
// makes it discard the Xing frame count ("invalid concatenated file"), and
// with it the gapless trimming of the LAME encoder delay and padding. The
// digest of a file with such a tag and the digest of the same file after a
// write (which removes the ID3v1 tag and shrinks the APE tag) would then
// differ, or worse, decode tag bytes. So, as for the trailing ID3v1 of a FLAC
// (N-128), the decoder reads only the bytes before the trailing tags, located
// by the rule of the tag helper's reader. Unlike the FLAC case, the bytes
// cannot come through a pipe: the demuxer applies the gapless trimming only
// to a seekable input (measured: 227 more frames through a pipe). They come
// through FFmpeg's subfile protocol over descriptor 3, which is seekable and
// ends where the tags start; the probe that supplies the declared length
// reads the same window.

// apeFooterSize is the size of an APE footer (and header).
const apeFooterSize = 32

// apeMaxTag is the largest APE tag (items and footer) the helper reads
// (kMaxApeTag in mp3.cpp, NOTES.md N-163): a larger one is refused.
const apeMaxTag = 256 << 20

// mp3AudioEnd returns how many leading bytes of f, a file the probe
// classified as MP3, the decode reads: wholeFile, or the offset where its
// trailing tags start. It reads with pread, so f's offset is not used.
//
// The rule is the helper's (readRawMp3 in native/musiclib-tags/src/mp3.cpp),
// which is TagLib 2.3.2's: an ID3v1 tag by hasTrailingID3v1, then an APE tag
// whose footer is in the 32 bytes before it (or before the end), with the
// extent its footer declares, its header included when the footer says so.
// TestMP3AudioEndIsTheHelpersRule pins the agreement file by file. A footer
// whose extent cannot be the tag's (smaller than a footer, larger than the
// file) is CodeDecode: the helper refuses such a file as corrupt too.
func mp3AudioEnd(f *os.File) (int64, error) {
	const op = "trailing tags check"
	st, err := f.Stat()
	if err != nil {
		return 0, newErr(CodeIO, op, "cannot stat the input", err)
	}
	size := st.Size()
	end := size
	n := min(size, id3v1Size+3)
	tail := make([]byte, n)
	if k, err := f.ReadAt(tail, size-n); int64(k) != n {
		return 0, newErr(CodeIO, op, "cannot read the end of the input", err)
	}
	if hasTrailingID3v1(tail, size) {
		end -= id3v1Size
	}
	if end >= apeFooterSize {
		footer := make([]byte, apeFooterSize)
		if k, err := f.ReadAt(footer, end-apeFooterSize); k != apeFooterSize {
			return 0, newErr(CodeIO, op, "cannot read the end of the input", err)
		}
		if string(footer[:8]) == "APETAGEX" {
			tagSize := int64(binary.LittleEndian.Uint32(footer[12:16]))
			complete := tagSize
			if binary.LittleEndian.Uint32(footer[20:24])&(1<<31) != 0 {
				complete += apeFooterSize
			}
			if tagSize < apeFooterSize || tagSize > apeMaxTag || complete > end {
				return 0, newErr(CodeDecode, op, "the APE tag at the end of the file declares "+
					strconv.FormatInt(tagSize, 10)+" bytes, which cannot be its size", nil)
			}
			end -= complete
		}
	}
	if end == size {
		return wholeFile, nil
	}
	return end, nil
}

// windowed rewrites a command line that reads descriptor 3 as "fd:" so that
// the tool reads only its first end bytes, through the subfile protocol over
// the same descriptor: the protocol whitelist becomes "subfile,fd", still
// without file or any other protocol, and the input
// "subfile,,start,0,end,<end>,,:fd:". The rest of the command is unchanged.
func windowed(args []string, end int64) []string {
	out := slices.Clone(args)
	for i, a := range out {
		switch {
		case a == "fd" && i > 0 && out[i-1] == "-protocol_whitelist":
			out[i] = "subfile,fd"
		case a == "fd:":
			out[i] = "subfile,,start,0,end," + strconv.FormatInt(end, 10) + ",,:fd:"
		}
	}
	return out
}

// probeWindow probes the first end bytes of f, an MP3 whose trailing tags
// start there, and returns p with the declared length of that window: the
// one the decode of the window must produce. The window must be the same
// stream as the whole file (an MP3 with the same parameters), CodeDecode
// otherwise.
func (t *Tools) probeWindow(ctx context.Context, f *os.File, p ProbeResult, end int64) (ProbeResult, error) {
	w, err := t.probe(ctx, f, windowed(probeArgs(), end))
	if err != nil {
		return ProbeResult{}, err
	}
	if w.Class != ClassAudio || w.Format != p.Format || w.Audio.Codec != p.Audio.Codec ||
		w.Audio.SampleRate != p.Audio.SampleRate || w.Audio.Channels != p.Audio.Channels || w.Audio.Layout != p.Audio.Layout {
		return ProbeResult{}, newErr(CodeDecode, "ffprobe", "the audio before the trailing tags is not the stream the file declares", nil)
	}
	p.DeclaredFrames = w.DeclaredFrames
	return p, nil
}

// The gapless length of an MP3.
//
// An encoder that writes a LAME gapless header (the Info or Xing header of
// the first frame, extended by LAME's tag) records two counts: the encoder
// delay, samples it added before the audio, and the end padding, samples it
// added after it. The pinned mp3 demuxer (libavformat/mp3dec.c) applies them
// when the header's encoder string starts with "LAME", "Lavf" or "Lavc".
// With frames, the header's frame count, and spf samples per frame (1152
// for MPEG-1, 576 for MPEG-2 and 2.5), it declares frames×spf − delay −
// padding samples, and it has the decoder drop delay + mp3DecoderDelay
// samples at the start and, at the end, the samples from frames×spf −
// padding + mp3DecoderDelay up to frames×spf. When the padding is shorter
// than mp3DecoderDelay that end range is empty, so a complete file decodes
// to frames×spf − delay − mp3DecoderDelay samples: exactly
// mp3DecoderDelay − padding fewer than declared. FFmpeg's own muxer wrote
// a padding of 0 from 1.0 to 3.1; such files are complete.
//
// The shortfall is computed from the header itself, never allowed as a
// range: a file whose padding is long (LAME's is at least 576) and that
// lost its last frame can decode fewer than mp3DecoderDelay declared
// samples short, and must still be refused.

// mp3DecoderDelay is the delay of the MP3 decoder in samples, 528 + 1, as
// the LAME gapless convention counts it and the pinned demuxer applies it:
// it drops that many samples after the encoder delay, and starts dropping
// the end padding that many samples after the padding starts.
const mp3DecoderDelay = 529

// id3v2HeaderSize is the size of an ID3v2 header (and footer).
const id3v2HeaderSize = 10

// mp3HeaderSize is the size of an MPEG audio frame header.
const mp3HeaderSize = 4

// xingOffset is where the Xing or Info header starts after the frame header,
// by [MPEG-2 or 2.5][mono]: the size of the frame's side information
// (xing_offtbl in mp3dec.c).
var xingOffset = [2][2]int{{32, 17}, {17, 9}}

// Flags of a Xing or Info header: which optional fields follow them.
const (
	xingFrames  = 1 << 0 // 4-byte frame count
	xingBytes   = 1 << 1 // 4-byte byte count
	xingTOC     = 1 << 2 // 100-byte seek table
	xingQuality = 1 << 3 // 4-byte VBR quality
)

// lameFieldsBeforeDelays is the size of the LAME tag between its 9-byte
// encoder string and the 3-byte delay and padding field: revision and VBR
// method (1), lowpass (1), peak (4), radio and audiophile ReplayGain (2 + 2),
// encoding flags and ATH (1), bitrate (1).
const lameFieldsBeforeDelays = 12

// mp3GaplessWindow is the most bytes parseMP3Gapless reads from the frame
// header on: the largest Xing offset, the identifier and flags, every
// optional field, the encoder string, the fields after it and the delay
// and padding field.
const mp3GaplessWindow = mp3HeaderSize + 32 + 4 + 4 + 4 + 4 + 100 + 4 + 9 + lameFieldsBeforeDelays + 3

// mp3Gapless is the gapless information of an MP3's first frame, as the
// pinned demuxer reads it.
type mp3Gapless struct {
	frames  int64 // the header's frame count
	spf     int64 // samples per frame
	delay   int64 // encoder delay, in samples
	padding int64 // end padding, in samples
}

// declared is the length the demuxer declares from g.
func (g mp3Gapless) declared() int64 { return g.frames*g.spf - g.delay - g.padding }

// shortfall is how many samples fewer than declared FFmpeg decodes from a
// complete file with g: max(0, mp3DecoderDelay − padding).
func (g mp3Gapless) shortfall() int64 { return max(0, mp3DecoderDelay-g.padding) }

// parseMP3Gapless reads the gapless information from b, the bytes of an MP3
// from the header of its first frame on, as mp3dec.c does
// (mp3_parse_vbr_tags, mp3_parse_info_tag). ok is false, no padding known,
// unless b starts with a valid MPEG audio Layer III header followed, at
// xingOffset, by a "Xing" or "Info" header with a frame count, then the
// encoder string "LAME", "Lavf" or "Lavc" and the delay and padding field,
// all within b.
func parseMP3Gapless(b []byte) (g mp3Gapless, ok bool) {
	if len(b) < mp3HeaderSize {
		return mp3Gapless{}, false
	}
	h := binary.BigEndian.Uint32(b)
	if !mpegAudioHeader(h) || (h>>17)&3 != 1 { // 01: Layer III
		return mp3Gapless{}, false
	}
	lsf, mono := 0, 0
	g.spf = 1152
	if h&(1<<20) == 0 || h&(1<<19) == 0 { // MPEG-2.5 or MPEG-2
		lsf, g.spf = 1, 576
	}
	if (h>>6)&3 == 3 { // channel mode: single channel
		mono = 1
	}
	pos := mp3HeaderSize + xingOffset[lsf][mono]
	if len(b) < pos+8 {
		return mp3Gapless{}, false
	}
	if id := string(b[pos : pos+4]); id != "Xing" && id != "Info" {
		return mp3Gapless{}, false
	}
	flags := binary.BigEndian.Uint32(b[pos+4:])
	pos += 8
	if flags&xingFrames == 0 || len(b) < pos+4 {
		return mp3Gapless{}, false
	}
	g.frames = int64(binary.BigEndian.Uint32(b[pos:]))
	pos += 4
	for _, f := range []struct {
		flag uint32
		size int
	}{{xingBytes, 4}, {xingTOC, 100}, {xingQuality, 4}} {
		if flags&f.flag != 0 {
			pos += f.size
		}
	}
	if len(b) < pos+9+lameFieldsBeforeDelays+3 {
		return mp3Gapless{}, false
	}
	switch string(b[pos : pos+4]) {
	case "LAME", "Lavf", "Lavc":
	default:
		return mp3Gapless{}, false
	}
	pos += 9 + lameFieldsBeforeDelays
	v := int64(b[pos])<<16 | int64(b[pos+1])<<8 | int64(b[pos+2])
	g.delay, g.padding = v>>12, v&0xfff
	return g, true
}

// mpegAudioHeader reports whether h is a valid MPEG audio frame header by
// FFmpeg's check (ff_mpa_check_header): the 11-bit sync, and no reserved
// version, layer, bitrate or sample rate.
func mpegAudioHeader(h uint32) bool {
	return h&0xffe00000 == 0xffe00000 &&
		(h>>19)&3 != 1 &&
		(h>>17)&3 != 0 &&
		(h>>12)&0xf != 0xf &&
		(h>>10)&3 != 3
}

// id3v2TagSize returns the size of the ID3v2 tag whose header is the first
// id3v2HeaderSize bytes of b, as libavformat skips the tags at the start of
// a file (ff_id3v2_match, id3v2_parse): "ID3", version and revision bytes
// other than 0xFF, a synchsafe size (every byte below 0x80) plus the header,
// plus a 10-byte footer for an ID3v2.4 tag with the footer flag (0x10). ok
// is false when b starts with no ID3v2 header.
func id3v2TagSize(b []byte) (size int64, ok bool) {
	if len(b) < id3v2HeaderSize || string(b[:3]) != "ID3" || b[3] == 0xff || b[4] == 0xff ||
		(b[6]|b[7]|b[8]|b[9])&0x80 != 0 {
		return 0, false
	}
	size = id3v2HeaderSize + (int64(b[6])<<21 | int64(b[7])<<14 | int64(b[8])<<7 | int64(b[9]))
	if b[3] == 4 && b[5]&0x10 != 0 {
		size += id3v2HeaderSize
	}
	return size, true
}

// mp3GaplessShortfall returns how many frames fewer than declared the
// decode of f, a complete MP3, yields; declared is the exact length its
// probe reports. It is the shortfall of the gapless header of the first
// frame, the one after the ID3v2 tags at the start of f, when the demuxer
// applies that header and declared is the length it derives from it; 0
// otherwise. The
// agreement with declared makes sure the header read here is the one the
// demuxer read: any other reading excuses nothing. It reads the ID3v2
// headers and mp3GaplessWindow bytes after them with pread, so f's offset is
// not used.
func mp3GaplessShortfall(f *os.File, declared int64) (int64, error) {
	if declared <= 0 {
		return 0, nil
	}
	const op = "gapless header check"
	var off int64
	for {
		b, err := readSomeAt(f, id3v2HeaderSize, off)
		if err != nil {
			return 0, newErr(CodeIO, op, "cannot read the start of the input", err)
		}
		size, ok := id3v2TagSize(b)
		if !ok {
			break
		}
		off += size
	}
	b, err := readSomeAt(f, mp3GaplessWindow, off)
	if err != nil {
		return 0, newErr(CodeIO, op, "cannot read the start of the input", err)
	}
	g, ok := parseMP3Gapless(b)
	if !ok || g.declared() != declared {
		return 0, nil
	}
	return g.shortfall(), nil
}

// readSomeAt reads up to n bytes of f at off with pread: fewer only where
// the file ends.
func readSomeAt(f *os.File, n int, off int64) ([]byte, error) {
	b := make([]byte, n)
	k, err := f.ReadAt(b, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return b[:k], nil
}
