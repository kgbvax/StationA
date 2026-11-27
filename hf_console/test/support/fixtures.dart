import 'dart:convert';
import 'package:hf_console/store/auto_op.dart' show autoOpSlot;
import 'package:hf_console/store/bus_store.dart';

/// Helpers for seeding a [BusStore] with realistic slot state in tests.
extension BusStoreFixtures on BusStore {
  void applyState(String address, Map<String, dynamic> state) {
    apply('$address/state', jsonEncode(state), true);
  }

  void applyStatus(String address, String status) {
    apply('$address/status', status, true);
  }

  void applyMeta(String address, Map<String, dynamic> meta) {
    apply('$address/meta', jsonEncode(meta), true);
  }

  /// Mark a slot as online (bridge + device).
  void setOnline(String address) {
    applyStatus(address, 'online');
    applyState(address, {'device_online': true});
  }

  /// Mark a slot as offline (bridge down).
  void setBridgeOffline(String address) {
    applyStatus(address, '');
  }

  /// Mark a slot as bridge-up but device unreachable.
  void setDeviceOffline(String address) {
    applyStatus(address, 'online');
    applyState(address, {'device_online': false});
  }

  /// Populate the PA slot with an active fault.
  /// Seed the hf/switch PA remote-on relay as energized — the default
  /// station backdrop for PA panel fixtures (relay off is the special case).
  void setPaRelayOn() {
    setOnline('muehle/hf/switch');
    applyState('muehle/hf/switch', {'pa': 'on', 'trx': 'on', 'device_online': true});
  }

  void setPaFault({
    String fault = 'other',
    String error = 'HOT SWITCHING ATTEMPT',
    String mode = 'standby',
    String keyed = 'rx',
    double temp = 35.0,
  }) {
    setPaRelayOn();
    setOnline('muehle/hf/pa');
    applyState('muehle/hf/pa', {
      'mode': mode,
      'keyed': keyed,
      'fault': fault,
      'error': error,
      'temp_c': temp,
      'fwd_power_w': 0,
      'rfl_power_w': 0,
      'swr': 1.0,
      'pa_state': 'STBY',
      'power': 'on',
      'device_online': true,
      'ts': '2026-08-20T14:30:00.000000',
    });
  }

  /// Populate the PA slot as transmitting (keyed=tx) with forward/reflected power.
  void setPaTransmitting({double fwd = 800, double rfl = 20, double swr = 1.5}) {
    setPaRelayOn();
    setOnline('muehle/hf/pa');
    applyState('muehle/hf/pa', {
      'mode': 'operate',
      'keyed': 'tx',
      'fault': 'none',
      'error': '',
      'temp_c': 42.0,
      'fwd_power_w': fwd,
      'rfl_power_w': rfl,
      'swr': swr,
      'pa_state': 'OPR/TX',
      'power': 'on',
      'device_online': true,
      'ts': '2026-08-20T14:30:00.000000',
    });
  }

  /// Populate the PA slot as healthy operate.
  void setPaHealthy() {
    setPaRelayOn();
    setOnline('muehle/hf/pa');
    applyState('muehle/hf/pa', {
      'mode': 'operate',
      'keyed': 'rx',
      'fault': 'none',
      'error': '',
      'temp_c': 38.5,
      'fwd_power_w': 0,
      'rfl_power_w': 0,
      'swr': 1.1,
      'pa_state': 'OPR/RX',
      'power': 'on',
      'device_online': true,
      'ts': '2026-08-20T14:30:00.000000',
    });
  }

  /// Populate the PLC #1 pa-arm slot with the firmware's /state shape
  /// (m5stamp-hf-ctrl API §4): `block`/`error` omitted when empty.
  void setPaArm({bool? enabled = true, bool? armed = true, String block = '', String error = ''}) {
    applyStatus('muehle/hf/pa-arm', 'online');
    applyState('muehle/hf/pa-arm', {
      'ts': 123456,
      'enabled': ?enabled,
      'armed': ?armed,
      'device_online': true,
      if (block.isNotEmpty) 'block': block,
      if (error.isNotEmpty) 'error': error,
    });
  }

  /// Populate the rotator slot at a given azimuth.
  void setRotator({double az = 0.0, double? targetAz, bool moving = false}) {
    setOnline('muehle/hf/rotator');
    applyState('muehle/hf/rotator', {
      'az': az,
      'target_az': targetAz ?? az,
      'moving': moving,
      'device_online': true,
      'ts': '2026-08-20T14:30:00.000000',
    });
  }

  /// Populate a sat-rotator axis slot (uhf/az-rotator / uhf/el-rotator) with
  /// the wire shape spid-ercm-rotator-bridge publishes: retained /meta with
  /// capabilities.axes + capabilities.limits, retained /state with
  /// {ts, az|el, target, moving, link, device_online, error}. A null [pos] or
  /// [target] omits the key (invalid readback / no target), like the bridge.
  void setSatRotator(
    String address, {
    String axis = 'az',
    double? pos,
    double? target,
    bool moving = false,
    bool deviceOnline = true,
    String error = '',
    Map<String, dynamic>? limits,
  }) {
    applyStatus(address, 'online');
    applyMeta(address, {
      'schema': '1.0',
      'role': 'rotator',
      'capabilities': {
        'axes': [axis],
        'limits': limits ??
            {'min': 0.0, 'max': axis == 'az' ? 360.0 : 90.0, 'park': 0.0},
      },
    });
    final state = <String, dynamic>{
      'moving': moving,
      'link': 'serial',
      'device_online': deviceOnline,
      'ts': '2026-09-13T12:34:56Z',
    };
    if (error.isNotEmpty) state['error'] = error;
    if (pos != null) state[axis] = pos;
    if (target != null) state['target'] = target;
    applyState(address, state);
  }

  /// Populate the pol-ctrl slot (m5stamp-pol-ctrl, M5 Stamp PLC #2) with the
  /// wire shape the firmware publishes (docs/m5stamp-pol-ctrl-mqtt-api.md):
  /// retained /meta with role pol-ctrl + capabilities.polarizations, retained
  /// /state { ts, pol, device_online, error? } where `pol` is derived from
  /// the AW9523 relay readback (KTD15), never from the select or /cmd echo.
  void setPolCtrl({
    String pol = 'v',
    bool deviceOnline = true,
    String error = '',
  }) {
    const address = 'muehle/uhf/pol-ctrl';
    applyStatus(address, 'online');
    applyMeta(address, {
      'schema': '1.0',
      'role': 'pol-ctrl',
      'device': {
        'model': 'M5 Stamp PLC #2 (StamPLC K141)',
        'serial': 'xctrl',
        'firmware': '1.0.0',
      },
      'capabilities': {
        'polarizations': ['h', 'v', 'cl', 'cr'],
        'exclusive': true,
        'shared': true,
        'vertical': 'all_relays_off',
      },
    });
    final state = <String, dynamic>{
      'pol': pol,
      'device_online': deviceOnline,
      'ts': '2026-09-13T12:34:56Z',
    };
    if (error.isNotEmpty) state['error'] = error;
    applyState(address, state);
  }

  /// Populate the uhf/radio slot (icom9700-radio-bridge, Icom IC-9700,
  /// receive-only since 2026-09) with the wire shape
  /// icom9700-radio-bridge/docs/mqtt-api.md publishes: always {ts,
  /// session_state (CAPTURE state), audio_demand, monitor, device_online}
  /// (+ error when a fact is held); monitor-gated telemetry
  /// {radio_responding, freq_hz, band, mode, satellite, meters} present
  /// only while the monitor is on AND the radio responds. `device_online`
  /// is capture liveness: healthy idle reads false (R16) — the default
  /// derives it from the session state. Omitted ≠ zeroed.
  void setUhfRadio({
    String sessionState = 'idle',
    bool? deviceOnline,
    bool audioDemand = false,
    bool monitor = false,
    bool responding = false,
    int? freqHz,
    String band = '70cm',
    String mode = 'fm',
    bool satellite = false,
    int? sMeter,
    int? swr,
    int? alc,
    String error = '',
    String ts = '2026-09-15T12:34:56Z',
  }) {
    const address = 'muehle/uhf/radio';
    applyStatus(address, 'online');
    applyMeta(address, {
      'schema': '1.0',
      'role': 'radio',
      'device': {'model': 'Icom IC-9700'},
      'link': 'ethernet',
      'capabilities': {
        'bands': ['2m', '70cm', '23cm'],
        'modes': ['cw', 'usb', 'lsb', 'am', 'fm', 'data'],
      },
    });
    final deviceOnlineValue = deviceOnline ?? (sessionState == 'live');
    final state = <String, dynamic>{
      'ts': ts,
      'session_state': sessionState,
      'audio_demand': audioDemand,
      'monitor': monitor,
      'device_online': deviceOnlineValue,
    };
    if (monitor) {
      state['radio_responding'] = responding;
      if (responding) {
        if (freqHz != null) {
          state['freq_hz'] = freqHz;
          state['band'] = band;
        }
        if (mode.isNotEmpty) state['mode'] = mode;
        state['satellite'] = satellite;
        if (sMeter != null) state['s_meter'] = sMeter;
        if (swr != null) state['swr'] = swr;
        if (alc != null) state['alc'] = alc;
      }
    }
    if (error.isNotEmpty) state['error'] = error;
    applyState(address, state);
  }

  /// Populate the tuner slot.
  void setTuner({bool inline = true, bool settling = false, String fault = '', double swr = 1.2}) {
    setOnline('muehle/hf/tuner');
    applyState('muehle/hf/tuner', {
      'inline': inline,
      'settling': settling,
      'fault': fault,
      'swr': swr,
      'device_online': true,
      'ts': '2026-08-20T14:30:00.000000',
    });
  }

  /// Populate the antenna switch / selector.
  void setAntenna({String selected = 'off', bool settled = true, String mode = 'auto'}) {
    setOnline('muehle/hf/ant-switch');
    applyState('muehle/hf/ant-switch', {
      'selected': selected,
      'settled': settled,
      'device_online': true,
      'ts': '2026-08-20T14:30:00.000000',
    });
    setOnline('muehle/hf/antenna-select');
    applyState('muehle/hf/antenna-select', {
      'mode': mode,
      'device_online': true,
      'ts': '2026-08-20T14:30:00.000000',
    });
  }

  /// Populate the Ultrabeam controller. `band` defaults to '' (unknown) so
  /// tests that don't care aren't tripped into the mismatch pill.
  void setUltrabeam({String direction = 'forward', bool moving = false, String band = '', bool retracted = false}) {
    setOnline('muehle/hf/ant-ctrl');
    applyState('muehle/hf/ant-ctrl', {
      'direction': direction,
      'moving': moving,
      'retracted': retracted,
      'band': band,
      'device_online': true,
      'ts': '2026-08-20T14:30:00.000000',
    });
  }

  /// Populate the radio slot.
  void setRadio({
    int freqHz = 14200000,
    String band = '20m',
    String mode = 'usb',
    String tx = 'rx',
    bool tuning = false,
    int drive = 50,
    bool split = false,
    bool crossBand = false,
    int? rxFreqHz,
    String rxBand = '',
    String? rxInput,
  }) {
    setOnline('muehle/hf/radio');
    applyState('muehle/hf/radio', {
      'freq_hz': freqHz,
      'band': band,
      'mode': mode,
      'tx': tx,
      'tuning': tuning,
      'drive': drive,
      'dvk_status': 'idle',
      'dvk_id': 0,
      'device_online': true,
      'ts': '2026-08-20T14:30:00.000000',
      // Split (flexbridge): /state is the TX slice; rx_* is slice A while a
      // different slice holds the TX flag, omitted otherwise.
      'split': split,
      'cross_band': crossBand,
      if (rxFreqHz != null) 'rx_freq_hz': rxFreqHz,
      if (rxBand.isNotEmpty) 'rx_band': rxBand,
      // Receive antenna of the listened-on slice; omitted like an older
      // flexbridge that does not publish it.
      if (rxInput != null) 'rx_input': rxInput,
    });
  }

  /// Populate power / switch slots for a healthy station.
  void setPower({bool psu = true, bool trx = true, bool pa = true}) {
    setOnline('muehle/power/psu-13v8');
    applyState('muehle/power/psu-13v8', {'power': psu ? 'on' : 'off', 'device_online': true});
    setOnline('muehle/hf/switch');
    applyState('muehle/hf/switch', {'pa': pa ? 'on' : 'off', 'trx': trx ? 'on' : 'off', 'device_online': true});
    setOnline('muehle/hf/power-seq');
    applyState('muehle/hf/power-seq', {'phase': 'running', 'fault': '', 'device_online': true});
  }

  /// Populate the antenna-mode slot (antmode logic slot, no device_online).
  /// [mode] normal leaves no lock; storm / maintenance carry a built lock
  /// parked at [parkAz] (null = not yet), with the nudge in maintenance.
  void setAntMode(
    String mode, {
    int? parkAz = 175,
    bool stormSuggested = false,
    String phase = '',
    String fault = '',
    bool online = true,
    List<Map<String, dynamic>> warnings = const [],
    String reason = '',
    bool powerHold = false,
    Map<String, dynamic>? powerBorrow,
  }) {
    applyStatus('muehle/hf/ant-mode', online ? 'online' : 'offline');
    applyState('muehle/hf/ant-mode', {
      'ts': '2026-10-22T18:00:05Z',
      'ant_mode': mode,
      'source': 'manual',
      'since': '2026-10-22T18:00:00Z',
      if (mode != 'normal')
        'lock': {
          'seq': 17,
          'ground': true,
          'retract': true,
          if (parkAz != null) 'park_az': parkAz,
          'nudge': mode == 'maintenance',
        },
      'phase': phase.isNotEmpty ? phase : (mode == 'normal' ? 'idle' : 'parked'),
      'fault': fault,
      'power_hold': powerHold,
      if (powerBorrow != null) 'power_borrow': powerBorrow,
      'presence': 'manned',
      'wind': {'dir_deg': 265, 'gust_kmh': 72, 'ts': '2026-10-22T17:00:00Z', 'stale': false},
      'warnings': warnings,
      'storm_suggested': stormSuggested,
      'reason': reason,
    });
  }

  /// Populate the 75auto slot (`muehle/hf/auto-op`): /status, /meta, and the
  /// full /state document of the contract (see [autoOpStateDoc]).
  /// [retained] false delivers the /state as a live publish, which makes the
  /// slot fresh when [seq] is new. [changes] replaces top-level keys of the
  /// document.
  void setAutoOp({
    int seq = 1234,
    bool retained = true,
    String status = 'online',
    String commands = 'operate',
    Map<String, dynamic> changes = const {},
  }) {
    applyStatus(autoOpSlot, status);
    applyMeta(autoOpSlot, {
      'schema': '1.0',
      'role': 'auto-op',
      'link': 'none',
      'location': 'shack',
      'host': 'bwpc',
      'capabilities': {
        'schema_name': 'auto-op/v1',
        'bands': ['80m', '40m', '30m', '20m', '17m', '15m', '12m', '10m'],
        'commands': commands,
        'signed': true,
      },
    });
    apply('$autoOpSlot/state', jsonEncode({...autoOpStateDoc(seq: seq), ...changes}), retained);
  }

  /// Deliver one /beat of the 75auto slot.
  void applyAutoOpBeat({int stateSeq = 1234, int nowMs = 1795703563000, int slotOffsetMs = 8100}) {
    apply(
      '$autoOpSlot/beat',
      jsonEncode({'ts': '2026-11-26T14:32:43Z', 'state_seq': stateSeq, 'now_ms': nowMs, 'slot_offset_ms': slotOffsetMs}),
      false,
    );
  }
}

/// The /state document of the 75auto contract (§5.3), with every key set.
Map<String, dynamic> autoOpStateDoc({int seq = 1234}) => {
      'ts': '2026-11-26T14:32:41Z',
      'seq': seq,
      'now_ms': 1795703561000,
      'session': {'state': 'running', 'rule_set': 'everyday.gess', 'level': 'normal', 'mode': 'auto', 'source': 'live'},
      'start': {
        'rule_sets': ['everyday.gess', 'hunt.gess'],
        'last': {'rule_set': 'everyday.gess', 'mode': 'confirm'},
      },
      'station': {
        'band': '20m',
        'dial_hz': 14074000,
        'mode': 'FT8',
        'dx_call': 'VK9XQ',
        'dx_entity': 'Christmas Island',
        'tx_df': 1487,
        'rx_df': 1502,
        'tx_enabled': true,
        'transmitting': false,
        'period_ms': 15000,
        'slot_offset_ms': 6100,
        'band_change': {'target': '17m', 'result': 'pending'},
        'decode_count': 23,
        'last_decode_end_ms': 1795703550000,
        'qso_stage': 'R+RPT',
        'try_count': 0,
        'repeat_count': 1,
        'tx_parity': 'odd',
        'target_choice': 'follow',
      },
      'presence_end_ms': 1795707161000,
      'pending': {
        'id': 42,
        'rule': 'call-needed',
        'action': 'call-station',
        'call': 'VK9XQ',
        'band': '20m',
        'reason': 'dxcc VK9',
        'priority': 50,
        'deadline_ms': 1795703570000,
        'expired': false,
      },
      'current': {
        'id': 41,
        'time_ms': 1795703535000,
        'rule': 'call-needed',
        'action': 'call-station',
        'call': 'VK9XQ',
        'band': '20m',
        'result': 'took-effect',
        'cause': '',
        'gate_reason': '',
        'reason': 'dxcc VK9',
      },
      'idle': {
        'proposed': 0,
        'blocks': [
          {'call': 'DL1ABC', 'why': 'worked before'},
        ],
        'more_blocks': 2,
      },
      'hint': 'waiting for the answer',
      'arm': {'source': 'operator', 'until_ms': 1795703900000},
      'latched': false,
      'stop_unconfirmed': false,
      'stop_reason': '',
      'cycle_error': '',
      'decodes': [
        {
          'period_start_ms': 1795703535000,
          'df': 1502,
          'snr': -14,
          'text': 'DL2XYZ VK9XQ -11',
          'call': 'VK9XQ',
          'needed': true,
          'to_me': true,
          'latest': true,
        },
        {'period_start_ms': 1795703535000, 'df': 800, 'snr': -3, 'text': 'CQ EA8ZZ IL18', 'call': 'EA8ZZ', 'cq': true},
      ],
      'tx_lines': [
        {'period_start_ms': 1795703520000, 'band': '20m', 'df': 1487, 'text': 'VK9XQ DL2XYZ R-09'},
      ],
      'bands': [
        {'band': '40m', 'dial_hz': 7074000, 'seen': true, 'count': 12, 'period_start_ms': 1795702000000},
        {'band': '20m', 'dial_hz': 14074000, 'seen': false, 'count': 0, 'period_start_ms': 0},
      ],
      'log': [
        {
          'id': 40,
          'time_ms': 1795703520000,
          'rule': 'call-needed',
          'action': 'call-station',
          'call': 'VK9XQ',
          'band': '20m',
          'result': 'refused',
          'cause': 'gate',
          'gate_reason': 'tx limit',
          'reason': 'dxcc VK9',
        },
      ],
      'health': [
        {'interface': 'digimode', 'name': 'WSJT-X', 'fresh': true, 'reason': ''},
      ],
      'rig': {'present': true, 'ok': true, 'name': 'rigctld', 'reason': ''},
      'clock_failing': false,
      'remote': {
        'commands': 'operate',
        'last_cmd': {'cmd_id': 'c1f0e7a2c1f0e7a2', 'action': 'target', 'result': 'accepted', 'reason': '', 'at_ms': 1795703561500},
      },
      'error': '',
    };
