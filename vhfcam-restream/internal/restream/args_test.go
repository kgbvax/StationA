// SPDX-License-Identifier: AGPL-3.0-or-later

package restream

import (
	"strings"
	"testing"

	"vhfcam-restream/internal/config"
)

func testCfg() *config.Config {
	c := config.Default()
	c.SourceURL = "rtsps://192.168.0.1:7441/TOKEN1?enableSrtp"
	c.SourceHDURL = "rtsps://192.168.0.1:7441/TOKEN2?enableSrtp"
	c.YouTubeURL = "rtmp://a.rtmp.youtube.com/live2"
	c.StreamKey = "SEKRIT"
	return &c
}

func TestBuildArgs(t *testing.T) {
	src, _ := testCfg().EffectiveSource()
	args := BuildArgs(testCfg(), src)
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"-rtsp_transport tcp",
		"-map 0:v:0",
		"-map 0:a:0", // first audio track, whatever codec (some SDP sessions expose Opus only)
		"-c:v copy",
		"-c:a aac", // RTMP/FLV carries AAC only; transcode Opus/AAC -> AAC
		"-f flv",
		"-progress pipe:1",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "-vf") {
		t.Errorf("overlay disabled but -vf present:\n%s", joined)
	}
	if last := args[len(args)-1]; last != "rtmp://a.rtmp.youtube.com/live2/SEKRIT" {
		t.Errorf("output URL = %q", last)
	}
}

func TestBuildArgsWithOverlay(t *testing.T) {
	cfg := testCfg()
	cfg.Overlay.Enabled = true
	src, _ := cfg.EffectiveSource()
	args := BuildArgs(cfg, src)
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"-vf drawbox=",
		"drawtext=fontfile=/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
		"textfile=/run/vhfcam-restream/overlay/az.txt:reload=1",
		"fontcolor=red", // TX indicator
		"-c:v libx264",
		"-preset superfast",
		"-pix_fmt yuv420p",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("overlay args missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "-c:v copy") {
		t.Errorf("overlay enabled but video still copied:\n%s", joined)
	}
}

func TestBuildPreviewArgs(t *testing.T) {
	cfg := testCfg()
	cfg.Preview.Enabled = true
	src, _ := cfg.EffectiveSource()
	args := BuildPreviewArgs(cfg, src)
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"-rtsp_transport tcp",
		"-map 0:v:0",
		"-map 0:a:0",
		"-c:v copy", // overlay off in the default test config: copy, no transcode
		"-c:a aac",
		"-f hls",
		"-hls_time 2",
		"-hls_list_size 6",
		"-hls_flags delete_segments+temp_file",
		"-hls_segment_filename /run/vhfcam-restream/preview/seg_%05d.ts",
		"/run/vhfcam-restream/preview/live.m3u8",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("preview args missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "-f flv") {
		t.Errorf("preview args must not carry the YouTube output:\n%s", joined)
	}

	// With the overlay on, the preview transcodes too (burn-in required).
	cfg.Overlay.Enabled = true
	joined = strings.Join(BuildPreviewArgs(cfg, src), " ")
	if !strings.Contains(joined, "-c:v libx264") || !strings.Contains(joined, "-vf drawbox=") {
		t.Errorf("overlay-enabled preview args missing transcode+filter:\n%s", joined)
	}
}

func TestRedactArgs(t *testing.T) {
	src, _ := testCfg().EffectiveSource()
	args := RedactArgs(BuildArgs(testCfg(), src), src, "SEKRIT")
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "SEKRIT") || strings.Contains(joined, "TOKEN1") {
		t.Errorf("secrets leaked in redacted args:\n%s", joined)
	}
	if !strings.Contains(joined, "[redacted]") {
		t.Errorf("no redaction marker present:\n%s", joined)
	}
}

func TestRedactURL(t *testing.T) {
	if got := RedactURL("rtsps://192.168.0.1:7441/TOKEN1?enableSrtp"); got != "rtsps://192.168.0.1:7441/[redacted]" {
		t.Errorf("RedactURL source = %q", got)
	}
	if got := RedactURL("rtmp://a.rtmp.youtube.com/live2/SEKRIT"); got != "rtmp://a.rtmp.youtube.com/[redacted]" {
		t.Errorf("RedactURL target = %q", got)
	}
	if got := RedactURL("::not a url"); got != "[unparseable-url]" {
		t.Errorf("RedactURL garbage = %q", got)
	}
}
