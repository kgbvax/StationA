import 'dart:convert';

// Port→name map. Source of truth is antennaselect/config.example.toml [wiring_map].
const ANTENNA_MAP = {
  'off': 'Grounded',
  'port1': 'Dummy load',
  'port2': 'Port 2',
  'port3': 'Port 3',
  'port4': 'Ultrabeam',
  'port5': 'Port 5',
  'port6': 'Fan dipole 80/40',
};

// Which /cmd topics are retained per the real bus policy.
const CMD_RETAIN = {
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
};

String cmdTopic(String slot) => 'muehle/$slot/cmd';

String cmdPayload(String action, dynamic value) =>
    jsonEncode({'action': action, 'value': value});

// Value-key deviations per verified bus contracts.
String paArmPayload(bool enabled) =>
    jsonEncode({'action': 'set_enabled', 'value': enabled ? 'true' : 'false'});

String tunerInlinePayload(bool inline) =>
    jsonEncode({'action': 'set_inline', 'value': inline});

String tunerTunePayload(String mode) =>
    jsonEncode({'action': 'tune', 'value': mode});

String antennaSelectPayload(String port) =>
    jsonEncode({'request': port});

String antennaSwitchPayload(String port) =>
    jsonEncode({'select': port});

String rotatorAzPayload(double az) =>
    jsonEncode({'action': 'set_az', 'az': az});

String dvkPlayPayload(int id) =>
    jsonEncode({'action': 'dvk_play_${id.clamp(1, 12)}'});

String dvkStopPayload([int? id]) =>
    jsonEncode({'action': 'dvk_stop', 'value': id?.toString() ?? ''});
