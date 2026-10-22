import 'ant_mode.dart';
import 'bus_store.dart';
import 'station_hosts.dart';
import 'wiring.dart' show optionalSlots;

/// How far a slot's displayed values can be trusted.
enum SlotQuality {
  /// Bridge online, device reachable (or a logic slot with a snapshot).
  ok,

  /// Never heard from this session — no snapshot to draw.
  noData,

  /// Bridge process down (/status not `online`).
  down,

  /// Bridge up, device unreachable (/state.device_online false).
  lost,

  /// Deliberately without power (antenna plug off, standby, PSU off, PA
  /// remote-on off, the shack PC asleep): quiet by design, not faulty.
  unpowered,

  /// Our own broker link is down: every value is last-known.
  stale,
}

enum Severity { none, warn, alarm }

const psuSlot = 'muehle/power/psu-13v8';
const antennaPlugSlot = 'muehle/power/antennas';
const pcMainsSlot = 'muehle/power/shack-pc-mains';
const shackPcSlot = 'muehle/power/shack-pc';
const powerSeqSlot = 'muehle/hf/power-seq';
const paSlot = 'muehle/hf/pa';
const hfSwitchSlot = 'muehle/hf/switch';
const uhfRadioSlot = 'muehle/uhf/radio';

/// Slots behind the `muehle/power/antennas` plug (HF + UHF rotators and the
/// Ultrabeam controller). They go offline by design in standby.
const antPoweredSlots = {
  'muehle/hf/ant-ctrl',
  'muehle/hf/rotator',
  'muehle/uhf/az-rotator',
  'muehle/uhf/el-rotator',
};

/// Slots fed by the 13.8 V TRX PSU. A confirmed-off PSU explains their
/// silence. The IC-9700 is fed too; its session slot reports a deaf radio
/// through `radio_responding` instead (see [StationSchematicModel.uhfRadioDeaf]).
const psuPoweredSlots = {
  'muehle/hf/radio',
  'muehle/hf/tuner',
  'muehle/hf/switch',
  'muehle/hf/pa-arm',
  'muehle/hf/ant-switch',
};

/// SWR limits — the same ones the tuner panel colours its suffix with.
const swrWarn = 2.0;
const swrAlarm = 3.0;

/// Per-bridge liveness on one host.
class HostHealth {
  /// component → any of its slots has /status online.
  final Map<String, bool> components;

  const HostHealth(this.components);

  int get total => components.length;
  int get up => components.values.where((v) => v).length;
}

/// Read-only view of the bus for the station schematic: one quality and
/// severity per slot, the power-path facts the wires are drawn from, and
/// per-host bridge health.
///
/// Liveness follows the faults bar (two-layer rule, silent slots after the
/// connect grace, the standby / antenna-plug suppression, the sleeping PC,
/// the idle IC-9700 session, the optional sat tracker) and goes one step
/// further: a device whose feed is confirmed off — 13.8 V PSU off, or the
/// ACOM's remote-on relay off — reads as unpowered. The fault list still
/// lists those devices, under its own PSU OFF root-cause line.
class StationSchematicModel {
  final BusStore store;

  StationSchematicModel(this.store);

  bool get linkUp => store.linkUp;

  T? value<T>(String address, String key) => store.stateValueAs<T>(address, key);

  /// A plug's relay as reported by a live plug: true/false, or null when the
  /// plug bridge or the Shelly is down or the key is missing — unknown, never
  /// assumed either way.
  bool? plugPower(String slot) {
    if (!(store.slots[slot]?.isOnline ?? false)) return null;
    return switch (value<String>(slot, 'power')) { 'on' => true, 'off' => false, _ => null };
  }

  bool? get psuPower => plugPower(psuSlot);
  bool? get antennaPower => plugPower(antennaPlugSlot);
  bool? get pcMainsPower => plugPower(pcMainsSlot);

  bool get psuConfirmedOff => psuPower == false;

  /// Antenna side deliberately unpowered: standby, or the plug confirmed off.
  bool get antennaUnpowered => value<String>(powerSeqSlot, 'phase') == 'standby' || antennaPower == false;

  /// The ACOM's remote-on is off: PLC #1 reports the PA relay off, or the
  /// PSU that feeds PLC #1 is off.
  bool get paRemoteOff =>
      psuConfirmedOff || ((store.slots[hfSwitchSlot]?.isOnline ?? false) && value<String>(hfSwitchSlot, 'pa') == 'off');

  /// The IC-9700 monitor is armed but the radio does not answer — the UHF
  /// panel's STANDBY tag.
  bool get uhfRadioDeaf =>
      value<bool>(uhfRadioSlot, 'monitor') == true && value<bool>(uhfRadioSlot, 'radio_responding') == false;

  /// A carrier is up on the HF path (radio tx, tune carrier, or PA keyed).
  bool get carrier => store.rfState != RfState.rx;

  SlotQuality quality(String address) {
    if (!linkUp) return SlotQuality.stale;
    final s = store.slots[address];
    // The bwpc agent's LWT goes offline whenever the PC sleeps.
    if (address == shackPcSlot && s?.status == 'offline') return SlotQuality.unpowered;
    if (s == null) return SlotQuality.noData;
    if (!s.bridgeOnline) {
      // Firmware-as-bridge slots (PLC #1, the ESPHome switch) lose their
      // LWT together with the 13.8 V that feeds them.
      if (psuPoweredSlots.contains(address) && psuConfirmedOff && slotHosts[address]?.host == embeddedHost) {
        return SlotQuality.unpowered;
      }
      return SlotQuality.down;
    }
    if (s.sessionSlot) {
      return address == uhfRadioSlot && uhfRadioDeaf && psuConfirmedOff ? SlotQuality.unpowered : SlotQuality.ok;
    }
    // The optional tracker may simply be closed.
    if (optionalSlots.contains(address)) return SlotQuality.ok;
    if (!s.deviceOnline) {
      if (antPoweredSlots.contains(address) && antennaUnpowered) return SlotQuality.unpowered;
      if (psuPoweredSlots.contains(address) && psuConfirmedOff) return SlotQuality.unpowered;
      if (address == paSlot && paRemoteOff) return SlotQuality.unpowered;
      return SlotQuality.lost;
    }
    return SlotQuality.ok;
  }

  Severity severity(String address) {
    switch (quality(address)) {
      case SlotQuality.stale:
      case SlotQuality.unpowered:
        return Severity.none;
      case SlotQuality.down:
      case SlotQuality.lost:
        return Severity.alarm;
      case SlotQuality.noData:
        // Silent expected slots surface after the connect grace, exactly
        // when the faults bar lists them.
        return store.offlineList.any((e) => e.startsWith('$address:')) ? Severity.alarm : Severity.none;
      case SlotQuality.ok:
        break;
    }
    if (!optionalSlots.contains(address) && problemText(address) != null) return Severity.alarm;
    switch (address) {
      case 'muehle/hf/tuner':
        return _swrSeverity(value<num>(address, 'swr')?.toDouble());
      case paSlot:
        // Reflected power only means something while the amp is keyed.
        if (value<String>(address, 'keyed') != 'tx') return Severity.none;
        return _swrSeverity(value<num>(address, 'swr')?.toDouble());
      case 'muehle/hf/antenna-select':
        // Manual hold: the reconciler stands down until released.
        return value<String>(address, 'mode') == 'manual' ? Severity.warn : Severity.none;
      case antModeSlot:
        // Storm / maintenance lock or a pending storm suggestion: a
        // deliberate hold or a question to the operator, like manual hold.
        final a = store.antMode;
        return a.locked || a.stormSuggested ? Severity.warn : Severity.none;
    }
    return Severity.none;
  }

  static Severity _swrSeverity(double? swr) {
    if (swr == null) return Severity.none;
    if (swr >= swrAlarm) return Severity.alarm;
    if (swr >= swrWarn) return Severity.warn;
    return Severity.none;
  }

  /// The device-reported fault or error, upper-cased — the same text the
  /// faults bar records ([BusStore] fault history rule). Null when clear.
  String? problemText(String address) {
    final state = store.slots[address]?.state;
    if (state == null) return null;
    String? str(dynamic v) => v?.toString().trim();
    final fault = str(state['fault']);
    final error = str(state['error']);
    if (error != null && error.isNotEmpty && error.toUpperCase() != 'NONE') return error.toUpperCase();
    if (fault != null && fault.isNotEmpty && fault != 'none') return fault.toUpperCase();
    return null;
  }

  /// Whether [problemText] comes from the `fault` field (a device-declared
  /// fault) rather than a bridge `error`.
  bool hasFault(String address) {
    final f = store.slots[address]?.state?['fault']?.toString().trim();
    return f != null && f.isNotEmpty && f != 'none';
  }

  /// When this slot's liveness last changed (best effort, local clock).
  DateTime? lastChange(String address) {
    final s = store.slots[address];
    if (s == null) return null;
    final a = s.statusChangedAt, b = s.deviceChangedAt;
    if (a == null) return b;
    if (b == null) return a;
    return a.isAfter(b) ? a : b;
  }

  HostHealth hostHealth(String host) {
    final components = <String, bool>{};
    slotHosts.forEach((address, sh) {
      if (sh.host != host) return;
      final online = store.slots[address]?.bridgeOnline ?? false;
      components[sh.component] = (components[sh.component] ?? false) || online;
    });
    return HostHealth(components);
  }
}
