package bridge

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"flexbridge/internal/flexradio"
)

// recLogger records Warn and Info lines for assertions.
type recLogger struct {
	mu          sync.Mutex
	warns, info []string
}

func (l *recLogger) Infof(f string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.info = append(l.info, fmt.Sprintf(f, a...))
}
func (l *recLogger) Warnf(f string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, fmt.Sprintf(f, a...))
}
func (l *recLogger) Errorf(f string, a ...any) { l.Warnf(f, a...) }
func (l *recLogger) Debugf(string, ...any)     {}

// A tune the radio refuses must be a Warn that carries the command and the
// radio's code, so `journalctl -p warning` shows why the radio did not move.
func TestHandleReply_RefusedTuneWarns(t *testing.T) {
	log := &recLogger{}
	b := New(Config{}, &MemoPublisher{}, log)

	b.HandleReply(flexradio.Reply{Seq: 9, Code: 0x50000016, Message: "not owner", Command: "slice tune 0 21.108300 autopan=1"})
	if len(log.warns) != 1 {
		t.Fatalf("warns = %q, want one", log.warns)
	}
	for _, want := range []string{"slice tune 0 21.108300", "0x50000016", "not owner"} {
		if !strings.Contains(log.warns[0], want) {
			t.Errorf("warn %q lacks %q", log.warns[0], want)
		}
	}

	// An accepted tune is logged at Info; handshake subscriptions are not.
	b.HandleReply(flexradio.Reply{Seq: 10, Command: "slice tune 0 21.107300 autopan=1"})
	b.HandleReply(flexradio.Reply{Seq: 11, Command: "sub slice all"})
	if len(log.info) != 1 || !strings.Contains(log.info[0], "slice tune 0 21.107300") {
		t.Errorf("info = %q, want one accepted-tune line", log.info)
	}

	// A refused optional handshake command is expected on some radios: Info.
	b.HandleReply(flexradio.Reply{Seq: 12, Code: 0x50000015, Command: "sub dvk all"})
	if len(log.warns) != 1 {
		t.Errorf("refused sub dvk all produced a warn: %q", log.warns)
	}
}
