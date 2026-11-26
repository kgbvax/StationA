/// The status part of the top bar of the 75AUTO page: the session pill
/// (with REPLAY), the mode and presence chip, the rule set, the age of the
/// data from bwpc, and UTC.
///
/// In landscape the strip sits in the one row of the console top bar. When
/// the row is too narrow, the rule set goes first, then the UTC seconds,
/// then UTC. In portrait the strip wraps under the top bar.
///
/// A tap on the mode and presence chip sends `touch`: it counts as operator
/// presence at 75auto. It goes out only under the same gate as the other
/// commands of the page. Nothing else on the page sends a touch.
library;

import 'package:clock/clock.dart';
import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../store/auto_op.dart';
import '../../store/bus_store.dart';
import '../theme.dart';
import 'auto_commands.dart';
import 'auto_live.dart';

class AutoStatusStrip extends StatelessWidget {
  /// One row with items dropped when narrow (landscape top bar), or a wrap
  /// (portrait).
  final bool oneRow;

  const AutoStatusStrip({super.key, this.oneRow = true});

  @override
  Widget build(BuildContext context) {
    final cmd = context.watch<AutoCommander?>();
    return AutoTicker(
      builder: (context, live) {
        final store = context.read<BusStore>();
        VoidCallback? touch;
        if (cmd != null && autoGateOf(store, live, keyStored: cmd.keyStored).operate == null) {
          touch = () => cmd.send(AutoControl.presence, null);
        }
        final items = _items(store, live, touch);
        if (!oneRow) {
          return Wrap(
            spacing: 6,
            runSpacing: 6,
            children: [for (final i in items) i.pill],
          );
        }
        return LayoutBuilder(builder: (context, c) => _row(items, c.maxWidth));
      },
    );
  }

  /// The items that fit in [width], left group at the start and right group
  /// at the end.
  Widget _row(List<_Item> items, double width) {
    const gap = 6.0;
    final shown = [...items];
    double total() => shown.fold(0.0, (w, i) => w + i.width + gap);
    // The order in which items leave a narrow row.
    for (final drop in const ['rules', 'utc-seconds', 'utc', 'age-host']) {
      if (total() <= width) break;
      if (drop == 'utc-seconds') {
        final i = shown.indexWhere((e) => e.id == 'utc');
        if (i >= 0) shown[i] = shown[i].short!;
      } else if (drop == 'age-host') {
        final i = shown.indexWhere((e) => e.id == 'age');
        if (i >= 0 && shown[i].short != null) shown[i] = shown[i].short!;
      } else {
        shown.removeWhere((e) => e.id == drop);
      }
    }
    final left = shown.where((e) => !e.right).toList();
    final right = shown.where((e) => e.right).toList();
    if (total() > width) {
      // Still too wide: scale the row down rather than cut a pill.
      return FittedBox(
        fit: BoxFit.scaleDown,
        alignment: Alignment.centerLeft,
        child: Row(
          mainAxisSize: MainAxisSize.min,
          children: [
            for (final i in shown) ...[i.pill, const SizedBox(width: gap)],
          ],
        ),
      );
    }
    return ClipRect(
      child: Row(
        children: [
          for (final i in left) ...[i.pill, const SizedBox(width: gap)],
          const Spacer(),
          for (final i in right) ...[const SizedBox(width: gap), i.pill],
        ],
      ),
    );
  }

  List<_Item> _items(BusStore store, AutoLive live, VoidCallback? touch) {
    final view = store.autoOp;
    final status = store.slots[autoOpSlot]?.status;
    final items = <_Item>[];

    // Session state, or 75auto offline with the time it went dark.
    if (status == 'offline') {
      final since = store.slots[autoOpSlot]?.statusChangedAt;
      items.add(_Item('session', '75AUTO OFFLINE${since == null ? '' : ' since ${utcHmsOf(since)}'}', AppTheme.red,
          dot: AppTheme.red, key: const ValueKey('auto-session')));
    } else if (status == null && view == null) {
      items.add(_Item('session', '75AUTO ?', AppTheme.txtMute, key: const ValueKey('auto-session')));
    } else {
      final state = view?.session?.state;
      final (text, color) = switch (state) {
        'running' => ('RUNNING', AppTheme.green),
        'connecting' => ('CONNECTING', AppTheme.amber),
        'ended' => ('ENDED', AppTheme.txtMute),
        'not-started' => ('NOT STARTED', AppTheme.txtMute),
        null => ('SESSION ?', AppTheme.txtMute),
        _ => (state.toUpperCase(), AppTheme.txtMute),
      };
      items.add(_Item('session', text, color, dot: color, key: const ValueKey('auto-session')));
    }
    if (view?.session?.source == 'replay') {
      items.add(_Item('replay', 'REPLAY', AppTheme.amber, filled: true, key: const ValueKey('auto-replay')));
    }

    // Mode and presence. A missing mode is "?", never AUTO.
    items.add(_modeItem(view, live).withTap(touch));

    final rules = view?.session?.ruleSet;
    if (rules != null && rules.isNotEmpty) {
      items.add(_Item('rules', 'RULES $rules', AppTheme.txtMute));
    }

    // Age of the data from bwpc.
    final host = store.autoOpMeta?.host ?? '75auto';
    switch (live.data) {
      case AutoData.none:
        items.add(_Item('age', 'NO DATA', AppTheme.txtMute, right: true, key: const ValueKey('auto-age')));
      case AutoData.retained:
        final ts = live.ts;
        final text = ts == null ? 'RETAINED' : 'RETAINED ${tsHms(ts)}';
        items.add(_Item('age', text, AppTheme.amber, right: true, key: const ValueKey('auto-age')));
      case AutoData.fresh:
        final age = live.age!;
        final color = age >= const Duration(seconds: 3) ? AppTheme.amber : AppTheme.txtMute;
        final full = _Item('age', '$host · ${ageText(age)}', color, right: true, key: const ValueKey('auto-age'));
        items.add(full.withShort(_Item('age', ageText(age), color, right: true, key: const ValueKey('auto-age'))));
      case AutoData.stale:
        final age = live.age;
        final text = age == null ? 'DATA STALE' : 'DATA STALE · ${ageText(age)}';
        items.add(_Item('age', text, AppTheme.red, right: true, key: const ValueKey('auto-age')));
    }

    final now = clock.now().toUtc();
    final hms = utcHmsOf(now);
    items.add(_Item('utc', '$hms UTC', AppTheme.txt, right: true, bare: true, size: 16)
        .withShort(_Item('utc', '${hms.substring(0, 5)} UTC', AppTheme.txt, right: true, bare: true, size: 16)));
    return items;
  }

  _Item _modeItem(AutoOpView? view, AutoLive live) {
    const key = ValueKey('auto-mode');
    final mode = view?.session?.mode;
    switch (mode) {
      case 'observe':
        return _Item('mode', 'OBSERVE', AppTheme.accent, key: key);
      case 'confirm':
        return _Item('mode', 'CONFIRM', AppTheme.accent, key: key);
      case 'auto':
        final end = view?.presenceEndMs;
        final now = live.nowMs;
        if (end == null || end <= 0 || now == null) {
          return _Item('mode', 'AUTO · PRESENCE ?', AppTheme.accent, key: key);
        }
        final left = end - now;
        if (left <= 0) {
          return _Item('mode', 'AUTO · PRESENCE 00:00 · HALT ARMED', AppTheme.amber, key: key);
        }
        final s = (left + 999) ~/ 1000;
        final mmss = '${(s ~/ 60).toString().padLeft(2, '0')}:${(s % 60).toString().padLeft(2, '0')}';
        // The countdown stops while the data is not fresh: the faint color
        // says that it does not move.
        return _Item('mode', 'AUTO · PRESENCE $mmss', live.live ? AppTheme.accent : AppTheme.txtFaint, key: key);
      default:
        return _Item('mode', 'MODE ?', AppTheme.txtMute, key: key);
    }
  }
}

/// One pill of the strip with its measured width.
class _Item {
  final String id;
  final String text;
  final Color color;
  final Color? dot;
  final bool right;
  final bool filled;

  /// No border: plain text, for UTC.
  final bool bare;
  final double size;
  final Key? key;
  final _Item? short;

  /// A tap on the pill. Null for a pill that only shows.
  final VoidCallback? onTap;

  _Item(this.id, this.text, this.color,
      {this.dot,
      this.right = false,
      this.filled = false,
      this.bare = false,
      this.size = 13,
      this.key,
      this.short,
      this.onTap});

  _Item withShort(_Item s) => _Item(id, text, color,
      dot: dot, right: right, filled: filled, bare: bare, size: size, key: key, short: s, onTap: onTap);

  _Item withTap(VoidCallback? tap) => _Item(id, text, color,
      dot: dot, right: right, filled: filled, bare: bare, size: size, key: key, short: short?.withTap(tap), onTap: tap);

  TextStyle get _style => bare
      ? AppTheme.mono(size, color: color, weight: FontWeight.w600)
      : AppTheme.display(size, color: filled ? AppTheme.activeButtonText : color, weight: FontWeight.w600, letterSpacing: 0.06);

  double get width {
    final tp = TextPainter(text: TextSpan(text: text, style: _style), textDirection: TextDirection.ltr, maxLines: 1)
      ..layout();
    final w = tp.width;
    tp.dispose();
    return w + (bare ? 4 : 22) + (dot != null ? 14 : 0);
  }

  Widget get pill {
    final label = Text(text, key: key, maxLines: 1, softWrap: false, style: _style);
    if (bare) return Padding(padding: const EdgeInsets.symmetric(horizontal: 2), child: label);
    final box = _box(label);
    if (id != 'mode') return box;
    // The presence chip: a 48 dp touch target around the 36 dp pill.
    return GestureDetector(
      key: const ValueKey('auto-presence-chip'),
      behavior: HitTestBehavior.opaque,
      onTap: onTap,
      child: SizedBox(height: 48, child: Center(widthFactor: 1, child: box)),
    );
  }

  Widget _box(Widget label) {
    return Container(
      height: 36,
      padding: const EdgeInsets.symmetric(horizontal: 10),
      decoration: BoxDecoration(
        color: filled ? color : AppTheme.pane,
        border: Border.all(color: filled ? color : AppTheme.blend(color, 0.7)),
        borderRadius: BorderRadius.circular(4),
      ),
      child: Row(
        mainAxisSize: MainAxisSize.min,
        children: [
          if (dot != null) ...[
            Container(width: 9, height: 9, decoration: BoxDecoration(color: dot, shape: BoxShape.circle)),
            const SizedBox(width: 5),
          ],
          label,
        ],
      ),
    );
  }
}
