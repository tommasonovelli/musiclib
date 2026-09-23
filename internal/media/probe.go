package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Class is what the probe says a file is (§7.2, §8.1).
type Class string

const (
	// ClassAudio is supported audio: FLAC, MP3, or M4A with exactly one AAC
	// or ALAC stream, attached pictures allowed, no other stream, no
	// encryption (§8.1). It still has to decode fully (AudioDigest, §7.6).
	ClassAudio Class = "audio"
	// ClassUnsupportedAudio has at least one audio stream but is not
	// supported: another codec or container, several audio streams, a real
	// video stream, another kind of stream, or encryption (Reason says
	// which). For the importer it is an album error, never an attachment.
	ClassUnsupportedAudio Class = "unsupported_audio"
	// ClassNoAudio was read by ffprobe and has no audio stream: an image, a
	// video without sound, but also a damaged audio file whose container is
	// still recognized, like a .flac whose only stream is its cover (the
	// case "only a cover" of classify_test.go). The rule is the
	// importer's (§7.2): a file with a known audio extension and no audio
	// stream is "audio corrotto", an album error, not an attachment; only
	// the others become attachments.
	ClassNoAudio Class = "no_audio"
	// ClassUnreadable could not be read as media at all: empty, text, PDF,
	// or damaged beyond recognition. Whether it is an attachment or a
	// corrupt audio file depends on its extension (§7.2), which only the
	// importer knows.
	ClassUnreadable Class = "unreadable"
)

// Reasons of ClassUnsupportedAudio and ClassUnreadable. They are stable
// codes; ProbeResult.Detail has the specifics.
const (
	ReasonProbeFailed       = "probe_failed"
	ReasonUnsupportedFormat = "unsupported_format"
	ReasonMultipleAudio     = "multiple_audio_streams"
	ReasonVideo             = "video_stream"
	ReasonOtherStream       = "other_stream"
	ReasonEncrypted         = "encrypted"
	ReasonInvalidParameters = "invalid_audio_parameters"
)

// Values of blobs.format for audio (§4.2). They come from the content,
// never from the extension.
const (
	FormatFLAC    = "flac"
	FormatMP3     = "mp3"
	FormatM4AAAC  = "m4a-aac"
	FormatM4AALAC = "m4a-alac"
)

// AudioInfo are the parameters of the one audio stream of a supported file,
// as ffprobe reports them.
type AudioInfo struct {
	// Codec is ffprobe's codec name: flac, mp3, aac or alac.
	Codec      string
	SampleRate int
	Channels   int
	// Layout is the channel layout in FFmpeg's canonical description
	// ("mono", "stereo", "5.1", "5.1(side)", ...), or "unknown:<channels>"
	// when the file does not declare one (§8.4).
	Layout string
	// Duration is what the container declares, 0 if it declares nothing. It
	// is for display: AudioDigest counts the frames that actually decode.
	Duration time.Duration
}

// ProbeResult is the classification of one file.
type ProbeResult struct {
	Class Class
	// Reason is one of the Reason* codes for ClassUnsupportedAudio and
	// ClassUnreadable, empty otherwise; Detail explains it in words.
	Reason string
	Detail string
	// Container is ffprobe's demuxer name ("flac", "mp3",
	// "mov,mp4,m4a,3gp,3g2,mj2", "ogg", ...), empty when unreadable.
	Container string
	// Format is the blobs.format value, set for ClassAudio only.
	Format string
	// Audio is set for ClassAudio only.
	Audio AudioInfo
	// AttachedPictures counts the attached-picture streams (embedded
	// covers), whatever the class.
	AttachedPictures int
	// DeclaredFrames is the exact number of frames the container declares
	// for the audio stream, 0 when it declares none or only an estimate:
	// the FLAC STREAMINFO total, or the frame count of an MP3 Xing, Info or
	// VBRI header. AudioDigest requires the decode to produce exactly that
	// many (NOTES.md N-078).
	DeclaredFrames int64
}

// probeOutputLimit bounds ffprobe's JSON. The requested entries are few, so
// only a file with an absurd number of streams comes near it.
const probeOutputLimit = 4 << 20

// probePackets is how many packets ffprobe reads to look for encryption
// side data: encrypted streams mark every encrypted packet.
const probePackets = "%+#16"

// probeEntries are the only fields ffprobe prints: tags stay out, they are
// the TagLib helper's job and could be large.
const probeEntries = "format=format_name" +
	":stream=index,codec_type,codec_name,sample_rate,channels,channel_layout,time_base,duration_ts" +
	":stream_disposition=attached_pic" +
	":packet=stream_index:packet_side_data=side_data_type"

// probeArgs is the ffprobe command line for the file on descriptor 3. Only
// the "fd" protocol is allowed, so the demuxer cannot open anything else
// (a playlist, a concat list, an external reference). The log level is
// "warning" because one warning is significant (mp3Estimated).
func probeArgs() []string {
	return []string{
		"-hide_banner",
		"-loglevel", "warning",
		"-show_error",
		"-print_format", "json",
		"-show_entries", probeEntries,
		"-read_intervals", probePackets,
		"-protocol_whitelist", "fd",
		"-fd", "3",
		"fd:",
	}
}

// Probe classifies the regular file f by its content with ffprobe (§7.2,
// §8.1). f must be open for reading, typically through internal/fsops; its
// offset is reset to 0 and is unspecified afterwards. It must not be used
// concurrently while Probe runs.
//
// A file ffprobe cannot read is ClassUnreadable, not an error. Errors are
// failures of the tool or of the adapter: timeout, cancellation, a crash, an
// I/O error on the input.
func (t *Tools) Probe(ctx context.Context, f *os.File) (ProbeResult, error) {
	if err := rewind(f, "ffprobe"); err != nil {
		return ProbeResult{}, err
	}
	var out bytes.Buffer
	res, runErr := t.run.Run(ctx, Command{
		Path:        t.ffprobe,
		Args:        probeArgs(),
		Files:       []*os.File{f},
		Stdout:      &out,
		StdoutLimit: probeOutputLimit,
		Timeout:     InspectTimeout,
	})
	var e *Error
	switch {
	case runErr == nil:
	case errors.As(runErr, &e) && e.Code == CodeToolFailed && e.ExitCode == 1:
		// ffprobe exits 1 when it cannot open or read the input; it then
		// prints the error object that -show_error asks for. Without it the
		// failure is the tool's (a bad option, for example), not the file's.
		var fail ffprobeOutput
		if json.Unmarshal(out.Bytes(), &fail) != nil || fail.Error == nil {
			return ProbeResult{}, runErr
		}
		return unreadable(fail.Error, runErr)
	default:
		return ProbeResult{}, runErr
	}

	var parsed ffprobeOutput
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		return ProbeResult{}, &Error{Code: CodeOutputInvalid, Op: "ffprobe", Msg: "the JSON output does not parse",
			ExitCode: 0, Stderr: res.Stderr, Err: err}
	}
	if parsed.Format == nil {
		return ProbeResult{}, &Error{Code: CodeOutputInvalid, Op: "ffprobe", Msg: "the output has no format section",
			ExitCode: 0, Stderr: res.Stderr}
	}
	return classify(parsed, mp3Estimated(res.Stderr)), nil
}

// ffprobeOutput is the part of ffprobe's JSON the adapter reads.
type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
	Packets []struct {
		StreamIndex  int `json:"stream_index"`
		SideDataList []struct {
			SideDataType string `json:"side_data_type"`
		} `json:"side_data_list"`
	} `json:"packets"`
	Format *struct {
		FormatName string `json:"format_name"`
	} `json:"format"`
	Error *ffprobeError `json:"error"`
}

type ffprobeStream struct {
	Index         int    `json:"index"`
	CodecType     string `json:"codec_type"`
	CodecName     string `json:"codec_name"`
	SampleRate    string `json:"sample_rate"`
	Channels      int    `json:"channels"`
	ChannelLayout string `json:"channel_layout"`
	TimeBase      string `json:"time_base"`
	DurationTS    *int64 `json:"duration_ts"`
	Disposition   struct {
		AttachedPic int `json:"attached_pic"`
	} `json:"disposition"`
}

type ffprobeError struct {
	Code   int    `json:"code"`
	String string `json:"string"`
}

// FFmpeg error codes (AVERROR(errno) is -errno) that mean the machine
// failed, not the file.
const (
	averrorEIO    = -5
	averrorENOMEM = -12
)

// unreadable turns ffprobe's error object into ClassUnreadable, except for
// errors of the machine, which stay errors.
func unreadable(fe *ffprobeError, runErr error) (ProbeResult, error) {
	if fe.Code == averrorEIO || fe.Code == averrorENOMEM {
		return ProbeResult{}, runErr
	}
	return ProbeResult{
		Class:  ClassUnreadable,
		Reason: ReasonProbeFailed,
		Detail: fe.String + " (" + strconv.Itoa(fe.Code) + ")",
	}, nil
}

// mp3EstimatedLine is the warning libavformat prints when an MP3 has no
// Xing, Info or VBRI frame count and its duration is estimated from the
// bitrate. ffprobe reports that duration exactly like an exact one, and this
// line is the only difference (NOTES.md N-078). If a future FFmpeg changed
// the wording, the estimate would be taken as exact and complete files would
// be refused, never the reverse; the tests catch it on a bump.
var mp3EstimatedLine = regexp.MustCompile(`(?m)^\[mp3 @ 0x[0-9a-f]+\] Estimating duration from bitrate, this may be inaccurate$`)

func mp3Estimated(stderr []byte) bool {
	return mp3EstimatedLine.Match(stderr)
}

// Containers of supported audio, by ffprobe's demuxer name.
const (
	containerFLAC = "flac"
	containerMP3  = "mp3"
	containerMOV  = "mov,mp4,m4a,3gp,3g2,mj2"
)

// classify applies §8.1 to ffprobe's report. It does no I/O.
func classify(out ffprobeOutput, mp3Estimated bool) ProbeResult {
	r := ProbeResult{Container: out.Format.FormatName}
	var audio []ffprobeStream
	var video, other int
	for _, s := range out.Streams {
		switch {
		case s.CodecType == "audio":
			audio = append(audio, s)
		case s.CodecType == "video" && s.Disposition.AttachedPic == 1:
			r.AttachedPictures++
		case s.CodecType == "video":
			video++
		default:
			other++
		}
	}
	unsupported := func(reason, detail string) ProbeResult {
		r.Class, r.Reason, r.Detail = ClassUnsupportedAudio, reason, detail
		return r
	}
	switch {
	case len(audio) == 0:
		r.Class = ClassNoAudio
		return r
	case encrypted(out):
		return unsupported(ReasonEncrypted, "the packets carry encryption (DRM) information")
	case len(audio) > 1:
		return unsupported(ReasonMultipleAudio, strconv.Itoa(len(audio))+" audio streams")
	case video > 0:
		return unsupported(ReasonVideo, strconv.Itoa(video)+" video stream(s) that are not attached pictures")
	case other > 0:
		return unsupported(ReasonOtherStream, strconv.Itoa(other)+" stream(s) that are neither audio nor attached pictures")
	}

	s := audio[0]
	format := formatOf(r.Container, s.CodecName)
	if format == "" {
		codec := s.CodecName
		if codec == "" {
			codec = "unknown"
		}
		return unsupported(ReasonUnsupportedFormat, "codec "+codec+" in container "+r.Container)
	}
	rate, err := strconv.Atoi(s.SampleRate)
	if err != nil || rate <= 0 || s.Channels <= 0 {
		return unsupported(ReasonInvalidParameters,
			"sample rate "+strconv.Quote(s.SampleRate)+", "+strconv.Itoa(s.Channels)+" channels")
	}
	r.Class, r.Format = ClassAudio, format
	r.Audio = AudioInfo{
		Codec:      s.CodecName,
		SampleRate: rate,
		Channels:   s.Channels,
		Layout:     normalizeLayout(s.ChannelLayout, s.Channels),
		Duration:   duration(s),
	}
	r.DeclaredFrames = declaredFrames(r.Container, s, rate, mp3Estimated)
	return r
}

// formatOf maps a supported container and codec to blobs.format, "" when
// the pair is not supported (§8.1). Any ISO/QuickTime file the mov demuxer
// reads counts as M4A, whatever its brand (NOTES.md N-077).
func formatOf(container, codec string) string {
	switch {
	case container == containerFLAC && codec == "flac":
		return FormatFLAC
	case container == containerMP3 && codec == "mp3":
		return FormatMP3
	case container == containerMOV && codec == "aac":
		return FormatM4AAAC
	case container == containerMOV && codec == "alac":
		return FormatM4AALAC
	}
	return ""
}

// encrypted reports whether a probed packet carries encryption info (CENC
// and similar). FairPlay-protected files are not decodable by FFmpeg at all
// and come out as an unknown codec.
func encrypted(out ffprobeOutput) bool {
	for _, p := range out.Packets {
		for _, sd := range p.SideDataList {
			if sd.SideDataType == "Encryption info" {
				return true
			}
		}
	}
	return false
}

// normalizeLayout returns FFmpeg's description of a declared layout, or
// "unknown:<channels>" for an undeclared one (ffprobe then omits the field,
// or prints "unknown").
func normalizeLayout(layout string, channels int) string {
	if layout == "" || layout == "unknown" {
		return "unknown:" + strconv.Itoa(channels)
	}
	return layout
}

// timeBase parses ffprobe's "num/den".
func timeBase(s string) (num, den int64, ok bool) {
	a, b, found := strings.Cut(s, "/")
	if !found {
		return 0, 0, false
	}
	n, err1 := strconv.ParseInt(a, 10, 64)
	d, err2 := strconv.ParseInt(b, 10, 64)
	if err1 != nil || err2 != nil || n <= 0 || d <= 0 {
		return 0, 0, false
	}
	return n, d, true
}

// duration converts the stream's duration_ts to a time.Duration, 0 when it
// is absent or not positive.
func duration(s ffprobeStream) time.Duration {
	num, den, ok := timeBase(s.TimeBase)
	if s.DurationTS == nil || *s.DurationTS <= 0 || !ok {
		return 0
	}
	ns := new(big.Int).Mul(big.NewInt(*s.DurationTS), big.NewInt(num))
	ns.Mul(ns, big.NewInt(int64(time.Second)))
	ns.Quo(ns, big.NewInt(den))
	if !ns.IsInt64() {
		return 0
	}
	return time.Duration(ns.Int64())
}

// declaredFrames returns the exact frame count the container declares, 0
// when it declares none or only an estimate:
//   - FLAC: STREAMINFO's total, which ffprobe reports as duration_ts in a
//     1/sample_rate time base; absent when STREAMINFO says 0 (unknown).
//   - MP3: the Xing/Info/VBRI frame count, already net of the LAME encoder
//     delay and padding, unless libavformat had to estimate it from the
//     bitrate. It must be a whole number of frames.
//   - M4A: none. An AAC decode includes the encoder's end padding, so its
//     count differs from the declared one by design; a truncated M4A fails
//     in the demuxer, because its sample table points past the end.
func declaredFrames(container string, s ffprobeStream, rate int, mp3Estimated bool) int64 {
	num, den, ok := timeBase(s.TimeBase)
	if s.DurationTS == nil || *s.DurationTS <= 0 || !ok {
		return 0
	}
	ts := *s.DurationTS
	switch container {
	case containerFLAC:
		if num == 1 && den == int64(rate) {
			return ts
		}
	case containerMP3:
		if mp3Estimated {
			return 0
		}
		frames := new(big.Int).Mul(big.NewInt(ts), big.NewInt(num*int64(rate)))
		rem := new(big.Int)
		frames.QuoRem(frames, big.NewInt(den), rem)
		if rem.Sign() == 0 && frames.IsInt64() {
			return frames.Int64()
		}
	}
	return 0
}

// rewind puts the offset of f, which a tool shares, back to the start.
func rewind(f *os.File, op string) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return newErr(CodeIO, op, "cannot rewind the input", err)
	}
	return nil
}
