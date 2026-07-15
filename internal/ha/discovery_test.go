package ha

import (
	"strings"
	"testing"
)

func TestStatusEntity_Frequency(t *testing.T) {
	d := Device{Serial: "S1"}
	cfg, comp := StatusEntity("Frequency", "frequency", "muehle/hf/radio/state", "Hz",
		"{{ value_json.freq_hz }}", d, "")
	if comp != ComponentSensor {
		t.Errorf("comp = %q, want sensor", comp)
	}
	if cfg.DeviceClass != "frequency" {
		t.Errorf("frequency DeviceClass = %q, want frequency", cfg.DeviceClass)
	}
	if cfg.UnitOfMeasurement != "Hz" {
		t.Errorf("unit = %q, want Hz", cfg.UnitOfMeasurement)
	}
	if cfg.ValueTemplate != "{{ value_json.freq_hz }}" {
		t.Errorf("ValueTemplate = %q", cfg.ValueTemplate)
	}
}

func TestStatusEntity_ModeString(t *testing.T) {
	d := Device{Serial: "S1"}
	cfg, _ := StatusEntity("Mode", "mode", "t", "", "{{ value_json.mode }}", d, "")
	if cfg.UnitOfMeasurement != "" {
		t.Errorf("mode unit = %q, want empty", cfg.UnitOfMeasurement)
	}
	if cfg.StateClass != "" {
		t.Errorf("mode StateClass = %q, want empty", cfg.StateClass)
	}
}

func TestBinaryEntity_Transmitting(t *testing.T) {
	d := Device{Serial: "S1"}
	cfg, comp := BinaryEntity("Transmitting", "transmitting", "t",
		"tx", "rx", "{{ value_json.tx }}", d, "")
	if comp != ComponentBinarySensor {
		t.Errorf("comp = %q, want binary_sensor", comp)
	}
	if cfg.PayloadOn != "tx" || cfg.PayloadOff != "rx" {
		t.Errorf("on/off = %q/%q, want tx/rx", cfg.PayloadOn, cfg.PayloadOff)
	}
	if cfg.ValueTemplate != "{{ value_json.tx }}" {
		t.Errorf("ValueTemplate = %q", cfg.ValueTemplate)
	}
}

func TestDeviceInfo_Firmware(t *testing.T) {
	d := Device{Serial: "S1", Model: "FLEX-8400", Firmware: "3.8.19"}
	di := DeviceInfoFor(d)
	if di.SWVersion != "3.8.19" {
		t.Errorf("SWVersion = %q, want 3.8.19", di.SWVersion)
	}
}

func TestConfigTopic(t *testing.T) {
	got := ConfigTopic("homeassistant", "sensor", "flexradio-S1", "pa_temp")
	want := "homeassistant/sensor/flexradio-S1/pa_temp/config"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"1234-5678-8400.12345": "1234-5678-8400_12345",
		"FLEX-8400":            "flex-8400",
		"clean_id":             "clean_id",
	}
	for in, want := range cases {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNodeID(t *testing.T) {
	if got := NodeID("1234-5678"); !strings.HasPrefix(got, "flexradio-") {
		t.Errorf("NodeID = %q, want flexradio- prefix", got)
	}
}
