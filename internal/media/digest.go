package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
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
//     and a byte count; nothing is held in memory.
//  3. The byte count must be a positive multiple of 8 × channels, and, when
//     the container declares an exact length, equal to it.
//
// A file that is not supported audio fails with CodeNotSupported; one that
// does not decode completely, with CodeDecode. Timeouts (DecodeTimeout) and
// cancellation have their own codes. f follows the rules of Probe.
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
	return t.decode(ctx, f, p)
}

// decode is steps 2 and 3 of AudioDigest, for a file already classified as
// ClassAudio.
func (t *Tools) decode(ctx context.Context, f *os.File, p ProbeResult) (Digest, error) {
	const op = "ffmpeg decode"
	if err := rewind(f, op); err != nil {
		return Digest{}, err
	}
	pcm := &pcmSink{h: sha256.New()}
	_, err := t.run.Run(ctx, Command{
		Path:    t.ffmpeg,
		Args:    decodeArgs(demuxerOf[p.Format]),
		Files:   []*os.File{f},
		Stdout:  pcm,
		Timeout: DecodeTimeout,
	})
	var e *Error
	if errors.As(err, &e) && e.Code == CodeToolFailed {
		// A non-zero exit of the decoder is the file failing to decode.
		e.Code, e.Op = CodeDecode, op
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
	if p.DeclaredFrames > 0 && frames != p.DeclaredFrames {
		return Digest{}, newErr(CodeDecode, op, "decoded "+strconv.FormatInt(frames, 10)+
			" frames, the container declares "+strconv.FormatInt(p.DeclaredFrames, 10)+
			" (truncated, or damaged header)", nil)
	}
	return Digest{
		SampleRate: p.Audio.SampleRate,
		Channels:   p.Audio.Channels,
		Layout:     p.Audio.Layout,
		Frames:     frames,
		PCMSHA256:  hex.EncodeToString(pcm.h.Sum(nil)),
	}, nil
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
