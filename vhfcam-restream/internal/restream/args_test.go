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
		"-map 0:a:m:aac", // must pin the AAC track: default selection would pick Opus, which FLV cannot carry
		"-c:v copy",
		"-c:a copy",
		"-f flv",
		"-progress pipe:1",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q:\n%s", want, joined)
		}
	}
	if last := args[len(args)-1]; last != "rtmp://a.rtmp.youtube.com/live2/SEKRIT" {
		t.Errorf("output URL = %q", last)
	}
	if has := strings.Contains(joined, "TOKEN1"); !has {
		t.Errorf("source URL not in args (sanity): %s", joined)
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
