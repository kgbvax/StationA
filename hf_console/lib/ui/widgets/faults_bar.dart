import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import '../../store/bus_store.dart';
import '../theme.dart';

class FaultsBar extends StatelessWidget {
  const FaultsBar({super.key});

  @override
  Widget build(BuildContext context) {
    final store = context.watch<BusStore>();
    final now = DateTime.now();
    final time = '${_twoDigits(now.hour)}:${_twoDigits(now.minute)}:${_twoDigits(now.second)}';

    final List<_Fault> history = [];
    for (final record in store.faultHistory) {
      history.add(_Fault(
        _ts(record.ts, time),
        _parseTime(record.ts, now),
        '${record.address}: ${record.text}',
        active: record.active,
      ));
    }

    // If an address has an active fault, the fault text is more useful than the
    // generic offline line, so suppress the offline line for that address.
    final activeFaultAddresses = store.faultHistory
        .where((r) => r.active)
        .map((r) => r.address)
        .toSet();

    final offlineSince = store.offlineSince;
    final List<_Fault> offline = [];
    for (final entry in store.offlineList) {
      final address = entry.split(':').first;
      if (activeFaultAddresses.contains(address)) continue;
      // Stamp the row with when the slot actually went offline — the store
      // tracks /status flips, /state device-link flips and connect time for
      // silent slots individually (retained state lies about recency; a
      // render-time fallback would lie harder and tick on every rebuild).
      final since = offlineSince[address] ?? now;
      offline.add(_Fault(_clockTime(since, time), since, entry, active: true));
    }

    // Root cause: a switched-off PSU silently kills hf/switch, pa-arm,
    // ant-switch and everything downstream of 13.8 V — the wall of
    // 'device unreachable' entries needs its cause named on the top line.
    // Confirmed-off only: a missing 'power' key is unknown, not off —
    // inferring a fault from an absent key manufactures the one claim the
    // inference exists to avoid.
    final psuPower = store.stateValueAs<String>('muehle/power/psu-13v8', 'power');
    final psuOnline = store.slots['muehle/power/psu-13v8']?.isOnline ?? false;
    final hfChainDead = store.offlineList.any((e) => e.startsWith('muehle/hf/'));
    if (psuOnline && psuPower == 'off' && hfChainDead) {
      offline.add(_Fault(time, now, 'muehle/power/psu-13v8: PSU OFF — HF control chain unpowered', active: true));
    }

    final faults = [...history, ...offline];
    // Active problems first, then by time (newest first).
    faults.sort((a, b) {
      final priA = a.active ? 0 : 1;
      final priB = b.active ? 0 : 1;
      final pri = priA.compareTo(priB);
      if (pri != 0) return pri;
      return b.at.compareTo(a.at);
    });
    final visible = faults.take(4).toList();
    final activeCount = faults.where((f) => f.active).length;

    return Container(
      color: AppTheme.card,
      padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 10),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        mainAxisSize: MainAxisSize.min,
        children: [
          Row(
            mainAxisAlignment: MainAxisAlignment.spaceBetween,
            children: [
              Text('FAULTS', style: AppTheme.mono(12, weight: FontWeight.w700, letterSpacing: 0.14, color: AppTheme.txtMute)),
              _Tag(activeCount > 0 ? '$activeCount ACTIVE' : 'ALL OK', activeCount > 0 ? AppTheme.red : AppTheme.green),
            ],
          ),
          const SizedBox(height: 6),
          if (visible.isEmpty)
            _FaultRow(fault: _Fault(time, now, 'No faults or offline devices', active: false))
          else
            ...visible.map((f) => _FaultRow(fault: f)),
        ],
      ),
    );
  }

  String _twoDigits(int n) => n.toString().padLeft(2, '0');

  String _clockTime(DateTime at, String fallback) {
    if (at.year > 2000) {
      return '${_twoDigits(at.hour)}:${_twoDigits(at.minute)}:${_twoDigits(at.second)}';
    }
    return fallback;
  }

  String _ts(String? ts, String fallback) {
    if (ts != null && ts.length >= 19) {
      return ts.substring(11, 19);
    }
    return fallback;
  }

  DateTime _parseTime(String? ts, DateTime fallback) {
    if (ts == null || ts.isEmpty) return fallback;
    final parsed = DateTime.tryParse(ts);
    return parsed ?? fallback;
  }
}

class _FaultRow extends StatelessWidget {
  final _Fault fault;

  const _FaultRow({required this.fault});

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.only(bottom: 3),
      child: Row(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Text(
            fault.ts,
            style: AppTheme.mono(13, color: AppTheme.txtFaint, weight: FontWeight.w500),
          ),
          const SizedBox(width: 10),
          Expanded(
            child: Text(
              fault.text,
              style: AppTheme.mono(14,
                  color: fault.active ? AppTheme.red : AppTheme.txtMute,
                  weight: fault.active ? FontWeight.w700 : FontWeight.w500),
            ),
          ),
        ],
      ),
    );
  }
}

class _Tag extends StatelessWidget {
  final String label;
  final Color color;

  const _Tag(this.label, this.color);

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 7, vertical: 2),
      decoration: BoxDecoration(
        color: AppTheme.blend(color, 0.12),
        border: Border.all(color: color),
        borderRadius: BorderRadius.circular(4),
      ),
      child: Text(label, style: AppTheme.mono(11, color: color, weight: FontWeight.w700)),
    );
  }
}

class _Fault {
  final String ts;
  final DateTime at;
  final String text;
  final bool active;

  const _Fault(this.ts, this.at, this.text, {this.active = false});
}
