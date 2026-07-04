package ha

import (
	"encoding/json"
	"strings"
	"testing"

	"flex2mqtt/internal/flexradio"
)

func TestMeterEntity_FwdPower(t *testing.T) {
	def := flexradio.MeterDef{
		Name: "FWDPWR", PublishUnit: "W", Label: "Forward RF Power",
		Group: flexradio.GroupTX,
	}
	d := Device{Serial: "1234-5678-8400.12345", Model: "FLEX-8400", Name: "FlexRadio 8400"}
	cfg, comp := MeterEntity(def, d, "flex2mqtt/1234/state/tx_fwd_power", "tx_fwd_power", "flex2mqtt/status")

	if comp != ComponentSensor {
		t.Errorf("comp = %q, want sensor", comp)
	}
	if cfg.DeviceClass != "power" {
		t.Errorf("DeviceClass = %q, want power", cfg.DeviceClass)
	}
	if cfg.UnitOfMeasurement != "W" {
		t.Errorf("UnitOfMeasurement = %q, want W", cfg.UnitOfMeasurement)
	}
	if cfg.StateClass != "measurement" {
		t.Errorf("StateClass = %q, want measurement", cfg.StateClass)
	}
	if cfg.UniqueID != "flexradio-1234-5678-8400_12345_tx_fwd_power" {
		t.Errorf("UniqueID = %q", cfg.UniqueID)
	}
	// Device block
	if cfg.Device.Name != "FlexRadio 8400" {
		t.Errorf("Device.Name = %q", cfg.Device.Name)
	}
	if len(cfg.Device.Identifiers) != 1 || cfg.Device.Identifiers[0] != "1234-5678-8400.12345" {
		t.Errorf("Identifiers = %v", cfg.Device.Identifiers)
	}
}

func TestMeterEntity_SWR_NoDeviceClass(t *testing.T) {
	def := flexradio.MeterDef{Name: "SWR", PublishUnit: "SWR", Label: "SWR"}
	d := Device{Serial: "S1"}
	cfg, _ := MeterEntity(def, d, "t", "tx_swr", "")
	if cfg.DeviceClass != "" {
		t.Errorf("SWR DeviceClass = %q, want empty (ratio)", cfg.DeviceClass)
	}
}

func TestStatusEntity_Frequency(t *testing.T) {
	d := Device{Serial: "S1"}
	cfg, comp := StatusEntity("Frequency", "slice_0_frequency", "flex2mqtt/S1/state/slice/0/frequency", "MHz", d, "")
	if comp != ComponentSensor {
		t.Errorf("comp = %q, want sensor", comp)
	}
	// MHz has no valid HA device class (frequency expects Hz), so empty.
	if cfg.DeviceClass != "" {
		t.Errorf("frequency DeviceClass = %q, want empty", cfg.DeviceClass)
	}
	if cfg.UnitOfMeasurement != "MHz" {
		t.Errorf("unit = %q, want MHz", cfg.UnitOfMeasurement)
	}
}

func TestStatusEntity_ModeString(t *testing.T) {
	d := Device{Serial: "S1"}
	cfg, _ := StatusEntity("Mode", "slice_0_mode", "t", "", d, "")
	if cfg.UnitOfMeasurement != "" {
		t.Errorf("mode unit = %q, want empty", cfg.UnitOfMeasurement)
	}
	if cfg.StateClass != "" {
		t.Errorf("mode StateClass = %q, want empty", cfg.StateClass)
	}
}

func TestBinaryEntity_Transmitting(t *testing.T) {
	d := Device{Serial: "S1"}
	cfg, comp := BinaryEntity("Transmitting", "transmitting", "t", "TRANSMITTING", "RECEIVING", d, "")
	if comp != ComponentBinarySensor {
		t.Errorf("comp = %q, want binary_sensor", comp)
	}
	if cfg.PayloadOn != "TRANSMITTING" || cfg.PayloadOff != "RECEIVING" {
		t.Errorf("on/off = %q/%q", cfg.PayloadOn, cfg.PayloadOff)
	}
}

func TestConfigTopic(t *testing.T) {
	got := ConfigTopic("homeassistant", "sensor", "flexradio-S1", "pa_temp")
	want := "homeassistant/sensor/flexradio-S1/pa_temp/config"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDiscoveryJSONRoundtrip(t *testing.T) {
	// Ensure the struct marshals to the JSON keys HA expects.
	def := flexradio.MeterDef{Name: "PATEMP", PublishUnit: "degC", Label: "PA Temperature"}
	d := Device{Serial: "S1"}
	cfg, _ := MeterEntity(def, d, "t", "pa_temp", "")
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, key := range []string{`"unique_id"`, `"state_topic"`, `"unit_of_measurement":"°C"`, `"device_class":"temperature"`, `"state_class":"measurement"`, `"device":{`, `"identifiers"`, `"manufacturer":"FlexRadio"`} {
		if !strings.Contains(s, key) {
			t.Errorf("JSON missing %q in: %s", key, s)
		}
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
