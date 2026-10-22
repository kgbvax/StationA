import 'package:hf_console/store/bus_store.dart';

import 'fixtures.dart';

/// Whole-station bus scenarios for the station schematic: a healthy running
/// station plus the states the diagram has to tell apart.
enum StationScenario { running, tx, standby, faults, psuOff, linkDown, storm }

extension StationFixtures on BusStore {
  void _device(String address, Map<String, dynamic> state) {
    applyStatus(address, 'online');
    applyState(address, {'device_online': true, ...state});
  }

  void _logic(String address, Map<String, dynamic> state) {
    applyStatus(address, 'online');
    applyState(address, state);
  }

  /// Every slot online with realistic readings: 20 m FT8 on the Ultrabeam,
  /// IC-9700 idle at the 2 m park frequency, sequencer running.
  void seedStation() {
    // shelly-power-bridge /state today: power, device_online, error, ts —
    // no metering fields (docs/known-issues.md, the metering gap).
    _device('muehle/power/psu-13v8', {'power': 'on'});
    _device('muehle/power/antennas', {'power': 'on'});
    _device('muehle/power/shack-pc-mains', {'power': 'on'});
    _logic('muehle/power/shack-pc', {});
    _device('muehle/hf/switch', {'trx': 'on', 'pa': 'on'});
    _device('muehle/hf/pa-arm', {'enabled': true, 'armed': true});
    _device('muehle/hf/radio', {'freq_hz': 14074000, 'band': '20m', 'mode': 'data', 'tx': 'rx', 'tuning': false});
    _device('muehle/hf/pa', {
      'power': 'on', 'mode': 'operate', 'keyed': 'rx', 'fwd_power_w': 0, 'rfl_power_w': 0,
      'swr': 1.0, 'temp_c': 36, 'fault': 'none', 'error': '',
    });
    _device('muehle/hf/tuner', {'inline': true, 'swr': 1.12, 'fwd': 0, 'settling': false, 'fault': ''});
    _device('muehle/hf/ant-switch', {'selected': 'port4', 'settled': true});
    _device('muehle/hf/ant-ctrl', {'band': '20m', 'direction': 'forward', 'moving': false});
    _device('muehle/hf/rotator', {'az': 212, 'target_az': 212, 'moving': false});
    _logic('muehle/hf/antenna-select', {'mode': 'auto'});
    _logic('muehle/hf/beam-steer', {'enabled': true});
    _logic('muehle/hf/ant-mode', {
      'ant_mode': 'normal', 'source': 'manual', 'phase': 'idle', 'presence': 'manned',
      'wind': {'dir_deg': 265, 'gust_kmh': 31, 'stale': false}, 'warnings': [], 'storm_suggested': false,
    });
    _logic('muehle/hf/power-seq', {'phase': 'running', 'step': '', 'fault': ''});
    _logic('muehle/hf/discovery', {});
    _logic('muehle/hf/spots', {});
    applyStatus('muehle/uhf/radio', 'online');
    applyState('muehle/uhf/radio', {
      'device_online': false, 'session_state': 'idle', 'freq_hz': 145212500, 'mode': 'fm',
      'monitor': true, 'radio_responding': true,
    });
    _device('muehle/uhf/sat-track', {'tracking': false});
    _device('muehle/uhf/az-rotator', {'az': 200, 'moving': false});
    _device('muehle/uhf/el-rotator', {'el': 0, 'moving': false});
    _device('muehle/uhf/pol-ctrl', {'pol': 'cr'});
  }

  /// Seeds [seedStation], then bends it into [scenario]. Link down is a
  /// store call, made after the harness has marked the link up.
  void seedScenario(StationScenario scenario) {
    seedStation();
    switch (scenario) {
      case StationScenario.running:
      case StationScenario.linkDown:
        break;
      case StationScenario.storm:
        // An auto-entered storm, parked broadside: switch grounded, elements
        // in, antenna power borrowed and handed back (standby).
        _logic('muehle/hf/ant-mode', {
          'ant_mode': 'storm', 'source': 'auto', 'since': '2026-10-22T18:00:00Z',
          'lock': {'seq': 17, 'ground': true, 'retract': true, 'park_az': 175, 'nudge': false},
          'phase': 'parked', 'fault': '', 'power_hold': false, 'presence': 'unmanned',
          'wind': {'dir_deg': 265, 'gust_kmh': 72, 'stale': false},
          'warnings': [
            {'id': 'dwd.1', 'event': 'STURMBÖEN', 'ii': '52', 'onset': '2026-10-22T17:00:00Z', 'expires': '2026-10-22T23:00:00Z'},
          ],
          'storm_suggested': false, 'reason': 'DWD STURMBÖEN active, station unmanned', 'drift_deg': 1,
        });
        _device('muehle/hf/ant-switch', {'selected': 'off', 'settled': true});
        _device('muehle/hf/ant-ctrl', {'band': '20m', 'direction': 'forward', 'moving': false, 'retracted': true, 'lock': 'storm'});
        _device('muehle/hf/rotator', {'az': 176, 'target_az': 175, 'moving': false, 'lock': 'storm'});
        _logic('muehle/hf/beam-steer', {'enabled': true, 'lock': 'storm'});
      case StationScenario.tx:
        _device('muehle/hf/radio', {'freq_hz': 14074000, 'band': '20m', 'mode': 'data', 'tx': 'tx', 'tuning': false});
        _device('muehle/hf/pa', {
          'power': 'on', 'mode': 'operate', 'keyed': 'tx', 'fwd_power_w': 812, 'rfl_power_w': 14,
          'swr': 1.29, 'temp_c': 52, 'fault': 'none', 'error': '',
        });
        _device('muehle/hf/tuner', {'inline': true, 'swr': 1.12, 'fwd': 798, 'settling': false, 'fault': ''});
        _device('muehle/uhf/sat-track', {
          'tracking': true, 'sat_name': 'AO-91', 'sub_lat': 40.0, 'sub_lng': -20.0, 'el': 18.0, 'az': 142.0,
        });
      case StationScenario.standby:
        _logic('muehle/hf/power-seq', {'phase': 'standby', 'step': '', 'fault': ''});
        _device('muehle/power/antennas', {'power': 'off'});
        for (final a in ['muehle/hf/ant-ctrl', 'muehle/hf/rotator', 'muehle/uhf/az-rotator', 'muehle/uhf/el-rotator']) {
          applyState(a, {'device_online': false});
        }
        applyStatus('muehle/power/shack-pc', 'offline');
        _logic('muehle/hf/antenna-select', {'mode': 'auto', 'activity': 'inactive'});
      case StationScenario.faults:
        _device('muehle/hf/pa', {
          'power': 'on', 'mode': 'standby', 'keyed': 'rx', 'fwd_power_w': 0, 'rfl_power_w': 0,
          'swr': 2.9, 'temp_c': 58, 'fault': 'swr', 'error': 'HIGH SWR',
        });
        _device('muehle/hf/tuner', {'inline': false, 'swr': 2.6, 'fwd': 0, 'settling': false, 'fault': ''});
        _device('muehle/hf/ant-switch', {'selected': 'port6', 'settled': true});
        _logic('muehle/hf/antenna-select', {'mode': 'manual'});
        setBridgeOffline('muehle/hf/ant-ctrl');
        applyState('muehle/uhf/el-rotator', {'device_online': false, 'error': 'ERC-M: no reply'});
      case StationScenario.psuOff:
        // A normal station stop: the TRX PSU ends off. Bridges on scmino
        // report their devices gone; PLC #1 and the ESPHome switch ARE the
        // bridge, so their LWT fires and the last state stays retained.
        _logic('muehle/hf/power-seq', {'phase': 'idle', 'step': '', 'fault': ''});
        _device('muehle/power/psu-13v8', {'power': 'off'});
        for (final a in ['muehle/hf/radio', 'muehle/hf/tuner', 'muehle/hf/pa']) {
          applyState(a, {'device_online': false});
        }
        for (final a in ['muehle/hf/switch', 'muehle/hf/pa-arm', 'muehle/hf/ant-switch']) {
          applyStatus(a, 'offline');
        }
        applyState('muehle/uhf/radio', {
          'device_online': false, 'session_state': 'idle', 'freq_hz': 145212500, 'mode': 'fm',
          'monitor': true, 'radio_responding': false,
        });
    }
  }
}
