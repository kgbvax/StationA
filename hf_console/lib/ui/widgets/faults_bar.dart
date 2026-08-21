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
        '${record.address}: ${record.text}',
        active: record.active,
      ));
    }

    final List<_Fault> offline = [];
    for (final entry in store.offlineList) {
      offline.add(_Fault(time, entry, active: true));
    }

    final faults = [...offline, ...history.reversed];
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
              Text('FAULTS_AUTO', style: AppTheme.mono(12, weight: FontWeight.w700, letterSpacing: 0.14, color: AppTheme.txtMute)),
              _Tag(activeCount > 0 ? '$activeCount ACTIVE' : 'ALL OK', activeCount > 0 ? AppTheme.red : AppTheme.green),
            ],
          ),
          const SizedBox(height: 6),
          if (faults.isEmpty)
            _FaultRow(fault: _Fault(time, 'No faults or offline devices', active: false))
          else
            ...faults.take(8).map((f) => _FaultRow(fault: f)),
        ],
      ),
    );
  }

  String _twoDigits(int n) => n.toString().padLeft(2, '0');

  String _ts(String? ts, String fallback) {
    if (ts != null && ts.length >= 19) {
      return ts.substring(11, 19);
    }
    return fallback;
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
  final String text;
  final bool active;

  const _Fault(this.ts, this.text, {this.active = false});
}
