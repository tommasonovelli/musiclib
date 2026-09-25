package media

import (
	"context"
	"encoding/binary"
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
