/// The command path of the 75AUTO page: it signs and sends the commands of
/// the page to `muehle/hf/auto-op/cmd`, and it follows each sent command
/// until its result comes back in `/state.remote.last_cmd`.
///
/// Rules (contract `auto-op-mqtt-api.md` §7, plan U8):
///
/// - Each tap reads the key once from the credential store. The key is never
///   shown and never kept in this object.
/// - Stop TX is on when the link is up, `/status` is online, the commands
///   of `/meta` are not `none`, and a key is stored. Stale data does not
///   turn it off. It never locks.
/// - Every other command also needs fresh data, `operate`, and a live
///   session (no replay).
/// - A command with a bad signature gives no result. The page then shows
///   NO ANSWER after 5 s (STOP NOT CONFIRMED after 3 s for a stop).
/// - The page never sends a command on its own. Only a tap sends.
library;


import 'package:clock/clock.dart';
import 'package:flutter/foundation.dart';
import 'package:flutter/painting.dart';

import '../../mqtt/mqtt_service.dart';
import '../../store/auto_op.dart';
import '../../store/bus_store.dart';
import '../../store/credential_store.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import 'auto_live.dart';

/// Reads the command key, or null without one.
typedef AutoKeyReader = Future<Uint8List?> Function();

/// Without a result in this time after a stop, the page shows STOP NOT
/// CONFIRMED.
const autoStopConfirmTime = Duration(seconds: 3);

/// Without a result in this time after any other command, the page shows NO
/// ANSWER, and Approve and Reject unlock.
const autoAnswerTime = Duration(seconds: 5);

/// How long an accepted result shows next to its control.
const autoAcceptedShowTime = Duration(seconds: 10);

/// How long a refused result, NO ANSWER, or NOT SENT shows.
const autoProblemShowTime = Duration(minutes: 2);

/// The controls that send a command.
enum AutoControl {
  stop('STOP', 'stop'),
  approve('APPROVE', 'approve'),
  reject('REJECT', 'reject'),
  observe('OBSERVE', 'mode'),
  confirm('CONFIRM', 'mode'),
  auto('AUTO', 'mode'),
  follow('FOLLOW', 'target'),
  skip('SKIP', 'target'),
  presence('PRESENCE', 'touch'),
  armCq('ARM CQ', 'arm_cq');

  /// The name of the control in the feedback line.
  final String label;

  /// The action on the wire.
  final String action;

  const AutoControl(this.label, this.action);
}

/// One command that the page sent, and what came back.
class AutoSent {
  final AutoControl control;
  final String cmdId;

  /// The local time of the tap.
  final DateTime at;

  /// Null while the key is read. False when the publish failed or no key
  /// was found.
  bool? sent;

  /// Why the command did not go out: `no key` or `link`.
  String? notSentWhy;

  /// The result from `/state.remote.last_cmd` with the same `cmd_id`.
  AutoOpCmdResult? result;

  AutoSent(this.control, this.cmdId, this.at);

  bool get refused => result?.result == 'refused';
  bool get accepted => result != null && !refused;
}

/// The feedback of a control group as one short line.
class AutoLine {
  final String text;
  final Color color;

  /// The control that the line is about, for its amber border. Null for a
  /// reason that is about the whole group.
  final AutoControl? control;

  /// A refused result, NO ANSWER, or NOT SENT: the control gets an amber
  /// border.
  final bool problem;

  const AutoLine(this.text, this.color, {this.control, this.problem = false});
}

/// Whether the commands of the page may go out, as reasons. A null reason
/// means on.
class AutoGate {
  /// The reason that Stop TX is off.
  final String? stop;

  /// The reason that the other commands are off.
  final String? operate;

  const AutoGate({this.stop, this.operate});
}

/// The commands level of 75auto: `/meta` first, else the copy in `/state`.
String? autoCommandsLevel(BusStore store) {
  final meta = store.autoOpMeta?.commands;
  if (meta != null) return meta;
  final copy = store.autoOp?.remote?.commands;
  return autoOpCommandLevels.contains(copy) ? copy : null;
}

/// The gate of the page at [live], in the order of plan U8 step 7.
AutoGate autoGateOf(BusStore store, AutoLive live, {required bool? keyStored}) {
  final status = store.slots[autoOpSlot]?.status;
  final commands = autoCommandsLevel(store);
  final replay = store.autoOp?.session?.source == 'replay';

  String? common() {
    if (!store.linkUp) return 'LINK DOWN';
    if (status != 'online') return '75auto offline';
    return null;
  }

  String? level() {
    if (commands == null) return 'commands ?';
    if (commands == 'none') return 'commands off at bwpc';
    return null;
  }

  final key = keyStored == true ? null : 'no key';
  final stop = common() ?? level() ?? key;
  final operate = common() ??
      (live.live ? null : 'DATA STALE') ??
      level() ??
      (commands == 'stop' || replay ? 'stop only' : null) ??
      key;
  return AutoGate(stop: stop, operate: operate);
}

/// Sends the commands of the page and keeps the last one of each group.
class AutoCommander extends ChangeNotifier {
  final BusStore store;
  final MqttService mqtt;
  final AutoKeyReader readKey;

  /// Whether a key is stored. Null until the first read.
  bool? _keyStored;
  bool? get keyStored => _keyStored;

  /// The last stop that the page sent.
  AutoSent? _stop;
  AutoSent? get stop => _stop;

  /// The last other command that the page sent.
  AutoSent? _operate;
  AutoSent? get operate => _operate;

  bool _disposed = false;

  AutoCommander({required this.store, required this.mqtt, AutoKeyReader? readKey})
      : readKey = readKey ?? CredentialStore().readAutoOpKey {
    store.addListener(_onStore);
    autoOpKeyChanges.addListener(_checkKey);
    _checkKey();
  }

  @override
  void dispose() {
    _disposed = true;
    store.removeListener(_onStore);
    autoOpKeyChanges.removeListener(_checkKey);
    super.dispose();
  }

  void _notify() {
    if (!_disposed) notifyListeners();
  }

  Future<Uint8List?> _read() async {
    try {
      return await readKey();
    } catch (_) {
      // No storage (a test without a mock, or a broken keystore): no key.
      return null;
    }
  }

  /// Reads only whether a key is stored. The key itself is dropped.
  Future<void> _checkKey() async {
    final has = (await _read()) != null;
    if (_disposed || has == _keyStored) return;
    _keyStored = has;
    _notify();
  }

  /// Takes the results of sent commands from each new view.
  void _onStore() {
    final view = store.autoOp;
    if (view == null) return;
    var changed = false;
    final last = view.remote?.lastCmd;
    if (last != null && last.cmdId != null) {
      for (final s in [_stop, _operate]) {
        if (s != null && s.result == null && s.cmdId == last.cmdId) {
          s.result = last;
          changed = true;
        }
      }
    }
    // The latch answers a stop. From then on the page shows LATCHED from
    // the view, and an old stop must not show again after a resume at the
    // station.
    if (_stop != null && view.latched == true) {
      _stop = null;
      changed = true;
    }
    if (changed) _notify();
  }

  /// Whether Approve and Reject wait for the result of one of them.
  bool proposalLocked(DateTime now) {
    final s = _operate;
    if (s == null) return false;
    if (s.control != AutoControl.approve && s.control != AutoControl.reject) return false;
    if (s.sent == false || s.result != null) return false;
    return now.difference(s.at) < autoAnswerTime;
  }

  /// Sends [control] with [value]. The caller checks the gate first.
  Future<void> send(AutoControl control, Object? value) async {
    final rec = AutoSent(control, newAutoOpCmdId(), clock.now());
    if (control == AutoControl.stop) {
      _stop = rec;
    } else {
      _operate = rec;
    }
    // Set the record before the key read, so a second tap sees the lock.
    _notify();
    final key = await _read();
    if (_disposed) return;
    if (key == null) {
      rec.sent = false;
      rec.notSentWhy = 'no key';
      _keyStored = false;
      _notify();
      return;
    }
    _keyStored = true;
    final payload = autoOpCmd(control.action, value, key: key, cmdId: rec.cmdId);
    rec.sent = mqtt.publish(cmdTopic('hf/auto-op'), payload, retain: cmdRetain[autoOpSlot]!);
    if (rec.sent == false) rec.notSentWhy = 'link';
    _notify();
  }

  /// The line of Stop TX at [now], or null for no line.
  AutoLine? stopLine(AutoGate gate, DateTime now) {
    final reason = gate.stop;
    if (reason != null) return AutoLine(reason, AppTheme.amber);
    final view = store.autoOp;
    if (view?.stopUnconfirmed == true) return AutoLine('WSJT-X STILL SENDING', AppTheme.red);
    if (view?.latched == true) return AutoLine('LATCHED', AppTheme.red);
    final s = _stop;
    if (s == null) return null;
    if (s.sent == false) {
      return AutoLine(s.notSentWhy == 'no key' ? 'NOT SENT · no key' : 'NOT SENT · tap again', AppTheme.amber);
    }
    if (s.refused) return AutoLine('STOP REFUSED · ${s.result!.reason ?? '?'}', AppTheme.amber);
    if (s.accepted) return AutoLine('STOP SENT', AppTheme.txt);
    if (now.difference(s.at) >= autoStopConfirmTime) {
      return AutoLine('STOP NOT CONFIRMED · tap again', AppTheme.amber);
    }
    return AutoLine('STOP SENT', AppTheme.txt);
  }

  /// The line of the other controls at [now], or null for no line. The
  /// reason of the gate goes first: it says why nothing can be sent now.
  AutoLine? operateLine(AutoGate gate, DateTime now) {
    final reason = gate.operate;
    if (reason != null) return AutoLine(reason, AppTheme.txtMute);
    final s = _operate;
    if (s == null) return null;
    final age = now.difference(s.at);
    final name = s.control.label;
    if (s.sent == null) return AutoLine('$name SENT', AppTheme.txtMute, control: s.control);
    if (s.sent == false) {
      if (age >= autoProblemShowTime) return null;
      return AutoLine('$name NOT SENT${s.notSentWhy == 'no key' ? ' · no key' : ''}', AppTheme.amber,
          control: s.control, problem: true);
    }
    if (s.refused) {
      if (age >= autoProblemShowTime) return null;
      return AutoLine('$name REFUSED · ${s.result!.reason ?? '?'}', AppTheme.amber, control: s.control, problem: true);
    }
    if (s.accepted) {
      if (age >= autoAcceptedShowTime) return null;
      return AutoLine('$name ACCEPTED', AppTheme.txtMute, control: s.control);
    }
    if (age >= autoAnswerTime) {
      if (age >= autoProblemShowTime) return null;
      return AutoLine('$name · NO ANSWER', AppTheme.amber, control: s.control, problem: true);
    }
    return AutoLine('$name SENT', AppTheme.txtMute, control: s.control);
  }

  /// The control whose last command was refused or got no answer, for its
  /// amber border. Null otherwise.
  AutoControl? problemControl(AutoGate gate, DateTime now) {
    final line = operateLine(gate, now);
    if (line == null || !line.problem) return null;
    return line.control;
  }
}
