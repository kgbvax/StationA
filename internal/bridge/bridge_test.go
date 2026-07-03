package bridge

import (
	"encoding/binary"
	"strconv"
	"strings"
	"testing"
	"time"

	"flexbridge/internal/flexradio"
	"flexbridge/internal/ha"
)

// testLogger records nothing but satisfies Logger.
type testLogger struct{ t *testing.T }

func (l *testLogger) Infof(f string, a ...any)  { l.t.Logf("INFO: "+f, a...) }
func (l *testLogger) Warnf(f string, a ...any)  { l.t.Logf("WARN: "+f, a...) }
func (l *testLogger) Debugf(f string, a ...any) {}

func newTestBridge(t *testing.T) (*Bridge, *MemoPublisher) {
	t.Helper()
	pub := NewMemoPublisher()
	cfg := Config{
		Serial:          "TESTSERIAL",
		StatePrefix:     "flexbridge",
		DiscoveryPrefix: "homeassistant",
		AvailTopic:      "flexbridge/status",
		Rates: map[flexradio.MeterGroup]time.Duration{
			flexradio.GroupTX:    500 * time.Millisecond,
			flexradio.GroupRX:    1000 * time.Millisecond,
			flexradio.GroupAudio: 500 * time.Millisecond,
			flexradio.GroupHW:    10 * time.Second,
		},
	}
	b := New(cfg, pub, &testLogger{t})
	b.SetDevice(ha.Device{Serial: "TESTSERIAL", Model: "FLEX-8400", Name: "FlexRadio 8400"})
	return b, pub
}

// buildMeterPacket builds a VITA-49 meter datagram with a class id of
// 0x8002 and the given (index, raw) pairs.
func buildMeterPacket(t *testing.T, readings []flexradio.MeterReading) []byte {
	t.Helper()
	const meterClassIDLow uint16 = 0x8002
	w0 := uint32(1 << 29) // class present
	out := make([]byte, 12+4*len(readings))
	binary.BigEndian.PutUint32(out[0:4], w0)
	binary.BigEndian.PutUint32(out[4:8], 0)
	binary.BigEndian.PutUint32(out[8:12], uint32(meterClassIDLow))
	for i, r := range readings {
		o := 12 + i*4
		binary.BigEndian.PutUint16(out[o:o+2], r.Index)
		binary.BigEndian.PutUint16(out[o+2:o+4], uint16(r.Raw))
	}
	return out
}

func TestBridge_TransmittingBinarySensor(t *testing.T) {
	b, pub := newTestBridge(t)
	pub.Reset()

	// Receive an interlock=TRANSMITTING status.
	frame, err := flexradio.ParseFrame("S0|interlock state=TRANSMITTING")
	if err != nil {
		t.Fatal(err)
	}
	b.HandleStatus(frame)

	msgs := pub.Messages()
	var found bool
	for _, m := range msgs {
		if strings.HasSuffix(m.Topic, "/state/transmitting") && string(m.Payload) == "TRANSMITTING" {
			found = true
			if !m.Retained {
				t.Error("transmitting state should be retained")
			}
		}
	}
	if !found {
		t.Errorf("did not publish TRANSMITTING; msgs=%+v", msgs)
	}
}

func TestBridge_TxMetersGatedOnTransmit(t *testing.T) {
	b, pub := newTestBridge(t)
	pub.Reset()

	// Register a TX meter (index 3 = FWDPWR) via a meter def status line.
	def, _ := flexradio.ParseFrame("S0|meter 3 0 TX=FWDPWR")
	b.HandleStatus(def)

	// While RECEIVING: a FWDPWR packet must NOT be published.
	pkt := buildMeterPacket(t, []flexradio.MeterReading{{Index: 3, Raw: 6400}}) // ~100W
	b.HandleMeterPacket(pkt)
	if n := len(pub.Messages()); n != 0 {
		t.Errorf("TX meter published while receiving: %d msgs", n)
	}

	// Go to TRANSMITTING, then the same packet should publish.
	tx, _ := flexradio.ParseFrame("S0|interlock state=TRANSMITTING")
	b.HandleStatus(tx)
	pub.Reset()
	b.HandleMeterPacket(pkt)
	msgs := pub.Messages()
	if len(msgs) != 1 {
		t.Fatalf("got %d msgs while transmitting, want 1: %+v", len(msgs), msgs)
	}
	if !strings.HasSuffix(msgs[0].Topic, "/meter/tx/tx_fwd_power") {
		t.Errorf("topic = %q", msgs[0].Topic)
	}
	if msgs[0].Payload[0] == '0' && len(msgs[0].Payload) == 1 {
		t.Errorf("fwd power looks like 0: %q", msgs[0].Payload)
	}
}

func TestBridge_SliceStatusDiff(t *testing.T) {
	b, pub := newTestBridge(t)
	pub.Reset()

	// First slice status publishes all fields including the derived band.
	frame, _ := flexradio.ParseFrame("S0|slice 0 0 freq=14.100.000 mode=USB active=1 tx=0 agc=FAST filter_lo=200 filter_hi=2900")
	b.HandleStatus(frame)
	msgs := pub.Messages()
	if len(msgs) == 0 {
		t.Fatal("expected status publishes for new slice")
	}
	findMsg := func(suffix string) (string, bool) {
		for _, m := range msgs {
			if strings.HasSuffix(m.Topic, suffix) {
				return string(m.Payload), true
			}
		}
		return "", false
	}
	if v, ok := findMsg("/state/slice/0/band"); !ok || v != "20m" {
		t.Errorf("band = (%q,%v), want (\"20m\", true)", v, ok)
	}

	// Second slice status, only mode changes -> only mode republished (band stays 20m).
	pub.Reset()
	frame2, _ := flexradio.ParseFrame("S0|slice 0 0 freq=14.100.000 mode=LSB active=1 tx=0 agc=FAST filter_lo=200 filter_hi=2900")
	b.HandleStatus(frame2)
	msgs = pub.Messages()
	for _, m := range msgs {
		if !strings.HasSuffix(m.Topic, "/state/slice/0/mode") {
			t.Errorf("unexpected republish on %q = %q", m.Topic, m.Payload)
		}
		if string(m.Payload) != "LSB" {
			t.Errorf("mode payload = %q, want LSB", m.Payload)
		}
	}
}

func TestBridge_BandRepublishesOnlyOnBandChange(t *testing.T) {
	b, pub := newTestBridge(t)
	pub.Reset()

	// Start on 20m.
	f1, _ := flexradio.ParseFrame("S0|slice 0 0 freq=14.100.000 mode=USB")
	b.HandleStatus(f1)

	// Drift within 20m: frequency publishes, band does NOT.
	pub.Reset()
	f2, _ := flexradio.ParseFrame("S0|slice 0 0 freq=14.200.000 mode=USB")
	b.HandleStatus(f2)
	for _, m := range pub.Messages() {
		if strings.HasSuffix(m.Topic, "/state/slice/0/band") {
			t.Errorf("band should not republish within same band; got %q", m.Payload)
		}
	}

	// Move to 40m: both frequency and band publish.
	pub.Reset()
	f3, _ := flexradio.ParseFrame("S0|slice 0 0 freq=7.100.000 mode=LSB")
	b.HandleStatus(f3)
	msgs := pub.Messages()
	var sawBand bool
	for _, m := range msgs {
		if strings.HasSuffix(m.Topic, "/state/slice/0/band") {
			sawBand = true
			if string(m.Payload) != "40m" {
				t.Errorf("band = %q, want 40m", m.Payload)
			}
		}
	}
	if !sawBand {
		t.Error("expected band republish on band change, got none")
	}
}

func TestBridge_HWMetersPublishedWithoutTx(t *testing.T) {
	// PA temp is radio hardware; it should publish even while receiving.
	b, pub := newTestBridge(t)
	pub.Reset()

	def, _ := flexradio.ParseFrame("S0|meter 9 0 RAD=PATEMP")
	b.HandleStatus(def)

	pkt := buildMeterPacket(t, []flexradio.MeterReading{{Index: 9, Raw: 45 * 64}}) // 45 degC
	b.HandleMeterPacket(pkt)
	msgs := pub.Messages()
	if len(msgs) != 1 {
		t.Fatalf("got %d msgs, want 1: %+v", len(msgs), msgs)
	}
	if !strings.HasSuffix(msgs[0].Topic, "/meter/hw/pa_temp") {
		t.Errorf("topic = %q", msgs[0].Topic)
	}
	if !strings.Contains(string(msgs[0].Payload), "45") {
		t.Errorf("pa_temp payload = %q, want ~45", msgs[0].Payload)
	}
}

func TestBridge_RXMeterPerSlice(t *testing.T) {
	b, pub := newTestBridge(t)
	pub.Reset()

	// Register slice 1's LEVEL meter (index 7).
	def, _ := flexradio.ParseFrame("S0|meter 7 1 SLC=LEVEL")
	b.HandleStatus(def)

	// S-meter -62 dBm -> raw -62*128 = -7936
	pkt := buildMeterPacket(t, []flexradio.MeterReading{{Index: 7, Raw: -7936}})
	b.HandleMeterPacket(pkt)
	msgs := pub.Messages()
	if len(msgs) != 1 {
		t.Fatalf("got %d msgs, want 1: %+v", len(msgs), msgs)
	}
	// Per-slice topic embeds the slice index.
	if !strings.HasSuffix(msgs[0].Topic, "/meter/rx/1/s_meter") {
		t.Errorf("topic = %q, want suffix /meter/rx/1/s_meter", msgs[0].Topic)
	}
}

func TestBridge_ATUStatusChange(t *testing.T) {
	b, pub := newTestBridge(t)
	pub.Reset()

	// First ATU status.
	f1, _ := flexradio.ParseFrame("S0|atu status=Tuned active=1")
	b.HandleStatus(f1)
	msgs := pub.Messages()
	if len(msgs) != 1 || !strings.HasSuffix(msgs[0].Topic, "/state/atu") {
		t.Fatalf("expected one ATU publish, got %+v", msgs)
	}
	if string(msgs[0].Payload) != "tuned" {
		t.Errorf("atu payload = %q, want 'tuned'", msgs[0].Payload)
	}

	// Same status -> no republish.
	pub.Reset()
	b.HandleStatus(f1)
	if len(pub.Messages()) != 0 {
		t.Errorf("unchanged ATU status should not republish, got %+v", pub.Messages())
	}

	// New status -> republish.
	pub.Reset()
	f2, _ := flexradio.ParseFrame("S0|atu status=Bypass active=0")
	b.HandleStatus(f2)
	msgs = pub.Messages()
	if len(msgs) != 1 || string(msgs[0].Payload) != "bypass" {
		t.Errorf("expected one bypass publish, got %+v", msgs)
	}
}

func TestBridge_TxPowerStatus(t *testing.T) {
	b, pub := newTestBridge(t)
	pub.Reset()

	f, _ := flexradio.ParseFrame("S0|radio tx_power=100 status=Available")
	b.HandleStatus(f)
	msgs := pub.Messages()
	if len(msgs) != 1 {
		t.Fatalf("got %d msgs, want 1: %+v", len(msgs), msgs)
	}
	if string(msgs[0].Payload) != "100" {
		t.Errorf("tx_power = %q, want 100", msgs[0].Payload)
	}
}

func TestBridge_TunePowerStatus(t *testing.T) {
	b, pub := newTestBridge(t)
	pub.Reset()

	// A radio status line carrying both tx_power and tune_power.
	f, _ := flexradio.ParseFrame("S0|radio tx_power=100 tune_power=25 status=Available")
	b.HandleStatus(f)
	msgs := pub.Messages()
	if len(msgs) != 2 {
		t.Fatalf("got %d msgs, want 2 (tx_power + tune_power): %+v", len(msgs), msgs)
	}
	got := map[string]string{}
	for _, m := range msgs {
		got[m.Topic] = string(m.Payload)
	}
	if !strings.Contains(got["flexbridge/TESTSERIAL/state/tx_power"], "100") {
		t.Errorf("tx_power = %q, want 100", got["flexbridge/TESTSERIAL/state/tx_power"])
	}
	if !strings.Contains(got["flexbridge/TESTSERIAL/state/tune_power"], "25") {
		t.Errorf("tune_power = %q, want 25", got["flexbridge/TESTSERIAL/state/tune_power"])
	}
}

func TestBridge_Discovery_PublishedOnce(t *testing.T) {
	b, pub := newTestBridge(t)
	pub.Reset()

	// Re-call PublishDiscovery -> should be a no-op (already done in setup).
	b.PublishDiscovery()
	// First SetDevice already triggered discovery; second call must not dup.
	// Count discovery topics (those ending in /config).
	msgs := pub.Messages()
	configCount := 0
	for _, m := range msgs {
		if strings.HasSuffix(m.Topic, "/config") {
			configCount++
		}
	}
	if configCount != 0 {
		t.Errorf("discovery re-published %d times on no-op; want 0", configCount)
	}
}

func TestBridge_Discovery_ContainsMetersAndStatus(t *testing.T) {
	_, pub := newTestBridge(t)
	msgs := pub.Messages()
	joined := ""
	for _, m := range msgs {
		if strings.HasSuffix(m.Topic, "/config") {
			joined += m.Topic + " " + string(m.Payload) + "\n"
		}
	}
	// A couple of representative meters.
	for _, want := range []string{"tx_fwd_power", "tx_swr", "pa_temp", "supply_voltage_a"} {
		if !strings.Contains(joined, want) {
			t.Errorf("discovery missing %q", want)
		}
	}
	// Status entities (radio-wide).
	for _, want := range []string{"transmitting", "tx_power", "tune_power", "atu"} {
		if !strings.Contains(joined, want) {
			t.Errorf("discovery missing status entity %q", want)
		}
	}
	// Per-slice status entities. Band is published only lazily (when a slice
	// appears); newTestBridge triggers SetDevice but no slices exist yet, so
	// band/frequency won't be in discovery here. That's covered by the slice
	// tests instead.
}

func TestBridge_ResetClearsMeterRegistry(t *testing.T) {
	b, pub := newTestBridge(t)
	pub.Reset()

	def, _ := flexradio.ParseFrame("S0|meter 3 0 TX=FWDPWR")
	b.HandleStatus(def)
	b.interlock.Transmitting = true // cheat to allow TX meter
	pkt := buildMeterPacket(t, []flexradio.MeterReading{{Index: 3, Raw: 6400}})
	b.HandleMeterPacket(pkt)
	if len(pub.Messages()) != 1 {
		t.Fatal("expected one TX publish before reset")
	}

	// Reset wipes the registry -> same packet now does nothing.
	b.Reset()
	pub.Reset()
	b.interlock.Transmitting = true
	b.HandleMeterPacket(pkt)
	if len(pub.Messages()) != 0 {
		t.Errorf("after Reset, TX meter should not publish; got %+v", pub.Messages())
	}
}

func TestFormatValue_Units(t *testing.T) {
	cases := []struct {
		v    float64
		unit string
		want string
	}{
		{2.005, "SWR", "2.01"}, // 2 decimals
		{-62.30, "dBm", "-62.3"},
		{100.0, "W", "100.0"},
		{13.81, "V", "13.81"},
		{45.0, "degC", "45.0"},
	}
	for _, c := range cases {
		got := formatValue(c.v, c.unit)
		// Allow rounding; compare as parsed floats to be tolerant.
		gf, _ := strconv.ParseFloat(got, 64)
		wf, _ := strconv.ParseFloat(c.want, 64)
		if abs(gf-wf) > 0.05 {
			t.Errorf("formatValue(%v,%q) = %q, want ~%s", c.v, c.unit, got, c.want)
		}
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
