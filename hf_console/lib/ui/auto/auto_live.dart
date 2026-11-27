/// The freshness of the 75auto data on the 75AUTO page, and a small ticker
/// that redraws one widget on the local clock.
///
/// The staleness rule is the one of the contract (`auto-op-mqtt-api.md`,
/// §6): the data is fresh after a /beat or a live /state with a new `seq`.
/// It is stale after 6 s without such a message while a session runs or
/// connects, and after 25 s without a session. A retained /state at the
/// subscribe shows as RETAINED until the first fresh message. The phone
/// never compares `ts` with its own clock.
library;

import 'dart:async';

import 'package:clock/clock.dart';
import 'package:flutter/widgets.dart';
import 'package:provider/provider.dart';

import '../../store/auto_op.dart';
import '../../store/bus_store.dart';

/// What the page knows about the age of its data.
enum AutoData {
  /// No /state document at all.
  none,

  /// Only the retained copy of the broker. No fresh message on this
  /// connection yet.
  retained,

  /// A fresh message arrived within the limit.
  fresh,

  /// The last fresh message is older than the limit, or the link is down.
  stale,
}

/// The limit while a session runs or connects.
const autoStaleRunning = Duration(seconds: 6);

/// The limit without a session.
const autoStaleIdle = Duration(seconds: 25);

/// One reading of the freshness, made at [at] on the local clock.
class AutoLive {
  final AutoData data;

  /// Time since the last fresh message. Null before one arrived.
  final Duration? age;

  /// The `ts` of the document, for the RETAINED label.
  final String? ts;

  /// The time of the view on bwpc (`now_ms`), moved on by the local clock
  /// while the data is fresh. Frozen while it is not. Null without a time.
  final int? nowMs;

  /// The time into the period (`slot_offset_ms`), moved on like [nowMs] and
  /// wrapped at the period. Null without an offset or a period.
  final int? slotOffsetMs;
  final int? periodMs;

  const AutoLive({required this.data, this.age, this.ts, this.nowMs, this.slotOffsetMs, this.periodMs});

  /// True only for data that is fresh now. Bars and countdowns move only
  /// then.
  bool get live => data == AutoData.fresh;
}

/// Reads the freshness of the auto-op slot from [store] at [now].
AutoLive autoLiveOf(BusStore store, DateTime now) {
  final view = store.autoOp;
  if (view == null) return const AutoLive(data: AutoData.none);
  final freshAt = store.freshAt(autoOpSlot);
  final age = freshAt == null ? null : now.difference(freshAt);

  final AutoData data;
  if (freshAt == null) {
    data = AutoData.retained;
  } else {
    final state = view.session?.state;
    final limit = (state == 'running' || state == 'connecting') ? autoStaleRunning : autoStaleIdle;
    data = (!store.linkUp || age! >= limit) ? AutoData.stale : AutoData.fresh;
  }

  // The newer of the /state and the last /beat, by the time of the view.
  var baseNow = view.nowMs;
  var baseOffset = view.station?.slotOffsetMs;
  final beat = store.autoOpBeat;
  if (beat?.nowMs != null && (baseNow == null || beat!.nowMs! >= baseNow)) {
    baseNow = beat!.nowMs;
    baseOffset = beat.slotOffsetMs ?? baseOffset;
  }
  final elapsed = data == AutoData.fresh ? age!.inMilliseconds : 0;
  final period = view.station?.periodMs;
  int? offset;
  if (baseOffset != null && period != null && period > 0) {
    offset = (baseOffset + elapsed) % period;
  }
  return AutoLive(
    data: data,
    age: age,
    ts: view.ts,
    nowMs: baseNow == null ? null : baseNow + elapsed,
    slotOffsetMs: offset,
    periodMs: period,
  );
}

/// Redraws [builder] every [period] on the local clock, and on each new
/// view, beat, or link change. Only this widget redraws, not the page.
class AutoTicker extends StatefulWidget {
  final Duration period;
  final Widget Function(BuildContext context, AutoLive live) builder;

  const AutoTicker({super.key, this.period = const Duration(milliseconds: 250), required this.builder});

  @override
  State<AutoTicker> createState() => _AutoTickerState();
}

class _AutoTickerState extends State<AutoTicker> {
  Timer? _timer;

  @override
  void initState() {
    super.initState();
    _timer = Timer.periodic(widget.period, (_) {
      if (mounted) setState(() {});
    });
  }

  @override
  void didUpdateWidget(AutoTicker old) {
    super.didUpdateWidget(old);
    if (old.period != widget.period) {
      _timer?.cancel();
      _timer = Timer.periodic(widget.period, (_) {
        if (mounted) setState(() {});
      });
    }
  }

  @override
  void dispose() {
    _timer?.cancel();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    // Rebuild at once on a new message of the slot, not only on the tick.
    context.select<BusStore, (AutoOpView?, AutoOpBeat?, bool, DateTime?)>(
      (s) => (s.autoOp, s.autoOpBeat, s.linkUp, s.freshAt(autoOpSlot)),
    );
    return widget.builder(context, autoLiveOf(context.read<BusStore>(), clock.now()));
  }
}

// --- Formatting ------------------------------------------------------------------

String _two(int v) => v.toString().padLeft(2, '0');

/// `HH:MM:SS` in UTC of Unix ms.
String utcHms(int ms) {
  final t = DateTime.fromMillisecondsSinceEpoch(ms, isUtc: true);
  return '${_two(t.hour)}:${_two(t.minute)}:${_two(t.second)}';
}

/// `HH:MM` in UTC of Unix ms.
String utcHm(int ms) {
  final t = DateTime.fromMillisecondsSinceEpoch(ms, isUtc: true);
  return '${_two(t.hour)}:${_two(t.minute)}';
}

/// `HH:MM:SS` in UTC of a local time.
String utcHmsOf(DateTime t) => utcHms(t.millisecondsSinceEpoch);

/// The time of an RFC 3339 text as `HH:MM:SS` UTC, or the text itself when
/// it does not parse.
String tsHms(String ts) {
  final t = DateTime.tryParse(ts);
  return t == null ? ts : utcHmsOf(t);
}

/// An age as short text: `0.4 s`, `12 s`, `3 min`.
String ageText(Duration age) {
  final ms = age.inMilliseconds;
  if (ms < 10000) return '${(ms / 1000).toStringAsFixed(1)} s';
  if (ms < 120000) return '${ms ~/ 1000} s';
  return '${ms ~/ 60000} min';
}

/// The dial frequency as `14.074 000` (MHz with the Hz digits in a group).
String dialText(int hz) {
  final mhz = hz ~/ 1000000;
  final rest = (hz % 1000000).toString().padLeft(6, '0');
  return '$mhz.${rest.substring(0, 3)} ${rest.substring(3)}';
}
