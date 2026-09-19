// SPDX-License-Identifier: AGPL-3.0-or-later

package restream

import (
	"fmt"
	"net/url"
	"strings"

	"vhfcam-restream/internal/config"
)

// BuildArgs assembles the ffmpeg invocation for one stream run.
//
// The cameras emit H.264 video plus two audio tracks (AAC mono and Opus
// stereo). RTMP/FLV can only carry AAC, and ffmpeg's default stream selection
// would pick the 2-channel Opus track — so the audio is mapped explicitly
// (first audio track) and transcoded to AAC. Video is copied unless the
// overlay is enabled, which forces a software transcode to burn it in.
func BuildArgs(cfg *config.Config, sourceURL string) []string {
	args := []string{
		"-hide_banner",
		"-loglevel", "warning",
		"-nostdin",
		"-rtsp_transport", cfg.RTSPTransport,
		"-i", sourceURL,
		"-map", cfg.VideoMap,
		"-map", cfg.AudioMap,
	}
	if vf := BuildVideoFilter(cfg); vf != "" {
		args = append(args, "-vf", vf)
		args = append(args,
			"-c:v", "libx264",
			"-preset", "superfast",
			"-crf", "23",
			"-maxrate", "2500k",
			"-bufsize", "1250k",
			"-g", "40",
			"-pix_fmt", "yuv420p",
		)
	} else {
		args = append(args, "-c:v", cfg.VideoCodec)
	}
	args = append(args,
		"-c:a", cfg.AudioCodec,
		"-flvflags", "no_duration_filesize",
		"-stats_period", "5",
		"-progress", "pipe:1",
		"-f", "flv",
		cfg.YouTubeURL + "/" + cfg.StreamKey,
	)
	return args
}

// BuildVideoFilter returns the -vf drawtext chain for the operational-data
// overlay, or "" when the overlay is disabled. Each field is its own drawtext
// with reload=1 reading a textfile the overlay writer keeps current; the TX
// line is red and simply goes empty when not transmitting.
func BuildVideoFilter(cfg *config.Config) string {
	o := &cfg.Overlay
	if !o.Enabled {
		return ""
	}
	lineH := o.FontSize + 8
	panelW := o.FontSize * 13
	panelH := o.Margin*2 + 4*lineH
	txX := panelW - o.Margin - 2*o.FontSize
	txY := panelH - o.Margin - o.FontSize

	dt := func(name, color string, x, y int) string {
		return fmt.Sprintf(
			"drawtext=fontfile=%s:textfile=%s/%s.txt:reload=1:fontcolor=%s:fontsize=%d:x=%d:y=%d",
			o.FontFile, o.Dir, name, color, o.FontSize, x, y)
	}
	parts := []string{
		fmt.Sprintf("drawbox=x=0:y=0:w=%d:h=%d:color=black@0.5:t=fill", panelW, panelH),
		dt("az", "white", o.Margin, o.Margin),
		dt("el", "white", o.Margin, o.Margin+lineH),
		dt("freq", "white", o.Margin, o.Margin+2*lineH),
		dt("tx", "red", txX, txY),
	}
	return strings.Join(parts, ",")
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
