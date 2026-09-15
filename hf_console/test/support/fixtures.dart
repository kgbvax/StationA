import 'dart:convert';
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

  /// Populate the UHF radio slot (muehle/uhf/radio,
  /// icom9700-radio-bridge) with the hybrid wire shape (plan R5/R6):
  /// retained /state with top-level active-TX fields (freq_hz/band/mode, the
  /// canonical `tx` string enum), `main`/`sub` detail objects,
  /// `selected_vfo`, `satellite`, `session_state`, `armed`, and meter fields.
  ///
  /// Per-state payload rules (R6): only the `live` snapshot carries the
  /// radio-measured fields (VFO details, meters, satellite, tx); idle/
  /// connecting/error snapshots omit them and stamp
  /// `device_online:false` — CI-V session liveness, not device reachability
  /// (R16). `ts` defaults to a fresh RFC3339 stamp per call so PTT
  /// pending resolution ("first /state newer than the tap") can be driven
  /// by applying a second fixture.
  void setUhfRadio({
    String sessionState = 'live',
    bool armed = false,
    String tx = 'rx',
    String selectedVfo = 'sub',
    int mainFreqHz = 432100000,
    String mainBand = '70cm',
    String mainMode = 'usb',
    int subFreqHz = 145800000,
    String subBand = '2m',
    String subMode = 'fm',
    bool satellite = false,
    num? sMeter = 120,
    num? txPower,
    num? swr,
    num? alc,
    String error = '',
    String? ts,
  }) {
    const address = 'muehle/uhf/radio';
    applyStatus(address, 'online');
    applyMeta(address, {
      'schema': '1.0',
      'role': 'radio',
      'device': {
        'model': 'Icom IC-9700',
        'serial': '9700',
        'firmware': '1.50',
      },
      'capabilities': {
        'bands': ['2m', '70cm', '23cm'],
        'modes': ['cw', 'usb', 'lsb', 'am', 'fm', 'data'],
        'bias_t': true,
        'satellite': true,
        'vfos': ['main', 'sub'],
      },
    });
    final state = <String, dynamic>{
      'session_state': sessionState,
      'armed': armed,
      'selected_vfo': selectedVfo,
      // Healthy idle is CI-V-session-closed, not a device fault (R16).
      'device_online': sessionState == 'live',
      'ts': ts ?? DateTime.now().toUtc().toIso8601String(),
    };
    if (sessionState == 'live') {
      state['freq_hz'] = selectedVfo == 'sub' ? subFreqHz : mainFreqHz;
      state['band'] = selectedVfo == 'sub' ? subBand : mainBand;
      state['mode'] = selectedVfo == 'sub' ? subMode : mainMode;
      state['tx'] = tx;
      state['main'] = {
        'band': mainBand,
        'freq_hz': mainFreqHz,
        'mode': mainMode,
        'data_mode': false,
        'preamp': true,
        'attenuator': false,
      };
      state['sub'] = {
        'band': subBand,
        'freq_hz': subFreqHz,
        'mode': subMode,
        'data_mode': false,
        'preamp': false,
        'attenuator': false,
      };
      state['satellite'] = satellite;
      if (sMeter != null) state['s_meter'] = sMeter;
      if (txPower != null) state['tx_power'] = txPower;
      if (swr != null) state['swr'] = swr;
      if (alc != null) state['alc'] = alc;
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
  void setUltrabeam({String direction = 'forward', bool moving = false, String band = ''}) {
    setOnline('muehle/hf/ant-ctrl');
    applyState('muehle/hf/ant-ctrl', {
      'direction': direction,
      'moving': moving,
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
    });
  }

  /// Populate power / switch slots for a healthy station.
  void setPower({bool master = true, bool psu = true, bool trx = true, bool pa = true}) {
    setOnline('muehle/power/master');
    applyState('muehle/power/master', {'power': master ? 'on' : 'off', 'device_online': true});
    setOnline('muehle/power/psu-13v8');
    applyState('muehle/power/psu-13v8', {'power': psu ? 'on' : 'off', 'device_online': true});
    setOnline('muehle/hf/switch');
    applyState('muehle/hf/switch', {'pa': pa ? 'on' : 'off', 'trx': trx ? 'on' : 'off', 'device_online': true});
    setOnline('muehle/hf/power-seq');
    applyState('muehle/hf/power-seq', {'phase': 'running', 'fault': '', 'device_online': true});
  }
}
