package media

import (
	"encoding/json"
	"testing"
	"time"
)

// classify is a pure function of ffprobe's report (§8.1). These cases cover
// what real fixtures cannot produce with the pinned tools: an undeclared
// layout on a supported format, parameters ffprobe could not determine, an
// unknown codec (FairPlay "drms").
func TestClassify(t *testing.T) {
	stream := func(extra string) string {
		return `{"index": 0, "codec_type": "audio", "codec_name": "flac", "sample_rate": "44100", "channels": 2,
			"channel_layout": "stereo", "time_base": "1/44100", "duration_ts": 132300` + extra + `}`
	}
	pic := `{"index": 1, "codec_type": "video", "codec_name": "mjpeg", "disposition": {"attached_pic": 1}}`
	for _, tc := range []struct {
		name      string
		json      string
		estimated bool
		want      ProbeResult
	}{
		{
			name: "flac with a cover",
			json: `{"format": {"format_name": "flac"}, "streams": [` + stream("") + `,` + pic + `]}`,
			want: ProbeResult{Class: ClassAudio, Container: "flac", Format: FormatFLAC, AttachedPictures: 1,
				Audio:          AudioInfo{Codec: "flac", SampleRate: 44100, Channels: 2, Layout: "stereo", Duration: 3 * time.Second},
				DeclaredFrames: 132300},
		},
		{
			name: "undeclared layout",
			json: `{"format": {"format_name": "flac"}, "streams": [{"codec_type": "audio", "codec_name": "flac",
				"sample_rate": "48000", "channels": 3, "time_base": "1/48000"}]}`,
			want: ProbeResult{Class: ClassAudio, Container: "flac", Format: FormatFLAC,
				Audio: AudioInfo{Codec: "flac", SampleRate: 48000, Channels: 3, Layout: "unknown:3"}},
		},
		{
			name: "layout printed as unknown",
			json: `{"format": {"format_name": "mov,mp4,m4a,3gp,3g2,mj2"}, "streams": [{"codec_type": "audio", "codec_name": "alac",
				"sample_rate": "96000", "channels": 1, "channel_layout": "unknown", "time_base": "1/96000", "duration_ts": 96000}]}`,
			want: ProbeResult{Class: ClassAudio, Container: containerMOV, Format: FormatM4AALAC,
				Audio: AudioInfo{Codec: "alac", SampleRate: 96000, Channels: 1, Layout: "unknown:1", Duration: time.Second}},
		},
		{
			name: "mp3 with an exact length",
			json: `{"format": {"format_name": "mp3"}, "streams": [{"codec_type": "audio", "codec_name": "mp3",
				"sample_rate": "44100", "channels": 2, "channel_layout": "stereo", "time_base": "1/14112000", "duration_ts": 141120000}]}`,
			want: ProbeResult{Class: ClassAudio, Container: "mp3", Format: FormatMP3,
				Audio:          AudioInfo{Codec: "mp3", SampleRate: 44100, Channels: 2, Layout: "stereo", Duration: 10 * time.Second},
				DeclaredFrames: 441000},
		},
		{
			name:      "mp3 with an estimated length",
			estimated: true,
			json: `{"format": {"format_name": "mp3"}, "streams": [{"codec_type": "audio", "codec_name": "mp3",
				"sample_rate": "44100", "channels": 2, "channel_layout": "stereo", "time_base": "1/14112000", "duration_ts": 141120000}]}`,
			want: ProbeResult{Class: ClassAudio, Container: "mp3", Format: FormatMP3,
				Audio: AudioInfo{Codec: "mp3", SampleRate: 44100, Channels: 2, Layout: "stereo", Duration: 10 * time.Second}},
		},
		{
			name: "mp3 length not a whole number of frames",
			json: `{"format": {"format_name": "mp3"}, "streams": [{"codec_type": "audio", "codec_name": "mp3",
				"sample_rate": "44100", "channels": 2, "channel_layout": "stereo", "time_base": "1/14112000", "duration_ts": 141120001}]}`,
			// One tick of 1/14112000 s is 70.86 ns, truncated to 70.
			want: ProbeResult{Class: ClassAudio, Container: "mp3", Format: FormatMP3,
				Audio: AudioInfo{Codec: "mp3", SampleRate: 44100, Channels: 2, Layout: "stereo", Duration: 10*time.Second + 70}},
		},
		{
			name: "flac with a time base other than 1/rate declares nothing",
			json: `{"format": {"format_name": "flac"}, "streams": [` + stream(`, "time_base": "1/1000"`) + `]}`,
			want: ProbeResult{Class: ClassAudio, Container: "flac", Format: FormatFLAC,
				Audio: AudioInfo{Codec: "flac", SampleRate: 44100, Channels: 2, Layout: "stereo", Duration: 132300 * time.Millisecond}},
		},
		{
			name: "aac in m4a declares no exact length",
			json: `{"format": {"format_name": "mov,mp4,m4a,3gp,3g2,mj2"}, "streams": [{"codec_type": "audio", "codec_name": "aac",
				"sample_rate": "44100", "channels": 2, "channel_layout": "stereo", "time_base": "1/44100", "duration_ts": 441000}]}`,
			want: ProbeResult{Class: ClassAudio, Container: containerMOV, Format: FormatM4AAAC,
				Audio: AudioInfo{Codec: "aac", SampleRate: 44100, Channels: 2, Layout: "stereo", Duration: 10 * time.Second}},
		},
		{
			name: "unknown codec (FairPlay)",
			json: `{"format": {"format_name": "mov,mp4,m4a,3gp,3g2,mj2"}, "streams": [{"codec_type": "audio",
				"sample_rate": "44100", "channels": 2}]}`,
			want: ProbeResult{Class: ClassUnsupportedAudio, Reason: ReasonUnsupportedFormat, Container: containerMOV,
				Detail: "codec unknown in container " + containerMOV},
		},
		{
			name: "sample rate not determined",
			json: `{"format": {"format_name": "flac"}, "streams": [{"codec_type": "audio", "codec_name": "flac", "sample_rate": "0", "channels": 2}]}`,
			want: ProbeResult{Class: ClassUnsupportedAudio, Reason: ReasonInvalidParameters, Container: "flac",
				Detail: `sample rate "0", 2 channels`},
		},
		{
			name: "no channels",
			json: `{"format": {"format_name": "mp3"}, "streams": [{"codec_type": "audio", "codec_name": "mp3", "sample_rate": "44100", "channels": 0}]}`,
			want: ProbeResult{Class: ClassUnsupportedAudio, Reason: ReasonInvalidParameters, Container: "mp3",
				Detail: `sample rate "44100", 0 channels`},
		},
		{
			name: "subtitle stream",
			json: `{"format": {"format_name": "mov,mp4,m4a,3gp,3g2,mj2"}, "streams": [` + stream("") + `,{"codec_type": "subtitle"}]}`,
			want: ProbeResult{Class: ClassUnsupportedAudio, Reason: ReasonOtherStream, Container: containerMOV,
				Detail: "1 stream(s) that are neither audio nor attached pictures"},
		},
		{
			name: "encrypted packets win over everything else",
			json: `{"format": {"format_name": "flac"}, "streams": [` + stream("") + `,{"codec_type": "audio", "codec_name": "flac"}],
				"packets": [{"stream_index": 0}, {"stream_index": 0, "side_data_list": [{"side_data_type": "Skip Samples"}, {"side_data_type": "Encryption info"}]}]}`,
			want: ProbeResult{Class: ClassUnsupportedAudio, Reason: ReasonEncrypted, Container: "flac",
				Detail: "the packets carry encryption (DRM) information"},
		},
		{
			name: "only a cover",
			json: `{"format": {"format_name": "flac"}, "streams": [` + pic + `]}`,
			want: ProbeResult{Class: ClassNoAudio, Container: "flac", AttachedPictures: 1},
		},
		{
			name: "nothing at all",
			json: `{"format": {"format_name": "ffmetadata"}, "streams": []}`,
			want: ProbeResult{Class: ClassNoAudio, Container: "ffmetadata"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out ffprobeOutput
			if err := json.Unmarshal([]byte(tc.json), &out); err != nil {
				t.Fatal(err)
			}
			if got := classify(out, tc.estimated); got != tc.want {
				t.Fatalf("classify:\n got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// The MP3 estimation warning is recognized only as the full line libavformat
// prints for the mp3 demuxer.
func TestMP3Estimated(t *testing.T) {
	for stderr, want := range map[string]bool{
		"[mp3 @ 0x6a05900] Estimating duration from bitrate, this may be inaccurate\n":                            true,
		"[mp3 @ 0x1] Something else\n[mp3 @ 0x55d0c0a0] Estimating duration from bitrate, this may be inaccurate": true,
		"[aac @ 0x6081200] Estimating duration from bitrate, this may be inaccurate\n":                            false,
		"x[mp3 @ 0x6a05900] Estimating duration from bitrate, this may be inaccurate\n":                           false,
		"[mp3 @ 0x6a05900] Estimating duration from bitrate, this may be inaccurate!\n":                           false,
		"": false,
	} {
		if got := mp3Estimated([]byte(stderr)); got != want {
			t.Errorf("mp3Estimated(%q) = %v, want %v", stderr, got, want)
		}
	}
}
