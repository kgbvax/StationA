// SPDX-License-Identifier: AGPL-3.0-or-later

package restream

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"vhfcam-restream/internal/config"
)

// BuildArgs assembles the ffmpeg invocation for the YouTube sink.
func BuildArgs(cfg *config.Config, sourceURL string) []string {
	inputs, vmap, fc, audioMap := buildInputsAndOverlay(cfg, sourceURL)
	transcode := fc != ""
	args := append(inputs, "-map", vmap, "-map", audioMap)
	if fc != "" {
		// The bar is part of the filter complex when the logo rides along.
		args = append(args, "-filter_complex", fc)
	} else if vf := BuildVideoFilter(cfg); vf != "" {
		args = append(args, "-vf", vf)
		transcode = true
	}
	return append(args, youtubeOutputArgs(cfg, transcode)...)
}

// BuildPreviewArgs assembles the ffmpeg invocation for the local HLS preview
// sink. It is a separate ffmpeg process on purpose: the LAN preview must keep
// working when YouTube or the internet is down, which kills the YouTube push.
// With radio_audio configured, the second input is the IC-9700's demodulated
// audio (fed by the preview's silence-filling source) and the camera's own
// audio track is replaced by it.
func BuildPreviewArgs(cfg *config.Config, sourceURL string) []string {
	inputs, vmap, fc, audioMap := buildInputsAndOverlay(cfg, sourceURL)
	transcode := fc != ""
	args := append(inputs, "-map", vmap, "-map", audioMap)
	if fc != "" {
		// The bar is part of the filter complex when the logo rides along.
		args = append(args, "-filter_complex", fc)
	} else if vf := BuildVideoFilter(cfg); vf != "" {
		args = append(args, "-vf", vf)
		transcode = true
	}
	return append(args, previewOutputArgs(cfg, transcode)...)
}

// buildInputsAndOverlay assembles the input section (global flags, camera,
// optional radio PCM and logo image) and returns the input args, the video
// label for the sink's -map ("0:v" plainly, "[vout]" through the filter
// complex), the audio label ("0:a:0" camera track, or "1:a:0" radio PCM when
// radio_audio is configured) and the filter_complex itself ("" when none).
//
// The cameras emit H.264 video plus two audio tracks (AAC mono and Opus
// stereo); ffmpeg's default selection would pick the 2-channel Opus track,
// which RTMP/FLV cannot carry — audio is always mapped explicitly per sink.
func buildInputsAndOverlay(cfg *config.Config, sourceURL string) (args []string, vmap, fc, audioMap string) {
	args = []string{
		"-hide_banner",
		"-loglevel", "warning",
		"-nostdin",
		"-rtsp_transport", cfg.RTSPTransport,
		"-i", sourceURL,
	}
	if cfg.Preview.RadioAudio != "" {
		args = append(args,
			"-f", "s16le",
			"-ar", "48000",
			"-ac", "1",
			"-i", RadioAudioInputURL(cfg.Preview.RadioAudio),
		)
	}

	// The logo rides the overlay toggle: enabled + configured opens the
	// filtergraph path (which forces the transcode), otherwise plain maps.
	vmap = "0:v"
	audioMap = "0:a:0"
	if !cfg.Overlay.Enabled || cfg.Overlay.Logo == "" {
		return args, vmap, fc, audioMap
	}

	logoIdx := 0
	for _, a := range args {
		if a == "-i" {
			logoIdx++
		}
	}
	args = append(args, "-i", cfg.Overlay.Logo)

	bar := BuildVideoFilter(cfg)
	barSrc := "0:v"
	chain := ""
	if bar != "" {
		chain = fmt.Sprintf("[0:v]%s[bar];", bar)
		barSrc = "bar"
	}
	// Bottom-left corner: the dragon sits directly above the data bar
	// (bar height = fontsize + 2×margin), scaled to 140 px height.
	barH := cfg.Overlay.FontSize + 2*cfg.Overlay.Margin
	fc = fmt.Sprintf("%s[%d:v]scale=-1:140,colorkey=black:0.1:0[dl];[%s][dl]overlay=x=%d:y=main_h-%d-140[vout]",
		chain, logoIdx, barSrc, cfg.Overlay.Margin, cfg.Overlay.Margin+barH)
	vmap = "[vout]"
	return args, vmap, fc, audioMap
}

// RadioAudioInputURL derives the ffmpeg input from the configured UDP bind
// address (":45031" -> "tcp://127.0.0.1:45031"): the preview's audio source
// re-publishes the received PCM on loopback TCP with silence fill.
func RadioAudioInputURL(bindAddr string) string {
	_, port, err := net.SplitHostPort(bindAddr)
	if err != nil || port == "" {
		return ""
	}
	return "tcp://127.0.0.1:" + port
}

// videoCodecArgs returns the video codec arguments: a software x264 transcode
// when a filtergraph needs burning in (overlay bar or logo), otherwise a
// straight copy.
func videoCodecArgs(cfg *config.Config, transcode bool) []string {
	if transcode {
		return []string{
			"-c:v", "libx264",
			"-preset", "superfast",
			"-crf", "23",
			"-maxrate", "2500k",
			"-bufsize", "1250k",
			"-g", "40",
			"-pix_fmt", "yuv420p",
		}
	}
	return []string{"-c:v", cfg.VideoCodec}
}

func youtubeOutputArgs(cfg *config.Config, transcode bool) []string {
	args := videoCodecArgs(cfg, transcode)
	return append(args,
		"-c:a", cfg.AudioCodec,
		"-flvflags", "no_duration_filesize",
		"-stats_period", "5",
		"-progress", "pipe:1",
		"-f", "flv",
		cfg.YouTubeURL + "/" + cfg.StreamKey,
	)
}

func previewOutputArgs(cfg *config.Config, transcode bool) []string {
	p := &cfg.Preview
	args := videoCodecArgs(cfg, transcode)
	return append(args,
		"-c:a", cfg.AudioCodec,
		"-stats_period", "5",
		"-progress", "pipe:1",
		"-f", "hls",
		"-hls_time", strconv.Itoa(p.HlsTimeS),
		"-hls_list_size", strconv.Itoa(p.ListSize),
		"-hls_flags", "delete_segments+temp_file",
		"-hls_segment_filename", filepath.Join(p.Dir, "seg_%05d.ts"),
		filepath.Join(p.Dir, "live.m3u8"),
	)
}

// BuildVideoFilter returns the bottom-bar drawtext chain for the
// operational-data overlay, or "" when the overlay is disabled. AZ, EL and
// frequency share one line at the bottom edge; the red TX indicator is
// right-aligned on the same line and simply goes empty when not
// transmitting. Each field is its own drawtext with reload=1 reading a
// textfile the overlay writer keeps current.
func BuildVideoFilter(cfg *config.Config) string {
	o := &cfg.Overlay
	if !o.Enabled {
		return ""
	}
	fs := o.FontSize
	m := o.Margin
	barH := fs + 2*m
	textY := "main_h-" + strconv.Itoa(fs+m)

	dt := func(file, color, x string) string {
		return fmt.Sprintf(
			"drawtext=fontfile=%s:textfile=%s/%s.txt:reload=1:fontcolor=%s:fontsize=%d:x=%s:y=%s",
			o.FontFile, o.Dir, file, color, fs, x, textY)
	}
	parts := []string{
		fmt.Sprintf("drawbox=x=0:y=ih-%d:w=iw:h=%d:color=black@0.5:t=fill", barH, barH),
		dt("az", "white", strconv.Itoa(m)),
		dt("el", "white", strconv.Itoa(m+6*fs)),
		dt("freq", "white", strconv.Itoa(m+12*fs)),
		dt("tx", "red", "main_w-tw-"+strconv.Itoa(m)),
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
