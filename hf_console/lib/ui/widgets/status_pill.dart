import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import '../../store/bus_store.dart';
import '../theme.dart';

/// Uniform module status pill, top-right on every card: the device name in
/// green while everything is online and connected, the name plus an
/// irregular-state suffix (TX, TUNING, MOVING, FAULT, …) in that state's
/// severity colour, or the name plus OFFLINE in red when any tracked slot
/// is down.
///
/// [slots] is the set of bus addresses the module depends on — worst case
/// wins, so a module spanning az + el rotators shows OFFLINE as soon as
/// either axis drops. With [useMetaName] the displayed name comes live from
/// the first slot's `/meta.device` (the station convention: the device name
/// lives on the bus, not in the UI); [label] is the fallback and the
/// always-used name for multi-device modules.
class StatusPill extends StatelessWidget {
  final List<String> slots;
  final String label;
  final bool useMetaName;
  final String? suffix;
  final Color? suffixColor;

  const StatusPill({
    super.key,
    required this.slots,
    required this.label,
    this.useMetaName = true,
    this.suffix,
    this.suffixColor,
  });

  /// Friendly device name from the slot's `/meta` HA-discovery block
  /// (`expose.device.name` per the schema template) — the top-level
  /// `device` object carries only model/serial/firmware, no name.
  static String? _metaName(BusStore store, String slot) {
    final expose = store.slots[slot]?.meta?['expose'];
    if (expose is! Map) return null;
    final dev = expose['device'];
    if (dev is! Map) return null;
    final name = dev['name'];
    return name is String && name.isNotEmpty ? name : null;
  }

  @override
  Widget build(BuildContext context) {
    final store = context.watch<BusStore>();
    final online = store.linkUp &&
        slots.every((s) => store.slots[s]?.isOnline ?? false);
    final name = useMetaName
        ? (_metaName(store, slots.first) ?? label)
        : label;

    final String text;
    final Color color;
    if (!online) {
      // A red diagnosis (FAULT, ERR) must survive the link dying — the
      // reason the device is unreachable may be the fault itself. Amber
      // transient suffixes (TUNING, NO RF) are meaningless without a link.
      final redSuffix =
          suffix != null && suffix!.isNotEmpty && suffixColor == AppTheme.red;
      text = '$name${redSuffix ? ' · ${suffix!.toUpperCase()}' : ''} · OFFLINE';
      color = AppTheme.red;
    } else if (suffix != null && suffix!.isNotEmpty) {
      text = '$name · ${suffix!.toUpperCase()}';
      color = suffixColor ?? AppTheme.amber;
    } else {
      text = name;
      color = AppTheme.green;
    }

    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 7, vertical: 2),
      decoration: BoxDecoration(
        color: AppTheme.blend(color, 0.12),
        border: Border.all(color: color),
        borderRadius: BorderRadius.circular(4),
      ),
      child: Text(
        text,
        style: AppTheme.mono(11, color: color, weight: FontWeight.w700),
      ),
    );
  }
}
