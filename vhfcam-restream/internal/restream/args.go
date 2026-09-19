// SPDX-License-Identifier: AGPL-3.0-or-later

package restream

import (
	"net/url"
	"strings"

	"vhfcam-restream/internal/config"
)

// BuildArgs assembles the ffmpeg invocation for one stream run.
//
// The cameras emit H.264 video plus two audio tracks (AAC mono and Opus
// stereo). RTMP/FLV can only carry AAC, and ffmpeg's default stream selection
// would pick the 2-channel Opus track — so the audio is mapped explicitly by
// codec. Video and audio are both copied: no transcoding on the Pi.
func BuildArgs(cfg *config.Config, sourceURL string) []string {
	return []string{
		"-hide_banner",
		"-loglevel", "warning",
		"-nostdin",
		"-rtsp_transport", cfg.RTSPTransport,
		"-i", sourceURL,
		"-map", cfg.VideoMap,
		"-map", cfg.AudioMap,
		"-c:v", cfg.VideoCodec,
		"-c:a", cfg.AudioCodec,
		"-flvflags", "no_duration_filesize",
		"-stats_period", "5",
		"-progress", "pipe:1",
		"-f", "flv",
		cfg.YouTubeURL + "/" + cfg.StreamKey,
	}
}

// RedactArgs returns a copy of args with every occurrence of each secret
// replaced, for safe logging (the output URL carries the stream key and the
// source URL carries the camera token).
func RedactArgs(args []string, secrets ...string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		for _, s := range secrets {
			if s != "" && strings.Contains(a, s) {
				a = strings.ReplaceAll(a, s, "[redacted]")
			}
		}
		out[i] = a
	}
	return out
}

// RedactURL masks everything after the host of an rtsps/rtmp URL for logging
// (both URLs embed credentials: the RTSPS path token, the RTMP stream key).
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "[unparseable-url]"
	}
	return u.Scheme + "://" + u.Host + "/[redacted]"
}
