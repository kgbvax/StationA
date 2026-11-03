package loggers

import (
	"strings"
	"testing"
	"time"
)

func TestBuildLog4OM(t *testing.T) {
	got, err := Build([]Spec{{Kind: "Log4OM", Addr: "127.0.0.1:2241", Timeout: time.Second}})
	if err != nil || len(got) != 1 {
		t.Fatalf("Build = %v, %v", got, err)
	}
	l, ok := got[0].(*Log4OM)
	if !ok || l.Addr != "127.0.0.1:2241" || l.Timeout != time.Second {
		t.Errorf("sender = %+v", got[0])
	}
}

func TestBuildUnknownKind(t *testing.T) {
	_, err := Build([]Spec{{Kind: "log4om", Addr: "a:1"}, {Kind: "dxlog", Addr: "a:2"}})
	if err == nil || !strings.Contains(err.Error(), `unknown kind "dxlog"`) || !strings.Contains(err.Error(), "#2") {
		t.Errorf("err = %v", err)
	}
}

func TestBuildLog4OMNeedsAddr(t *testing.T) {
	if _, err := Build([]Spec{{Kind: "log4om"}}); err == nil {
		t.Error("log4om without addr built")
	}
}

func TestBuildNone(t *testing.T) {
	if got, err := Build(nil); err != nil || len(got) != 0 {
		t.Errorf("Build(nil) = %v, %v", got, err)
	}
}
