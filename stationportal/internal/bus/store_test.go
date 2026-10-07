package bus

import (
	"testing"
	"time"
)

func TestAddressOf(t *testing.T) {
	cases := []struct {
		topic, addr, plane string
		ok                 bool
	}{
		{"muehle/hf/radio/meta", "muehle/hf/radio", "meta", true},
		{"muehle/console/status", "muehle/console", "status", true},
		{"muehle/hf/radio/cmd", "", "", false},
		{"other/hf/radio/meta", "", "", false},
		{"muehle/meta", "", "", false},
		{"meta", "", "", false},
	}
	for _, c := range cases {
		a, p, ok := AddressOf("muehle", c.topic)
		if a != c.addr || p != c.plane || ok != c.ok {
			t.Errorf("%s: got %q %q %v", c.topic, a, p, ok)
		}
	}
}

func TestStoreTracksPlanesAndClears(t *testing.T) {
	s := NewStore("muehle")
	now := time.Unix(100, 0)
	s.Update("muehle/hf/pa/meta", []byte(`{"role":"pa","device":{"model":"ACOM 1200S"}}`), now)
	s.Update("muehle/hf/pa/status", []byte("online"), now)
	s.Update("muehle/hf/pa/state", []byte(`{"device_online":false,"swr":1.2}`), now)
	s.Update("muehle/hf/pa/cmd", []byte(`{"action":"x"}`), now)

	snap := s.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("want 1 slot, got %d", len(snap))
	}
	sl := snap[0]
	if sl.Status != "online" || sl.DeviceOnline == nil || *sl.DeviceOnline || MetaFacts(sl.Meta).Model != "ACOM 1200S" {
		t.Fatalf("got %+v", sl)
	}

	// A state without device_online forgets the old value (fields-omitted policy).
	s.Update("muehle/hf/pa/state", []byte(`{"swr":1.1}`), now)
	if s.Snapshot()[0].DeviceOnline != nil {
		t.Fatal("device_online must reset when omitted")
	}

	// Clearing every plane removes the slot.
	s2 := NewStore("muehle")
	s2.Update("muehle/x/y/meta", []byte(`{}`), now)
	s2.Update("muehle/x/y/meta", nil, now)
	if len(s2.Snapshot()) != 0 {
		t.Fatal("cleared slot should disappear")
	}
}

func TestSnapshotIsACopy(t *testing.T) {
	s := NewStore("muehle")
	s.Update("muehle/a/b/state", []byte(`{"device_online":true}`), time.Now())
	snap := s.Snapshot()
	*snap[0].DeviceOnline = false
	if !*s.Snapshot()[0].DeviceOnline {
		t.Fatal("snapshot aliases store state")
	}
}

func TestMetaFacts(t *testing.T) {
	f := MetaFacts(map[string]any{
		"role": "bandmap", "host": "bwpc",
		"device": map[string]any{"name": "Shack logger", "link": "udp-broadcast", "firmware": "1.0.0"},
	})
	if f.Model != "Shack logger" || f.Link != "udp-broadcast" || f.Firmware != "1.0.0" || f.Host != "bwpc" {
		t.Fatalf("got %+v", f)
	}
	f = MetaFacts(map[string]any{"expose": map[string]any{"device": map[string]any{"manufacturer": "Yaesu", "model": "G-450DC"}}})
	if f.Manufacturer != "Yaesu" || f.Model != "G-450DC" {
		t.Fatalf("expose fallback: %+v", f)
	}
	if (MetaFacts(nil) != Facts{}) {
		t.Fatal("nil meta must give empty facts")
	}
}
