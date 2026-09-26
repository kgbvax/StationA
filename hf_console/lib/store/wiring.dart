import 'dart:convert';

// Port→name map. Source of truth is antennaselect/config.example.toml [wiring_map].
// port2 and port3 are not wired at Mühle, so they are omitted from the UI.
const antennaMap = {
  'off': 'GND',
  'port1': 'Dummy',
  'port4': 'Ultrabeam',
  'port5': 'Port 5',
  'port6': 'Fan dipole',
};

// Which /cmd topics are retained per the real bus policy.
const cmdRetain = {
  'muehle/power/master': true,
  'muehle/power/psu-13v8': true,
  'muehle/hf/switch': true,
  'muehle/hf/pa-arm': true,
  'muehle/hf/ant-ctrl': true,
  'muehle/hf/ant-switch': true,
  'muehle/hf/antenna-select': true,
  // beamsteer smart-rotation toggle — retained steady state, so
  // enable/disable survives a beamsteer restart.
  'muehle/hf/beam-steer': true,
  // one-shot
  'muehle/hf/pa': false,
  'muehle/hf/rotator': false,
  'muehle/hf/tuner': false,
  'muehle/hf/power-seq': false,
  'muehle/hf/radio': false, // DVK play/stop are one-shot
  // Sat rotators (spid-ercm-rotator-bridge) — one-shot per KTD13: a stale
  // retained or queued goto must never replay against real antennas.
  'muehle/uhf/az-rotator': false,
  'muehle/uhf/el-rotator': false,
  // pol-ctrl (m5stamp-pol-ctrl) — retained steady state, the actuator
  // exception (KTD13's deliberate contrast with the one-shot rotators
  // above): a retained set_pol re-applies the operator's last intent after
  // a controller reboot or broker reconnect.
  'muehle/uhf/pol-ctrl': true,
  // uhf/radio (icom9700-radio-bridge) — one-shot across the whole action
  // set (receive-only posture, 2026-09: audio_on/audio_off/power_on/
  // monitor_on/monitor_off). A stale queued demand must never re-fire
  // into a fresh session; the bridge clears the topic after every
  // execute-or-reject.
  'muehle/uhf/radio': false,
};

String cmdTopic(String slot) => 'muehle/$slot/cmd';

/// Every deployed slot this console monitors — the HF + power + UHF device
/// slots from the station model (see ../docs/station-integration-model.md;
/// TBD/precision slots and host-liveness nodes are excluded until they
/// exist). The offline list also reports these when the console has never
/// heard anything from them — a dead-since-boot or undeployed service must
/// not be invisible just because it never published a retained state.
const expectedSlots = [
  'muehle/power/master',
  'muehle/power/psu-13v8',
  'muehle/hf/radio',
  'muehle/hf/ant-ctrl',
  'muehle/hf/ant-switch',
  'muehle/hf/switch',
  'muehle/hf/pa-arm',
  'muehle/hf/antenna-select',
  'muehle/hf/beam-steer',
  'muehle/hf/pa',
  'muehle/hf/rotator',
  'muehle/hf/tuner',
  'muehle/hf/power-seq',
  'muehle/hf/discovery',
  'muehle/uhf/rotator',
  'muehle/uhf/pol-ctrl',
  'muehle/uhf/az-rotator',
  'muehle/uhf/el-rotator',
  'muehle/uhf/radio',
];

String cmdPayload(String action, dynamic value) =>
    jsonEncode({'action': action, 'value': value});

// --- Power & station ---------------------------------------------------------

String powerSetPayload(String onOff) => cmdPayload('set_power', onOff);

String powerSeqStartPayload() => jsonEncode({'action': 'start'});
String powerSeqStopPayload() => jsonEncode({'action': 'stop'});

// --- HF switch / PA arm ------------------------------------------------------

String switchSetPaPayload(String onOff) => cmdPayload('set_pa', onOff);
String switchSetTrxPayload(String onOff) => cmdPayload('set_trx', onOff);

// pa-arm.set_enabled value is a **string** "true" / "false".
// No console UI publishes this today (the PA ARM panel was removed — arm
// stays sequencer/automation-owned), but the payload contract is kept here
// with the rest of the slot vocabulary.
String paArmPayload(bool enabled) =>
    cmdPayload('set_enabled', enabled ? 'true' : 'false');

// --- Rotator -----------------------------------------------------------------

String rotatorAzPayload(double az) =>
    jsonEncode({'action': 'set_az', 'az': az});

/// Which rotator a map dial reads, per page. The wrc HF rotator speaks
/// `set_az` with a numeric `az`; the VHF sat az-rotator (spid-ercm-rotator-
/// bridge) speaks the station value-key convention — `goto` with a string
/// degree under `value` (R3). Both are one-shot (KTD13), so every aim
/// publish goes out unretained.
class RotatorSurface {
  /// Full state slot address, e.g. `muehle/hf/rotator`.
  final String stateSlot;

  /// Segment passed to [cmdTopic], e.g. `hf/rotator`.
  final String cmdSlot;

  /// /state key of the commanded azimuth: `target_az` (wrc bridge) vs
  /// `target` (sat bridge).
  final String targetKey;

  /// Whether the direction-preset rail (HF big-DX headings) makes sense for
  /// this rotator. The VHF dial aims by tap only.
  final bool showPresets;

  /// The /cmd payload that aims this rotator at [deg].
  final String Function(double deg) aimPayload;

  /// Every cmd slot the map's STOP / E-STOP halts (the VHF array stops
  /// both axes, like the sat panel's STOP), and the payload it sends.
  final List<String> stopSlots;
  final String Function() stopPayload;

  /// Half the drawn main-lobe width, degrees (visual only).
  final double beamHalfWidthDeg;

  const RotatorSurface(
    this.stateSlot,
    this.cmdSlot, {
    required this.targetKey,
    required this.showPresets,
    required this.aimPayload,
    required this.stopSlots,
    required this.stopPayload,
    required this.beamHalfWidthDeg,
  });
}

const RotatorSurface hfRotator = RotatorSurface(
  'muehle/hf/rotator',
  'hf/rotator',
  targetKey: 'target_az',
  showPresets: true,
  aimPayload: rotatorAzPayload,
  stopSlots: ['hf/rotator'],
  stopPayload: rotatorStopPayload,
  beamHalfWidthDeg: 30,
);

/// The VHF array azimuth: `muehle/uhf/az-rotator` /state keys `az` / `target`
/// / `moving` (el lives on its own slot and is the SatRotatorPanel's business).
const RotatorSurface vhfRotator = RotatorSurface(
  'muehle/uhf/az-rotator',
  'uhf/az-rotator',
  targetKey: 'target',
  showPresets: false,
  aimPayload: satRotatorGotoPayload,
  stopSlots: ['uhf/az-rotator', 'uhf/el-rotator'],
  stopPayload: satRotatorStopPayload,
  // X-Quad main lobe, roughly ±20° at the -3 dB points.
  beamHalfWidthDeg: 20,
);

String rotatorStopPayload() => jsonEncode({'action': 'stop'});
String rotatorFwdPayload() => jsonEncode({'action': 'fwd'});
String rotatorRevPayload() => jsonEncode({'action': 'rev'});

// --- Sat rotators (uhf/az-rotator + uhf/el-rotator) ---------------------------
//
// R3: goto takes degrees under `value` — the station /cmd value-key
// convention, NOT the wrc hf/rotator set_az deviation above. The bridge
// parses the string; both are one-shot (KTD13), so callers pass
// cmdRetain[...]! which is false for both slots.

String satRotatorGotoPayload(double deg) => cmdPayload('goto', deg.toString());

String satRotatorStopPayload() => jsonEncode({'action': 'stop'});

// --- X-Quad polarization (uhf/pol-ctrl) ---------------------------------------
//
// R14/m5stamp-pol-ctrl contract: set_pol takes the canonical phase under
// `value` — the station /cmd value-key convention. The phase is one shared
// setting for BOTH X-Quads; valid vocabulary is h|v|cl|cr. /cmd is RETAINED
// (cmdRetain['muehle/uhf/pol-ctrl']! = true): desired steady state that
// re-applies on reconnect, in deliberate contrast to the one-shot sat
// rotators above (KTD13).

String setPolPayload(String pol) => cmdPayload('set_pol', pol);

// --- UHF radio (muehle/uhf/radio, icom9700-radio-bridge) ----------------------
//
// Receive-only posture (2026-09): the action set is exactly audio_on,
// audio_off, power_on, monitor_on, monitor_off — no value arguments, the
// action name is the whole intent. All are published with
// cmdRetain['muehle/uhf/radio']! = false (one-shot; a stale queued demand
// must never re-fire after a bridge restart). There is no PTT/arm/tuning
// path anymore — remote TX control was removed with the pivot.

String uhfRadioAudioOnPayload() => jsonEncode({'action': 'audio_on'});

String uhfRadioAudioOffPayload() => jsonEncode({'action': 'audio_off'});

String uhfRadioPowerOnPayload() => jsonEncode({'action': 'power_on'});

String uhfRadioMonitorOnPayload() => jsonEncode({'action': 'monitor_on'});

String uhfRadioMonitorOffPayload() => jsonEncode({'action': 'monitor_off'});

// --- Ultrabeam controller ----------------------------------------------------

String antCtrlFrequencyPayload(int freqHz) =>
    jsonEncode({'action': 'frequency', 'freq_hz': freqHz});

String antCtrlDirectionPayload(String direction) =>
    cmdPayload('direction', direction);

String antCtrlBandPayload(String band) => cmdPayload('band', band);

String antCtrlRetractPayload() => jsonEncode({'action': 'retract'});

// --- Smart rotation (beamsteer) ------------------------------------------------

String beamSteerEnablePayload(bool enabled) =>
    jsonEncode({'action': enabled ? 'enable' : 'disable'});

// --- PA ----------------------------------------------------------------------

String paSetModePayload(String mode) => cmdPayload('set_mode', mode);
String paSetBandPayload(String band) => cmdPayload('set_band', band);

// --- Tuner -------------------------------------------------------------------

// tuner.set_inline value is a real JSON bool.
String tunerInlinePayload(bool inline) => cmdPayload('set_inline', inline);

// tuner.tune value is a **string** "mem" / "full".
String tunerTunePayload(String mode) => cmdPayload('tune', mode);

// --- Antenna select / switch -------------------------------------------------

String antennaSelectPayload(String request) => jsonEncode({'request': request});

String antennaSwitchPayload(String port) => jsonEncode({'select': port});

// --- Radio DVK + band ---------------------------------------------------------

String dvkPlayPayload(int id) =>
    jsonEncode({'action': 'dvk_play_${id.clamp(1, 12)}'});

String dvkStopPayload([int? id]) =>
    jsonEncode({'action': 'dvk_stop', 'value': id?.toString() ?? ''});

String radioSetBandPayload(String band) => cmdPayload('set_band', band);

String radioSetMicProfilePayload(String name) =>
    cmdPayload('set_mic_profile', name);
