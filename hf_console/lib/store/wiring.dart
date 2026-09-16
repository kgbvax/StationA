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
  // uhf/radio (icom9700-radio-bridge) — one-shot across the whole action set:
  // `arm` is a session-hold permit that must never re-apply after a bridge
  // or broker restart (fail-disarmed, R11), and a stale queued ptt/set_freq
  // must never replay into a fresh session. The bridge clears the topic
  // after every execute-or-reject.
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

  const RotatorSurface(
    this.stateSlot,
    this.cmdSlot, {
    required this.targetKey,
    required this.showPresets,
    required this.aimPayload,
  });
}

const RotatorSurface hfRotator = RotatorSurface(
  'muehle/hf/rotator',
  'hf/rotator',
  targetKey: 'target_az',
  showPresets: true,
  aimPayload: rotatorAzPayload,
);

/// The VHF array azimuth: `muehle/uhf/az-rotator` /state keys `az` / `target`
/// / `moving` (el lives on its own slot and is the SatRotatorPanel's business).
const RotatorSurface vhfRotator = RotatorSurface(
  'muehle/uhf/az-rotator',
  'uhf/az-rotator',
  targetKey: 'target',
  showPresets: false,
  aimPayload: satRotatorGotoPayload,
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
// R9/R14 contract (icom9700-radio-bridge/docs/mqtt-api.md): per-VFO actions
// carry `vfo`:"main"|"sub" and take the argument under `value` — the station
// value-key convention. The bridge's cmd struct decodes `value` as a JSON
// **string** (strconv/on-off parsed Go-side), so these builders stringify;
// a JSON number fails to unmarshal and lands in /state.error. All are
// published with cmdRetain['muehle/uhf/radio']! = false (one-shot — see the
// cmdRetain comment above; the arm permit must never re-apply after a
// bridge restart — fail-disarmed, R11).

String uhfRadioSetFreqPayload(int freqHz, String vfo) =>
    jsonEncode({'action': 'set_freq', 'value': '$freqHz', 'vfo': vfo});

String uhfRadioSetModePayload(String mode, String vfo) =>
    jsonEncode({'action': 'set_mode', 'value': mode, 'vfo': vfo});

String uhfRadioArmPayload() => jsonEncode({'action': 'arm'});

String uhfRadioDisarmPayload() => jsonEncode({'action': 'disarm'});

String uhfRadioPttPayload(String onOff) => cmdPayload('ptt', onOff);

// --- Ultrabeam controller ----------------------------------------------------

String antCtrlFrequencyPayload(int freqHz) =>
    jsonEncode({'action': 'frequency', 'freq_hz': freqHz});

String antCtrlDirectionPayload(String direction) =>
    cmdPayload('direction', direction);

String antCtrlBandPayload(String band) => cmdPayload('band', band);

String antCtrlRetractPayload() => jsonEncode({'action': 'retract'});

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

String radioSetMicProfilePayload(String name) => cmdPayload('set_mic_profile', name);
