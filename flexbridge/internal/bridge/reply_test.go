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

// recordedSlice0 is slice 0's status as the FLEX-8400 (SmartSDR 4.3.16)
// reported it on 2026-11-14, trimmed to the fields the bridge reads.
const recordedSlice0 = "S1BD32545|slice 0 in_use=1 sample_rate=24000 RF_frequency=21.107300 client_handle=0x47C05DDB index_letter=A rxant=ANT1 mode=USB filter_lo=100 filter_hi=2800 agc_mode=med pan=0x40000000 txant=ANT1 lock=0 tx=1 active=1"

func feedRecordedSlice0(t *testing.T, b *Bridge) {
	t.Helper()
	f, err := flexradio.ParseFrame(recordedSlice0)
	if err != nil {
		t.Fatal(err)
	}
	b.HandleStatus(f)
}

// The radio does not echo the status of a tune to the client that sent it
// (recorded 2026-11-14: a client's own `slice tune` got R1|0| and no slice
// status; other clients saw the move). An accepted tune must therefore move
// /state by itself, or set_freq looks like a no-op to every /state consumer.
func TestHandleReply_AcceptedTuneUpdatesState(t *testing.T) {
	b, pub := newTestBridge(t)
	feedRecordedSlice0(t, b)

	// What the radio sends back for the tune: R<seq>|0| and nothing else.
	b.HandleReply(flexradio.Reply{Seq: 5, Command: "slice tune 0 21.108300 autopan=1"})
	snap, ok := lastState(pub.Messages(), testStateTopic)
	if !ok || snap.FreqHz != 21_108_300 || snap.Band != "15m" {
		t.Fatalf("after accepted tune: state=%+v ok=%v, want freq_hz 21108300 on 15m", snap, ok)
	}

	// Band change through a tune: 14.280 MHz, the 2026-11-14 16:38 case.
	b.HandleReply(flexradio.Reply{Seq: 6, Command: "slice tune 0 14.280000 autopan=1"})
	snap, _ = lastState(pub.Messages(), testStateTopic)
	if snap.FreqHz != 14_280_000 || snap.Band != "20m" {
		t.Errorf("after tune to 20m: freq_hz=%d band=%q, want 14280000 20m", snap.FreqHz, snap.Band)
	}

	// An accepted mode change is applied the same way.
	b.HandleReply(flexradio.Reply{Seq: 7, Command: "slice set 0 mode=CW"})
	snap, _ = lastState(pub.Messages(), testStateTopic)
	if snap.Mode != "cw" {
		t.Errorf("after accepted set mode: mode=%q, want cw", snap.Mode)
	}
}

// A refused tune, or a tune of a slice the bridge does not track, must not
// move /state or create a slice.
func TestHandleReply_RefusedOrUnknownTuneLeavesState(t *testing.T) {
	b, pub := newTestBridge(t)
	feedRecordedSlice0(t, b)

	b.HandleReply(flexradio.Reply{Seq: 5, Code: 0x50000016, Command: "slice tune 0 21.108300 autopan=1"})
	b.HandleReply(flexradio.Reply{Seq: 6, Command: "slice tune 3 7.150000 autopan=1"})
	snap, _ := lastState(pub.Messages(), testStateTopic)
	if snap.FreqHz != 21_107_300 {
		t.Errorf("freq_hz = %d, want the reported 21107300", snap.FreqHz)
	}
	b.mu.RLock()
	_, phantom := b.slices[3]
	b.mu.RUnlock()
	if phantom {
		t.Error("an accepted tune of an untracked slice created slice 3")
	}
}

// End to end through set_freq: the bridge sends the tune, the fake radio
// accepts it, and /state follows without any slice status from the radio.
func TestSetFreq_StateFollowsAcceptedTune(t *testing.T) {
	b, pub := newTestBridge(t)
	fc := &replyingCommander{}
	b.SetCommander(fc)
	feedRecordedSlice0(t, b)
	fc.b = b

	b.HandleCommand([]byte(`{"action":"set_freq","value":"21108300"}`))
	snap, _ := lastState(pub.Messages(), testStateTopic)
	if snap.FreqHz != 21_108_300 {
		t.Errorf("freq_hz = %d after set_freq, want 21108300", snap.FreqHz)
	}
}

// replyingCommander accepts every tune the way the FLEX-8400 does: code 0,
// no slice status for the commanding client.
type replyingCommander struct {
	fakeCommander
	b   *Bridge
	seq int
}

func (r *replyingCommander) TuneSlice(slice int, freqHz int64) error {
	r.seq++
	r.b.HandleReply(flexradio.Reply{Seq: r.seq, Command: fmt.Sprintf("slice tune %d %.6f autopan=1", slice, float64(freqHz)/1e6)})
	return nil
}
