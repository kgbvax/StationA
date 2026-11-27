import 'dart:convert';

import 'package:hf_console/store/auto_op.dart' show autoOpSlot;
import 'package:hf_console/store/bus_store.dart';

import 'fixtures.dart';

/// The time of the view in [busyAutoOpDoc]: 2026-11-26 14:32:41 UTC.
const busyNowMs = 1795703561000;

/// The start of the FT8 period at [hms] seconds after 14:32:00 UTC.
int _p(int s) => 1795703520000 + s * 1000;

/// A busy session in auto mode: a QSO with VK9XQ at R+RPT, a pending QSY
/// proposal, decodes with NEW, TGT, and one to me, own lines, band counts, a
/// log, and one degraded input.
Map<String, dynamic> busyAutoOpDoc({int seq = 2000}) => {
      ...autoOpStateDoc(seq: seq),
      'now_ms': busyNowMs,
      'session': {'state': 'running', 'rule_set': 'hunt-and-cq.gess', 'level': 'normal', 'mode': 'auto', 'source': 'live'},
      'station': {
        'band': '20m',
        'dial_hz': 14074000,
        'mode': 'FT8',
        'dx_call': 'VK9XQ',
        'dx_entity': 'Christmas I.',
        'dx_name': 'Test Operator',
        'dx_km': 11234,
        'tx_df': 1487,
        'rx_df': 1502,
        'tx_enabled': true,
        'transmitting': true,
        'period_ms': 15000,
        'slot_offset_ms': 11000,
        'band_change': null,
        'decode_count': 23,
        'last_decode_end_ms': _p(45),
        'qso_stage': 'R+RPT',
        'try_count': 0,
        'repeat_count': 1,
        'target_choice': 'follow',
      },
      'presence_end_ms': busyNowMs + 492000,
      'pending': {
        'id': 42,
        'rule': 'band-hop',
        'action': 'qsy',
        'call': '',
        'band': '17m',
        'reason': 'After this QSO. 20m: no new station in 6 cycles. 17m: 3 needed stations spotted.',
        'priority': 40,
        'deadline_ms': busyNowMs + 12000,
        'expired': false,
      },
      'hint': '',
      'latched': false,
      'stop_unconfirmed': false,
      'decodes': [
        {'period_start_ms': _p(0), 'df': 720, 'snr': -9, 'text': 'CQ PY2AA GG66', 'call': 'PY2AA', 'cq': true},
        {
          'period_start_ms': _p(0),
          'df': 1502,
          'snr': -14,
          'text': 'DL2XYZ VK9XQ -11',
          'call': 'VK9XQ',
          'needed': true,
          'to_me': true,
        },
        {'period_start_ms': _p(0), 'df': 1300, 'snr': -12, 'text': 'G4XYZ ZL1BQD -15', 'call': 'ZL1BQD'},
        {'period_start_ms': _p(0), 'df': 2600, 'snr': -5, 'text': 'IK2ABC F5XYZ 73', 'call': 'F5XYZ'},
        {'period_start_ms': _p(30), 'df': 612, 'snr': -7, 'text': 'CQ JA1ABC PM95', 'call': 'JA1ABC', 'cq': true, 'latest': true},
        {'period_start_ms': _p(30), 'df': 1840, 'snr': 2, 'text': 'CQ DX EA8BFK IL18', 'call': 'EA8BFK', 'cq': true, 'latest': true},
        {'period_start_ms': _p(30), 'df': 2210, 'snr': -19, 'text': 'K1ABC W5XYZ R-08', 'call': 'W5XYZ', 'latest': true},
        {
          'period_start_ms': _p(30),
          'df': 980,
          'snr': -21,
          'text': 'CQ 3B8HC LG89',
          'call': '3B8HC',
          'cq': true,
          'needed': true,
          'latest': true,
        },
        {'period_start_ms': _p(30), 'df': 450, 'snr': -3, 'text': 'OH2BH SP9KR RR73', 'call': 'SP9KR', 'latest': true},
        {'period_start_ms': _p(30), 'df': 1460, 'snr': -22, 'text': 'VK9XQ JA3XYZ R-18', 'call': 'JA3XYZ', 'latest': true},
        {'period_start_ms': _p(30), 'df': 1530, 'snr': -16, 'text': 'CQ VK9XQ OH29', 'call': 'VK9XQ', 'cq': true, 'needed': true, 'latest': true},
      ],
      'tx_lines': [
        {'period_start_ms': _p(-15), 'band': '20m', 'df': 1487, 'text': 'VK9XQ DL2XYZ JO31'},
        {'period_start_ms': _p(15), 'band': '20m', 'df': 1487, 'text': 'VK9XQ DL2XYZ R-09'},
        {'period_start_ms': _p(-300), 'band': '40m', 'df': 900, 'text': 'CQ DL2XYZ JO31'},
      ],
      'bands': [
        {'band': '80m', 'dial_hz': 3573000, 'seen': true, 'count': 4, 'period_start_ms': busyNowMs - 1800000},
        {'band': '40m', 'dial_hz': 7074000, 'seen': true, 'count': 12, 'period_start_ms': busyNowMs - 660000},
        {'band': '30m', 'dial_hz': 10136000, 'seen': true, 'count': 6, 'period_start_ms': busyNowMs - 900000},
        {'band': '20m', 'dial_hz': 14074000, 'seen': true, 'count': 23, 'period_start_ms': _p(30)},
        {'band': '17m', 'dial_hz': 18100000, 'seen': true, 'count': 9, 'period_start_ms': busyNowMs - 1200000},
        {'band': '15m', 'dial_hz': 21074000, 'seen': true, 'count': 3, 'period_start_ms': busyNowMs - 1500000},
        {'band': '12m', 'dial_hz': 24915000, 'seen': true, 'count': 1, 'period_start_ms': busyNowMs - 2400000},
        {'band': '10m', 'dial_hz': 28074000, 'seen': false, 'count': 0, 'period_start_ms': 0},
      ],
      'log': [
        _log(36, busyNowMs - 860000, 'logged', 'JA1ABC', '40m', 'took-effect'),
        _log(37, busyNowMs - 680000, 'qsy', '', '20m', 'took-effect'),
        _log(38, busyNowMs - 500000, 'call-station', 'ZL1BQD', '20m', 'refused', gate: 'tx limit'),
        _log(39, busyNowMs - 340000, 'call-station', 'VK9XQ', '20m', 'took-effect'),
        _log(40, busyNowMs - 220000, 'logged', '3B8HC', '20m', 'took-effect'),
      ],
      'health': [
        {'interface': 'digimode', 'name': 'WSJT-X', 'fresh': true, 'reason': ''},
        {'interface': 'needed', 'name': 'NEEDED', 'fresh': true, 'reason': ''},
        {'interface': 'spots', 'name': 'SPOTS', 'fresh': false, 'reason': '4 min old'},
      ],
      'rig': {'present': true, 'ok': true, 'name': 'rigctld', 'reason': ''},
      'clock_failing': false,
      'remote': {'commands': 'operate', 'last_cmd': null},
      'error': '',
    };

Map<String, dynamic> _log(int id, int t, String action, String call, String band, String result, {String gate = ''}) => {
      'id': id,
      'time_ms': t,
      'rule': 'everyday',
      'action': action,
      'call': call,
      'band': band,
      'result': result,
      'cause': gate.isEmpty ? '' : 'gate',
      'gate_reason': gate,
      'reason': '',
    };

extension AutoOpFixtures on BusStore {
  /// The retained copy of [doc] with /status and /meta, as at a subscribe.
  void seedAutoOp(Map<String, dynamic> doc, {String status = 'online'}) {
    setAutoOp(seq: doc['seq'] as int, status: status, changes: doc);
  }

  /// A live /state of [doc]: fresh when its `seq` is new.
  void liveAutoOp(Map<String, dynamic> doc) {
    apply('$autoOpSlot/state', jsonEncode(doc), false);
  }
}
