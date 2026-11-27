/// The input faults of 75auto as error events: one text per input that is
/// bad now (a digimode or other input not fresh, the rig not ok, the clock
/// failing). A healthy input and an input with no data yet show nothing.
/// The page shows the faults in its error line, and a fault goes away when
/// its input is fresh again.
library;

import '../../store/auto_op.dart';
import '../../store/bus_store.dart';

/// The faults of [view], each as the input name and its reason: the
/// digimode inputs, the rig, the other inputs, and the clock.
List<String> autoInputFaults(AutoOpView? view) {
  String of(AutoOpHealth h) {
    final name = (h.name ?? h.interface ?? '?').toUpperCase();
    final reason = h.reason ?? '';
    return reason.isEmpty ? '$name not reporting' : '$name $reason';
  }

  final health = view?.health ?? const <AutoOpHealth>[];
  final bad = health.where((h) => h.fresh == false);
  final rig = view?.rig;
  final rigReason = rig?.reason ?? '';
  return [
    ...bad.where((h) => h.interface == 'digimode').map(of),
    if (rig?.present == true && rig?.ok == false) rigReason.isEmpty ? 'RIG fault' : 'RIG $rigReason',
    ...bad.where((h) => h.interface != 'digimode').map(of),
    if (view?.clockFailing == true) 'CLOCK failing',
  ];
}

/// The faults to show now: none while the link is down or 75auto is not
/// online, because the retained document is then old. The status strip
/// and the LINK DOWN banner cover those cases.
List<String> autoInputFaultsNow(BusStore store) {
  if (!store.linkUp || store.slots[autoOpSlot]?.status != 'online') return const [];
  return autoInputFaults(store.autoOp);
}
