import 'dart:convert';

// Port→name map. Source of truth is antennaselect/config.example.toml [wiring_map].
// port2 and port3 are not wired at Mühle, so they are omitted from the UI.
const antennaMap = {
  'off': 'Grounded',
  'port1': 'Dummy load',
  'port4': 'Ultrabeam',
  'port5': 'Port 5',
  'port6': 'Fan dipole 80/40',
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
  // IC-9700 radio (icom9700-radio-bridge) — all one-shot per KTD6: a
  // retained arm permit would re-arm after every bridge restart and defeat
  // the settled fail-disarm (R11). arm/ptt/set_freq/set_mode are
  // tap-executed toggles and sets, never desired steady state.
  'muehle/uhf/radio': false,
  // pol-ctrl (m5stamp-pol-ctrl) — retained steady state, the actuator
  // exception (KTD13's deliberate contrast with the one-shot rotators
  // above): a retained set_pol re-applies the operator's last intent after
  // a controller reboot or broker reconnect.
  'muehle/uhf/pol-ctrl': true,
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
String paArmPayload(bool enabled) =>
    cmdPayload('set_enabled', enabled ? 'true' : 'false');

// --- Rotator -----------------------------------------------------------------

String rotatorAzPayload(double az) =>
    jsonEncode({'action': 'set_az', 'az': az});

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
// All one-shot (KTD6): cmdRetain['muehle/uhf/radio']! = false. The arm
// permit is bridge-held and must never re-apply after a bridge restart
// (fail-disarm, R11), so — unlike pol-ctrl above — nothing on this slot is
// retained steady state.

/// Arm the TX gate. Arm-while-idle is the on-demand session's connect
/// trigger (R1/R14): the panel publishes this with only the bus link up.
String uhfRadioArmPayload() => jsonEncode({'action': 'arm'});

/// Drop the TX permit. The permit also self-drops on session loss,
/// watchdog trip, and bridge restart (R11) — this is the operator's
/// explicit counterpart.
String uhfRadioDisarmPayload() => jsonEncode({'action': 'disarm'});

/// PTT is an on/off toggle, not hold-to-talk; the bridge rejects it unless
/// `armed` ∧ `session_state=live` (R10).
String uhfRadioPttPayload(bool on) => cmdPayload('ptt', on ? 'on' : 'off');

/// Per-VFO tuning: there is no select-VFO action, so the cmd carries the
/// target VFO alongside the value (station value-key convention — the
/// bridge parses the string form).
String uhfRadioSetFreqPayload(String vfo, int freqHz) =>
    jsonEncode({'action': 'set_freq', 'value': freqHz.toString(), 'vfo': vfo});

String uhfRadioSetModePayload(String vfo, String mode) =>
    jsonEncode({'action': 'set_mode', 'value': mode, 'vfo': vfo});

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
