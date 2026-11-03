package main

import (
	"strings"
	"testing"

	"chaseq-ui/internal/config"
	"chaseq-ui/internal/loggers"
)

func TestBuildSendersFallbackListDisabledAndUnknown(t *testing.T) {
	// Legacy keys only: one Log4OM.
	got, err := buildSenders(config.LoggersConfig{Log4OMAddr: "127.0.0.1:2241", Log4OMTimeout: "300ms"})
	if err != nil || len(got) != 1 || got[0].(*loggers.Log4OM).Timeout.Milliseconds() != 300 {
		t.Fatalf("fallback = %v, %v", got, err)
	}
	off := false
	got, err = buildSenders(config.LoggersConfig{Log4OMAddr: "ignored:1", Sender: []config.SenderConfig{
		{Kind: "log4om", Addr: "127.0.0.1:2241"}, {Kind: "log4om", Addr: "127.0.0.1:2251", Enabled: &off},
	}})
	if err != nil || len(got) != 1 || got[0].(*loggers.Log4OM).Addr != "127.0.0.1:2241" {
		t.Fatalf("list = %v, %v", got, err)
	}
	if _, err = buildSenders(config.LoggersConfig{Sender: []config.SenderConfig{{Kind: "nope", Addr: "x:1"}}}); err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Errorf("unknown kind: %v", err)
	}
	if _, err = buildSenders(config.LoggersConfig{Sender: []config.SenderConfig{{Kind: "log4om", Addr: "x:1", Timeout: "soon"}}}); err == nil {
		t.Error("bad timeout accepted")
	}
}
