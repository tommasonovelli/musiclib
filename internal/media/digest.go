package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"os"
	"strconv"
)

// Digest is the identity of the decoded audio of a track (§8.4): the same
// sequence of samples and parameters, as decoded by the pinned ffmpeg. It is
// transient and never stored (§8.4); it is comparable only with a digest
// computed by the same binary on the same machine (NOTES.md N-079).
type Digest struct {
	SampleRate int
	Channels   int
	// Layout is AudioInfo.Layout: FFmpeg's description, or
	// "unknown:<channels>".
	Layout string
	// Frames is the number of PCM frames: bytes / (8 × channels).
	Frames int64
	// PCMSHA256 is the lowercase hex SHA-256 of the f64le PCM stream.
	PCMSHA256 string
}

// pcmFrameBytes is the size of one sample of pcm_f64le.
const pcmFrameBytes = 8

// demuxerOf is the ffmpeg demuxer of each supported format, forced for the
// decode so that the file is read exactly as it was classified.
var demuxerOf = map[string]string{
	FormatFLAC:    "flac",
	FormatMP3:     "mp3",
	FormatM4AAAC:  "mov",
	FormatM4AALAC: "mov",
}

// decodeArgs is the ffmpeg command line of §8.4 for the file on descriptor
// 3: the first audio stream decoded to raw f64le on stdout, with no change
// of rate, channels or layout and no filter; one thread for the decoder, the
// encoder and the filter graph (§6.1); any decode error fatal.
//
// Three options go beyond the letter of §8.4 and serve it (NOTES.md N-074):
//   - "-err_detect crccheck+explode" instead of "explode": a bare "explode"
//     replaces FFmpeg's default flags, which turns off the FLAC frame
//     CRC-16, and most single-bit corruptions of a FLAC then decode with
//     exit status 0 to different samples;
//   - "-reinit_filter 0", without which ffmpeg silently resamples or remixes
//     when the parameters change mid-stream;
//   - the "fd"-only protocol whitelist of the probe.
func decodeArgs(demuxer string) []string {
	return []string{
		"-hide_banner",
		"-nostdin",
		"-nostats",
		"-loglevel", "error",
		"-xerror",
		// Input: demuxer and decoder options.
		"-threads", "1",
		"-err_detect", "crccheck+explode",
		"-reinit_filter", "0",
		"-protocol_whitelist", "fd",
		"-fd", "3",
		"-f", demuxer,
		"-i", "fd:",
		// Output.
		"-map", "0:a:0",
		"-vn", "-sn", "-dn",
		"-c:a", "pcm_f64le",
		"-threads", "1",
		"-filter_threads", "1",
		"-f", "f64le",
		"pipe:1",
	}
}

// AudioDigest verifies that f is supported audio and decodes it completely
// (§7.6, §8.4):
//
//  1. Probe: exactly one supported audio stream; sample rate, channels and
//     normalized layout come from it.
//  2. Decode with ffmpeg to pcm_f64le, streaming the output into SHA-256
//     and a byte count; nothing is held in memory. For a FLAC with a
//     trailing ID3v1 tag, the decoder reads only the bytes before the tag
//     (flacAudioEnd, NOTES.md N-128); for an MP3 with trailing ID3v1 or
//     APE tags, only the bytes before them, probed again for the declared
//     length (mp3AudioEnd, N-154); every other file is read whole.
//  3. The byte count must be a positive multiple of 8 × channels, and, when
//     the container declares an exact length, equal to it, less, for an
//     MP3, the frames its gapless header leaves FFmpeg no padding to trim
//     (mp3GaplessShortfall): max(0, mp3DecoderDelay − end padding) when
//     that header is one FFmpeg applies, 0 otherwise. The rule is exact,
//     never a tolerance. The digest is of the samples actually decoded.
//
// A file that is not supported audio fails with CodeNotSupported; one that
// does not decode completely, with CodeDecode. Timeouts (DecodeTimeout) and
// cancellation have their own codes. f follows the rules of Probe.
//
// The render's comparison of §9.1 step 6 calls AudioDigest on the input
// copy before the tag write and on the written copy after it, and compares
// the two digests, nothing else. The ID3v1 tag that the write strips
// (N-090) is the one step 2 leaves out of the input's decode, so both
// digests describe the same stream, and they are equal when the write kept
// the audio. The caller needs no knowledge of ID3 tags.
func (t *Tools) AudioDigest(ctx context.Context, f *os.File) (Digest, error) {
	p, err := t.Probe(ctx, f)
	if err != nil {
		return Digest{}, err
	}
	if p.Class != ClassAudio {
		msg := string(p.Class)
		if p.Reason != "" {
			msg += ": " + p.Reason + ": " + p.Detail
		}
		return Digest{}, newErr(CodeNotSupported, "audio digest", msg, nil)
	}
	limit, shortfall := int64(wholeFile), int64(0)
	switch p.Format {
	case FormatFLAC:
		if limit, err = flacAudioEnd(f); err != nil {
			return Digest{}, err
		}
	case FormatMP3:
		if limit, err = mp3AudioEnd(f); err != nil {
			return Digest{}, err
		}
		if limit != wholeFile {
			if p, err = t.probeWindow(ctx, f, p, limit); err != nil {
				return Digest{}, err
			}
		}
		if shortfall, err = mp3GaplessShortfall(f, p.DeclaredFrames); err != nil {
			return Digest{}, err
		}
	}
	return t.decode(ctx, f, p, limit, shortfall)
}

// wholeFile is the limit of decode that reads the whole file.
const wholeFile = -1

// id3v1Size is the size of an ID3v1 tag, which is always the last bytes of
// the file.
const id3v1Size = 128

// flacAudioEnd returns how many leading bytes of f, a file the probe
// classified as FLAC, the decode reads: wholeFile, or the offset where a
// trailing ID3v1 tag starts (NOTES.md N-128). It reads with pread, so f's
// offset is not used.
//
// The tag is found by the rule of the tag helper's reader (readRawFlac in
// native/musiclib-tags/src/flac.cpp), which is TagLib 2.3.2's
// Utils::findID3v1, the rule by which a render strips the tag (N-090):
// hasTrailingID3v1. TestTrailingID3v1IsTheHelpersRule pins the agreement
// with the helper file by file, so the bytes left out are exactly the bytes
// a render removes.
func flacAudioEnd(f *os.File) (int64, error) {
	// No tool runs here: the failures are reads of the input itself, before
	// the decode.
	const op = "trailing ID3v1 check"
	st, err := f.Stat()
	if err != nil {
		return 0, newErr(CodeIO, op, "cannot stat the input", err)
	}
	size := st.Size()
	n := min(size, id3v1Size+3)
	tail := make([]byte, n)
	if k, err := f.ReadAt(tail, size-n); int64(k) != n {
		return 0, newErr(CodeIO, op, "cannot read the end of the input", err)
	}
	if hasTrailingID3v1(tail, size) {
		return size - id3v1Size, nil
	}
	return wholeFile, nil
}

// hasTrailingID3v1 is TagLib's Utils::findID3v1 on a file of size bytes
// whose last min(size, 131) bytes are tail: "TAG" 128 bytes before the end,
// unless it is the "TAG" of an "APETAGEX" (an APEv2 tag's identifier) that
// starts 131 bytes before the end; for a file of 128 to 130 bytes, "TAG" at
// the start.
func hasTrailingID3v1(tail []byte, size int64) bool {
	switch {
	case size >= id3v1Size+3:
		return string(tail[3:6]) == "TAG" && string(tail[0:8]) != "APETAGEX"
	case size >= id3v1Size:
		return string(tail[len(tail)-id3v1Size:][:3]) == "TAG"
	}
	return false
}

// decode is steps 2 and 3 of AudioDigest, for a file already classified as
// ClassAudio. limit is wholeFile, or the number of leading bytes of f the
// decoder gets. shortfall is how many frames fewer than p.DeclaredFrames the
// decode of a complete file yields: 0, except for an MP3 whose gapless
// header pads less than the decoder delay (mp3GaplessShortfall).
//
// The whole file reaches ffmpeg as descriptor 3 itself. A limited FLAC
// reaches it as the read end of a pipe on descriptor 3, which a goroutine
// fills with exactly the first limit bytes of f (N-075: a descriptor, never
// a path), with the same command line. A limited MP3 is descriptor 3 itself,
// read through the subfile protocol up to limit (windowed, N-154): the mp3
// demuxer trims the gapless padding only on a seekable input.
func (t *Tools) decode(ctx context.Context, f *os.File, p ProbeResult, limit, shortfall int64) (Digest, error) {
	const op = "ffmpeg decode"
	in := f
	args := decodeArgs(demuxerOf[p.Format])
	var fed chan feedResult
	if limit == wholeFile || p.Format == FormatMP3 {
		// An MP3 is read through the subfile protocol, which seeks (N-154).
		if limit != wholeFile {
			args = windowed(args, limit)
		}
		if err := rewind(f, op); err != nil {
			return Digest{}, err
		}
	} else {
		r, w, err := os.Pipe()
		if err != nil {
			return Digest{}, newErr(CodeIO, op, "input pipe", err)
		}
		in, fed = r, make(chan feedResult, 1)
		go func() { fed <- feedPrefix(w, f, limit) }()
	}
	pcm := &pcmSink{h: sha256.New()}
	_, err := t.run.Run(ctx, Command{
		Path:    t.ffmpeg,
		Args:    args,
		Files:   []*os.File{in},
		Stdout:  pcm,
		Timeout: DecodeTimeout,
	})
	var e *Error
	if errors.As(err, &e) && e.Code == CodeToolFailed {
		// A non-zero exit of the decoder is the file failing to decode.
		e.Code, e.Op = CodeDecode, op
	}
	if fed != nil {
		// The decoder and its whole group are gone. Closing the last read
		// end makes a feeder that is still writing fail with EPIPE, so it
		// returns.
		closeErr := in.Close()
		res := <-fed
		switch {
		case res.readErr != nil:
			// The machine's fault, whatever the decoder made of the short
			// stream.
			return Digest{}, newErr(CodeIO, op, "reading the input", errors.Join(res.readErr, closeErr))
		case err != nil:
			return Digest{}, errors.Join(err, closeErr)
		case res.writeErr != nil:
			return Digest{}, newErr(CodeDecode, op, "the decoder exited before reading the whole audio stream",
				errors.Join(res.writeErr, closeErr))
		case closeErr != nil:
			return Digest{}, newErr(CodeIO, op, "closing the input pipe", closeErr)
		}
	}
	if err != nil {
		return Digest{}, err
	}

	frameBytes := int64(pcmFrameBytes * p.Audio.Channels)
	switch {
	case pcm.n == 0:
		return Digest{}, newErr(CodeDecode, op, "the decoder produced no audio", nil)
	case pcm.n%frameBytes != 0:
		return Digest{}, newErr(CodeDecode, op, strconv.FormatInt(pcm.n, 10)+
			" bytes of PCM are not a whole number of "+strconv.Itoa(p.Audio.Channels)+"-channel frames", nil)
	}
	frames := pcm.n / frameBytes
	if p.DeclaredFrames > 0 && frames != p.DeclaredFrames-shortfall {
		msg := "decoded " + strconv.FormatInt(frames, 10) +
			" frames, the container declares " + strconv.FormatInt(p.DeclaredFrames, 10)
		if shortfall > 0 {
			msg += ", of which the decoder trims " + strconv.FormatInt(shortfall, 10) +
				" more than the gapless header pads: expected " + strconv.FormatInt(p.DeclaredFrames-shortfall, 10)
		}
		return Digest{}, newErr(CodeDecode, op, msg+" (truncated, or damaged header)", nil)
	}
	return Digest{
		SampleRate: p.Audio.SampleRate,
		Channels:   p.Audio.Channels,
		Layout:     p.Audio.Layout,
		Frames:     frames,
		PCMSHA256:  hex.EncodeToString(pcm.h.Sum(nil)),
	}, nil
}

// feedResult is how feedPrefix ended: a failure to read the input, or a
// failure to write the pipe (the decoder stopped reading).
type feedResult struct {
	readErr, writeErr error
}

// feedPrefix writes exactly the first n bytes of f to w, with pread (f's
// offset is not used), then closes w so that the reader sees EOF.
func feedPrefix(w, f *os.File, n int64) feedResult {
	var res feedResult
	buf := make([]byte, readChunk)
	for off := int64(0); off < n && res.writeErr == nil; {
		k, err := f.ReadAt(buf[:min(int64(len(buf)), n-off)], off)
		if k > 0 {
			_, res.writeErr = w.Write(buf[:k])
			off += int64(k)
		}
		if err != nil && off < n {
			if err == io.EOF {
				err = errors.New("the input ended at byte " + strconv.FormatInt(off, 10) + " of " + strconv.FormatInt(n, 10))
			}
			res.readErr = err
			break
		}
	}
	if err := w.Close(); err != nil && res.writeErr == nil {
		res.writeErr = err
	}
	return res
}

// pcmSink hashes and counts the decoder's output as it streams.
type pcmSink struct {
	h hash.Hash
	n int64
}

func (s *pcmSink) Write(b []byte) (int, error) {
	s.n += int64(len(b))
	return s.h.Write(b)
}
