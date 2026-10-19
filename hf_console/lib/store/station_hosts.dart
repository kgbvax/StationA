/// Where each slot's bridge runs — the hosts row of the station schematic.
///
/// Mirrors the `[[software]]` table of
/// `../stationportal/internal/inventory/inventory.toml` (component → host,
/// slots), which is the source of truth: when a service moves host, edit the
/// inventory and this map in the same change. `station_hosts_test.dart`
/// cross-checks the two, so a stale entry fails `flutter test`.
library;

/// Host value for slots whose bridge is the device's own firmware (ESPHome,
/// M5 Stamp PLCs): no host process to count.
const embeddedHost = 'embedded';

class SlotHost {
  /// The repo component that owns the slot (`flexbridge`, …).
  final String component;

  /// [stationHosts] key, or [embeddedHost].
  final String host;

  const SlotHost(this.component, this.host);
}

const slotHosts = <String, SlotHost>{
  'muehle/power/psu-13v8': SlotHost('shelly-power-bridge', 'scmino'),
  'muehle/power/antennas': SlotHost('shelly-power-bridge', 'scmino'),
  'muehle/power/shack-pc-mains': SlotHost('shelly-power-bridge', 'scmino'),
  'muehle/power/shack-pc': SlotHost('winpc-power-bridge', 'bwpc'),
  'muehle/hf/radio': SlotHost('flexbridge', 'scmino'),
  'muehle/hf/ant-ctrl': SlotHost('ultrabridge', 'scmino'),
  'muehle/hf/pa': SlotHost('acom1200s-pa-bridge', 'scmino'),
  'muehle/hf/rotator': SlotHost('wrc-rotator-bridge', 'scmino'),
  'muehle/hf/tuner': SlotHost('atr1k-tuner-bridge', 'scmino'),
  'muehle/hf/antenna-select': SlotHost('antennaselect', 'scmino'),
  'muehle/hf/beam-steer': SlotHost('beamsteer', 'scmino'),
  'muehle/hf/power-seq': SlotHost('powerseq', 'scmino'),
  'muehle/hf/discovery': SlotHost('hadiscovery', 'scmino'),
  'muehle/hf/spots': SlotHost('logger-spot-bridge', 'scmino'),
  'muehle/uhf/radio': SlotHost('icom9700-radio-bridge', 'scmino'),
  'muehle/uhf/sat-track': SlotHost('oscarwatch-sattrack-bridge', 'scmino'),
  'muehle/uhf/az-rotator': SlotHost('spid-ercm-rotator-bridge', 'shari'),
  'muehle/uhf/el-rotator': SlotHost('spid-ercm-rotator-bridge', 'shari'),
  'muehle/hf/ant-switch': SlotHost('waveshare_relay-antswitch-bridge', embeddedHost),
  'muehle/hf/switch': SlotHost('m5stamp-hf-ctrl', embeddedHost),
  'muehle/hf/pa-arm': SlotHost('m5stamp-hf-ctrl', embeddedHost),
  'muehle/uhf/pol-ctrl': SlotHost('m5stamp-pol-ctrl', embeddedHost),
};

class StationHost {
  final String name;
  final String kind;
  final String address;
  final String role;

  const StationHost(this.name, this.kind, this.address, this.role);
}

/// The hosts drawn on the schematic, in drawing order. Addresses follow the
/// inventory's `[[host]]` table (cross-checked); roles are a short summary.
const stationHosts = <String, StationHost>{
  'bwpc': StationHost('bwpc', 'Windows', '192.168.1.197', 'Loggers, OscarWatch, PstRotator'),
  'scmino': StationHost('scmino', 'Pi CM5', '192.168.1.178', 'Station bridges, logic slots, broker mirror'),
  'hassio': StationHost('hassio', 'Broker', '192.168.1.50', 'Home Assistant + the live station broker'),
  'shari': StationHost('shari', 'Pi 4', '192.168.1.139', 'Sat rotator bridge, HamClock, SvxLink'),
};
