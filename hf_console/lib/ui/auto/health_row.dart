/// The health row of the 75AUTO page: one chip per input of 75auto
/// (WSJT-X, RIG, NEEDED, SPOTS, CLOCK). A healthy input has a green dot. A
/// degraded one has an amber frame and its reason. An unknown one shows `?`.
///
/// [StatusPill] works on bus slots, and these are inputs inside one slot,
/// so the page has its own chip in the same style.
library;

import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../store/auto_op.dart';
import '../../store/bus_store.dart';
import '../theme.dart';

/// The state of one input.
enum AutoHealthLevel { ok, warn, unknown }

class AutoHealthItem {
  final String name;
  final AutoHealthLevel level;
  final String reason;

  const AutoHealthItem(this.name, this.level, [this.reason = '']);
}

/// The chips for [view]: the digimode inputs, the rig, the other inputs,
/// and the clock.
List<AutoHealthItem> autoHealthItems(AutoOpView? view) {
  AutoHealthItem of(AutoOpHealth h) {
    final name = (h.name ?? h.interface ?? '?').toUpperCase();
    final reason = h.reason ?? '';
    return switch (h.fresh) {
      true => AutoHealthItem(name, AutoHealthLevel.ok),
      false => AutoHealthItem(name, AutoHealthLevel.warn, reason),
      null => AutoHealthItem(name, AutoHealthLevel.unknown),
    };
  }

  final health = view?.health ?? const <AutoOpHealth>[];
  final digi = health.where((h) => h.interface == 'digimode');
  final rest = health.where((h) => h.interface != 'digimode');
  final rig = view?.rig;
  final AutoHealthItem rigItem;
  if (rig == null || rig.present != true) {
    rigItem = const AutoHealthItem('RIG', AutoHealthLevel.unknown);
  } else {
    rigItem = switch (rig.ok) {
      true => const AutoHealthItem('RIG', AutoHealthLevel.ok),
      false => AutoHealthItem('RIG', AutoHealthLevel.warn, rig.reason ?? ''),
      null => const AutoHealthItem('RIG', AutoHealthLevel.unknown),
    };
  }
  final clockItem = switch (view?.clockFailing) {
    false => const AutoHealthItem('CLOCK', AutoHealthLevel.ok),
    true => const AutoHealthItem('CLOCK', AutoHealthLevel.warn, 'failing'),
    null => const AutoHealthItem('CLOCK', AutoHealthLevel.unknown),
  };
  return [...digi.map(of), rigItem, ...rest.map(of), clockItem];
}

class HealthRow extends StatelessWidget {
  /// Wrap the chips (portrait) instead of one row.
  final bool wrap;

  const HealthRow({super.key, this.wrap = false});

  @override
  Widget build(BuildContext context) {
    final view = context.select<BusStore, AutoOpView?>((s) => s.autoOp);
    final chips = [for (final i in autoHealthItems(view)) AutoHealthChip(item: i)];
    if (wrap) {
      return Container(
        decoration: AppTheme.cardDecoration(),
        padding: const EdgeInsets.all(4),
        child: Wrap(spacing: 4, runSpacing: 4, children: chips),
      );
    }
    return Container(
      decoration: AppTheme.cardDecoration(),
      padding: const EdgeInsets.symmetric(horizontal: 4, vertical: 3),
      child: Row(mainAxisSize: MainAxisSize.min, children: [
        for (var i = 0; i < chips.length; i++) ...[if (i > 0) const SizedBox(width: 4), chips[i]],
      ]),
    );
  }
}

/// One chip of the health row, in the style of [StatusPill].
class AutoHealthChip extends StatelessWidget {
  final AutoHealthItem item;

  const AutoHealthChip({super.key, required this.item});

  @override
  Widget build(BuildContext context) {
    final color = switch (item.level) {
      AutoHealthLevel.ok => AppTheme.green,
      AutoHealthLevel.warn => AppTheme.amber,
      AutoHealthLevel.unknown => AppTheme.txtMute,
    };
    final text = switch (item.level) {
      AutoHealthLevel.ok => item.name,
      AutoHealthLevel.warn => item.reason.isEmpty ? item.name : '${item.name} ${item.reason}',
      AutoHealthLevel.unknown => '${item.name} ?',
    };
    return Container(
      key: ValueKey('auto-health-${item.name}'),
      height: 28,
      padding: const EdgeInsets.symmetric(horizontal: 7),
      decoration: BoxDecoration(
        color: AppTheme.pane,
        border: Border.all(color: item.level == AutoHealthLevel.warn ? AppTheme.amber : AppTheme.cardLine),
        borderRadius: BorderRadius.circular(4),
      ),
      child: Row(mainAxisSize: MainAxisSize.min, children: [
        if (item.level == AutoHealthLevel.ok) ...[
          Container(width: 8, height: 8, decoration: BoxDecoration(color: color, shape: BoxShape.circle)),
          const SizedBox(width: 5),
        ],
        Text(text,
            maxLines: 1,
            style: AppTheme.display(12, color: item.level == AutoHealthLevel.ok ? AppTheme.txtMute : color, letterSpacing: 0.05)),
      ]),
    );
  }
}
