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
  void setPaFault({
    String fault = 'other',
    String error = 'HOT SWITCHING ATTEMPT',
    String mode = 'standby',
    String keyed = 'rx',
    double temp = 35.0,
  }) {
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

  /// Populate the Ultrabeam controller.
  void setUltrabeam({String direction = 'forward', bool moving = false}) {
    setOnline('muehle/hf/ant-ctrl');
    applyState('muehle/hf/ant-ctrl', {
      'direction': direction,
      'moving': moving,
      'device_online': true,
      'ts': '2026-08-20T14:30:00.000000',
    });
  }

  /// Populate the radio slot.
  void setRadio({int freqHz = 14200000, String band = '20m', String mode = 'usb', String tx = 'rx', int drive = 50}) {
    setOnline('muehle/hf/radio');
    applyState('muehle/hf/radio', {
      'freq_hz': freqHz,
      'band': band,
      'mode': mode,
      'tx': tx,
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
