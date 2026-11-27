/// The 75AUTO Start: one tap takes the station from any power state to a
/// running 75auto session.
///
/// - The tap publishes `auto_start` to `muehle/hf/power-seq/cmd`, not
///   retained, with the device id of this console as the requester. It does
///   not go through the gate of the 75AUTO page, because 75auto is often
///   offline at that moment (bwpc asleep or off).
/// - powerseq runs the chain: station power, bwpc awake and logged on,
///   AetherSDR, WSJT-X, 75auto. `/state.chain` shows the step that runs
///   or failed, and the page shows one row per step. The last row, the
///   session, is the page's own.
/// - When the chain is `ready` and its requester is this console, within
///   30 min of the tap, the page sends one signed `start_session` with the
///   picked rule set and mode, under the gate of the page and with the
///   stored key. The pending start is in memory only: an app restart drops
///   it, and the programs then stay up without a session.
/// - The Start is off in the antenna modes storm and maintenance, and the
///   page checks the mode again before it sends the session start.
/// - Without a stored key the Start is off: the chain could not end in a
///   session.
/// - A session start refused with `busy` (a session is open, also a latched
///   one) offers END SESSION. After that, a new Start begins a new session.
///
/// The Station tab Start is not this: it runs the station start only.
library;

import 'package:clock/clock.dart';
import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../mqtt/mqtt_service.dart';
import '../../store/auto_op.dart';
import '../../store/bus_store.dart';
import '../../store/device_id.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import 'auto_card.dart';
import 'auto_commands.dart';
import 'auto_live.dart';

/// The page sends the session start only within this time after its tap.
const autoStartWindow = Duration(minutes: 30);

/// The gap under the panel in a scroll view.
const autoStartGap = 4.0;

const _powerSeqSlot = 'muehle/hf/power-seq';

/// The labels of the chain rows, by step id.
const autoStartStepLabels = {
  'station': 'Station power',
  'bwpc': 'bwpc',
  'aethersdr': 'AetherSDR',
  'wsjtx': 'WSJT-X',
  '75auto': '75auto',
  'session': 'Session',
};

enum AutoStepState { waiting, running, done, failed }

/// One row of the progress list.
class AutoStepRow {
  final String id;
  final AutoStepState state;

  /// The fault of a failed step, or a short word about the step.
  final String note;

  const AutoStepRow(this.id, this.state, [this.note = '']);

  String get label => autoStartStepLabels[id] ?? id;
}

/// The start that this console asked for. Memory only.
class AutoStartRequest {
  final String requester;
  final String ruleSet;
  final String mode;

  /// The local time of the tap.
  final DateTime at;

  /// The chain record at the tap. A retained record from an earlier chain
  /// must not count as the answer to this tap.
  final PowerSeqChain? chainAtTap;

  /// A new chain record came after the tap.
  bool chainMoved = false;

  /// The publish of `auto_start` failed.
  bool notSent = false;

  /// The page decided to send the session start. Set before the send, so a
  /// notification during the send cannot send a second one.
  bool sessionAsked = false;

  /// The signed session start, once the page sent it.
  AutoSent? session;

  /// Why the page will not send the session start: expired, antenna mode,
  /// another requester.
  String? stopped;

  AutoStartRequest({
    required this.requester,
    required this.ruleSet,
    required this.mode,
    required this.at,
    required this.chainAtTap,
  });
}

bool _sessionOpen(AutoOpView? v) {
  final s = v?.session?.state;
  return s == 'connecting' || s == 'running';
}

/// The logic of the 75AUTO Start. One instance per [BusStore], so a pending
/// start lives on when the operator changes page, and ends with the app.
class AutoStarter extends ChangeNotifier {
  final BusStore store;
  final MqttService mqtt;

  /// Sends the signed session commands, with the key of the page.
  final AutoCommander commander;

  String? _deviceId;

  /// The device id of this console. Null until it is read.
  String? get deviceId => _deviceId;

  AutoStartRequest? _request;
  AutoStartRequest? get request => _request;

  /// The picks of the operator. Null means the default from
  /// `/state.start.last`.
  String? _ruleSet;
  String? _mode;

  /// The last END SESSION that the panel sent.
  AutoSent? _end;
  AutoSent? get end => _end;

  bool _disposed = false;

  AutoStarter({
    required this.store,
    required this.mqtt,
    AutoKeyReader? readKey,
    Future<String> Function()? deviceId,
  }) : commander = AutoCommander(store: store, mqtt: mqtt, readKey: readKey) {
    store.addListener(_evaluate);
    commander.addListener(_onCommander);
    (deviceId ?? consoleDeviceId)().then((id) {
      if (_disposed) return;
      _deviceId = id;
      _notify();
    }, onError: (_) {});
  }

  static final _byStore = Expando<AutoStarter>();

  /// The starter of [store]. It is made at the first call and lives as long
  /// as the store.
  static AutoStarter of(BusStore store, MqttService mqtt) {
    final s = _byStore[store];
    if (s != null && s.mqtt == mqtt) return s;
    s?.dispose();
    return _byStore[store] = AutoStarter(store: store, mqtt: mqtt);
  }

  @override
  void dispose() {
    _disposed = true;
    store.removeListener(_evaluate);
    commander.removeListener(_onCommander);
    commander.dispose();
    super.dispose();
  }

  void _notify() {
    if (!_disposed) notifyListeners();
  }

  void _onCommander() {
    _evaluate();
    _notify();
  }

  PowerSeqChain? get chain => PowerSeqChain.fromState(store.slots[_powerSeqSlot]?.state);

  bool get sessionOpen => _sessionOpen(store.autoOp);

  // --- Pickers -----------------------------------------------------------------

  List<String> get ruleSets => store.autoOp?.start?.ruleSets ?? const [];

  /// The picked rule set: the pick of the operator, else the last one, else
  /// the first of the list.
  String? get ruleSet {
    final sets = ruleSets;
    for (final c in [_ruleSet, store.autoOp?.start?.last?.ruleSet]) {
      if (c != null && sets.contains(c)) return c;
    }
    return sets.isEmpty ? null : sets.first;
  }

  /// The picked mode: the pick of the operator, else the last one, else
  /// observe.
  String get mode {
    for (final c in [_mode, store.autoOp?.start?.last?.mode]) {
      if (c != null && autoOpModes.contains(c)) return c;
    }
    return 'observe';
  }

  void pickRuleSet(String v) {
    _ruleSet = v;
    _notify();
  }

  void pickMode(String v) {
    _mode = v;
    _notify();
  }

  // --- Start ---------------------------------------------------------------------

  /// Why the Start is off at [now], or null when it is on.
  String? startBlock(DateTime now) {
    if (!store.linkUp) return 'LINK DOWN';
    if (store.slots[_powerSeqSlot]?.status != 'online') return 'powerseq offline';
    final ant = store.antMode;
    if (ant.locked) return 'antenna mode ${ant.mode}';
    if (commander.keyStored != true) return 'no key';
    if (_deviceId == null) return 'device id ?';
    if (ruleSet == null) return 'rule sets ?';
    if (sessionOpen) return 'session open';
    if (chain?.running == true) return 'start running';
    final r = _request;
    if (r != null && !r.notSent && !r.chainMoved && now.difference(r.at) < autoAnswerTime) return 'start sent';
    return null;
  }

  /// The tap on Start. It does nothing while [startBlock] gives a reason.
  void start() {
    final now = clock.now();
    if (startBlock(now) != null) return;
    final r = AutoStartRequest(requester: _deviceId!, ruleSet: ruleSet!, mode: mode, at: now, chainAtTap: chain);
    _request = r;
    _end = null;
    r.notSent = !mqtt.publish(cmdTopic('hf/power-seq'), powerSeqAutoStartPayload(r.requester),
        retain: cmdRetain[_powerSeqSlot]!);
    _notify();
  }

  /// Ends the open session of 75auto.
  void endSession() {
    if (endBlock(clock.now()) != null) return;
    commander.send(AutoControl.endSession, null).then((rec) => _notify());
    _end = commander.operate;
    _notify();
  }

  /// Why END SESSION is off, or null.
  String? endBlock(DateTime now) =>
      autoGateOf(store, autoLiveOf(store, now), keyStored: commander.keyStored).operate;

  bool _expired(AutoStartRequest r, DateTime now) => now.difference(r.at) > autoStartWindow;

  /// Sends the session start when the chain of this tap is ready.
  void _evaluate() {
    final r = _request;
    if (r == null || _disposed) return;
    final c = chain;
    if (!r.chainMoved && c != r.chainAtTap) r.chainMoved = true;
    final s = r.session;
    final last = store.autoOp?.remote?.lastCmd;
    if (r.sessionAsked) {
      if (s != null && s.result == null && last != null && last.cmdId == s.cmdId) {
        s.result = last;
        _notify();
      }
      return;
    }
    if (r.stopped != null || r.notSent || !r.chainMoved || c == null || !c.ready) return;
    final now = clock.now();
    if (c.requester != r.requester) {
      r.stopped = 'started by another device';
    } else if (_expired(r, now)) {
      r.stopped = 'START EXPIRED';
    } else if (store.antMode.locked) {
      r.stopped = 'antenna mode ${store.antMode.mode}';
    } else {
      final gate = autoGateOf(store, autoLiveOf(store, now), keyStored: commander.keyStored);
      // Closed now (75auto not fresh yet, the key not read yet): wait for
      // the next change.
      if (gate.operate != null) return;
      r.sessionAsked = true;
      commander.send(AutoControl.startSession, autoStartSessionValue(r.ruleSet, r.mode)).then((_) => _notify());
      r.session = commander.operate;
    }
    _notify();
  }

  // --- Display -------------------------------------------------------------------

  /// Whether a start of this console was refused because a session is open,
  /// so the panel offers END SESSION.
  bool get offerEnd {
    final r = _request;
    if (r == null || !sessionOpen) return false;
    final s = r.session;
    if (s != null && s.refused && s.result!.reason == 'busy') return true;
    final c = chain;
    return r.chainMoved && c != null && c.failed && c.fault.startsWith('busy: session active');
  }

  /// The start of this console is still on its way to a session.
  bool active(DateTime now) {
    final r = _request;
    if (r == null || r.notSent || r.stopped != null) return false;
    final s = r.session;
    if (s == null) {
      final c = chain;
      if (r.chainMoved && c != null && c.failed) return false;
      return !_expired(r, now);
    }
    if (s.sent == false || s.refused) return false;
    return store.autoOp?.session?.state != 'running';
  }

  /// The panel shows without an open session, and during one while the
  /// start of this console still needs it.
  bool showPanel(DateTime now) => !sessionOpen || active(now) || offerEnd;

  /// The rows of the chain steps and of the session at [now].
  List<AutoStepRow> rows(DateTime now) {
    final c = chain;
    final steps = <AutoStepRow>[];
    final at = c == null ? -1 : powerSeqChainSteps.indexOf(c.step);
    for (var i = 0; i < powerSeqChainSteps.length; i++) {
      final id = powerSeqChainSteps[i];
      if (c == null) {
        steps.add(AutoStepRow(id, AutoStepState.waiting));
      } else if (c.ready) {
        steps.add(AutoStepRow(id, AutoStepState.done));
      } else if (at < 0 || i > at) {
        steps.add(AutoStepRow(id, AutoStepState.waiting));
      } else if (i < at) {
        steps.add(AutoStepRow(id, AutoStepState.done));
      } else {
        steps.add(c.failed
            ? AutoStepRow(id, AutoStepState.failed, c.fault)
            : AutoStepRow(id, AutoStepState.running));
      }
    }
    steps.add(_sessionRow(now));
    return steps;
  }

  AutoStepRow _sessionRow(DateTime now) {
    const id = 'session';
    final r = _request;
    if (r == null) return const AutoStepRow(id, AutoStepState.waiting);
    if (r.stopped != null) return AutoStepRow(id, AutoStepState.failed, r.stopped!);
    final s = r.session;
    if (s == null) {
      final c = chain;
      if (r.chainMoved && c != null && c.failed) return const AutoStepRow(id, AutoStepState.waiting);
      if (_expired(r, now)) return const AutoStepRow(id, AutoStepState.failed, 'START EXPIRED');
      if (r.chainMoved && c != null && c.ready) {
        final gate = autoGateOf(store, autoLiveOf(store, now), keyStored: commander.keyStored);
        return AutoStepRow(id, AutoStepState.running, 'waiting · ${gate.operate ?? '…'}');
      }
      return const AutoStepRow(id, AutoStepState.waiting);
    }
    if (s.sent == null) return const AutoStepRow(id, AutoStepState.running, 'sending');
    if (s.sent == false) {
      return AutoStepRow(id, AutoStepState.failed, s.notSentWhy == 'no key' ? 'NOT SENT · no key' : 'NOT SENT');
    }
    if (s.refused) return AutoStepRow(id, AutoStepState.failed, 'REFUSED · ${s.result!.reason ?? '?'}');
    final state = store.autoOp?.session?.state;
    if (state == 'running') return const AutoStepRow(id, AutoStepState.done, 'running');
    if (state == 'connecting') return const AutoStepRow(id, AutoStepState.running, 'connecting');
    if (s.accepted) return const AutoStepRow(id, AutoStepState.running, 'accepted');
    if (now.difference(s.at) >= autoAnswerTime) return const AutoStepRow(id, AutoStepState.failed, 'NO ANSWER');
    return const AutoStepRow(id, AutoStepState.running, 'sent');
  }

  /// The line under the Start: why it is off, or what came of the tap.
  AutoLine? line(DateTime now) {
    final block = startBlock(now);
    final r = _request;
    if (r != null && r.notSent) return AutoLine('NOT SENT · tap again', AppTheme.amber);
    final c = chain;
    if (r != null && r.chainMoved && c != null && c.failed && !powerSeqChainSteps.contains(c.step)) {
      return AutoLine('REFUSED · ${c.fault}', AppTheme.amber);
    }
    if (r != null && !r.chainMoved && now.difference(r.at) >= autoAnswerTime && c?.running != true) {
      return AutoLine('powerseq · NO ANSWER', AppTheme.amber);
    }
    if (block != null) return AutoLine(block, AppTheme.txtMute);
    return null;
  }

  /// The line of END SESSION.
  AutoLine? endLine(DateTime now) {
    final block = endBlock(now);
    if (block != null) return AutoLine(block, AppTheme.txtMute);
    final e = _end;
    if (e == null) return null;
    if (e.sent == false) return AutoLine('NOT SENT${e.notSentWhy == 'no key' ? ' · no key' : ''}', AppTheme.amber);
    if (e.refused) return AutoLine('REFUSED · ${e.result!.reason ?? '?'}', AppTheme.amber);
    return AutoLine('END SENT', AppTheme.txtMute);
  }
}

// --- Panel -----------------------------------------------------------------------

/// The Start panel of the 75AUTO page: rule set, mode, START, the progress
/// rows, and END SESSION after a busy refusal.
class AutoStartPanel extends StatelessWidget {
  const AutoStartPanel({super.key});

  @override
  Widget build(BuildContext context) {
    final st = context.watch<AutoStarter>();
    return AutoTicker(builder: (context, _) {
      final now = clock.now();
      final block = st.startBlock(now);
      final line = st.line(now);
      final rows = st.rows(now);
      final sets = st.ruleSets;
      return AutoCard(
        key: const ValueKey('auto-start-panel'),
        title: 'Start session',
        child: SingleChildScrollView(
          padding: const EdgeInsets.all(4),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.stretch,
            children: [
              SizedBox(
                height: 36,
                child: Row(children: [
                  Text('RULES', style: AppTheme.display(11, color: AppTheme.txtMute, letterSpacing: 0.08)),
                  const SizedBox(width: 8),
                  Expanded(
                    child: DropdownButton<String>(
                      key: const ValueKey('auto-start-rule-set'),
                      value: st.ruleSet,
                      isExpanded: true,
                      isDense: true,
                      underline: const SizedBox.shrink(),
                      dropdownColor: AppTheme.pane,
                      style: AppTheme.mono(13, color: AppTheme.txt, weight: FontWeight.w600),
                      hint: Text('rule sets ?', style: AppTheme.mono(13, color: AppTheme.txtFaint)),
                      items: [
                        for (final r in sets)
                          DropdownMenuItem(value: r, child: Text(r, maxLines: 1, overflow: TextOverflow.ellipsis)),
                      ],
                      onChanged: (v) {
                        if (v != null) st.pickRuleSet(v);
                      },
                    ),
                  ),
                ]),
              ),
              const SizedBox(height: 4),
              SizedBox(
                height: 48,
                child: Row(children: [
                  for (var i = 0; i < autoOpModes.length; i++) ...[
                    if (i > 0) const SizedBox(width: 3),
                    Expanded(child: _ModeButton(autoOpModes[i], active: st.mode == autoOpModes[i], onTap: st.pickMode)),
                  ],
                ]),
              ),
              const SizedBox(height: 4),
              ElevatedButton(
                key: const ValueKey('auto-start'),
                onPressed: block == null ? st.start : null,
                style: AppTheme.actionButton().copyWith(
                  minimumSize: const WidgetStatePropertyAll(Size(48, 48)),
                  side: WidgetStatePropertyAll(
                      BorderSide(color: block == null ? AppTheme.green : AppTheme.cardLine)),
                  foregroundColor: WidgetStatePropertyAll(block == null ? AppTheme.green : AppTheme.txtFaint),
                ),
                child: Text('START',
                    style: AppTheme.display(17,
                        color: block == null ? AppTheme.green : AppTheme.txtFaint,
                        weight: FontWeight.w700,
                        letterSpacing: 0.08)),
              ),
              if (line != null)
                Padding(
                  padding: const EdgeInsets.only(top: 2),
                  child: Text(line.text,
                      key: const ValueKey('auto-start-line'),
                      maxLines: 2,
                      overflow: TextOverflow.ellipsis,
                      style: AppTheme.mono(11, color: line.color, weight: FontWeight.w600)),
                ),
              const SizedBox(height: 4),
              for (final r in rows) _StepRowView(r),
              if (st.offerEnd) ...[
                const SizedBox(height: 4),
                _EndSession(st, now),
              ],
            ],
          ),
        ),
      );
    });
  }
}

class _ModeButton extends StatelessWidget {
  final String mode;
  final bool active;
  final ValueChanged<String> onTap;

  const _ModeButton(this.mode, {required this.active, required this.onTap});

  @override
  Widget build(BuildContext context) {
    return ElevatedButton(
      key: ValueKey('auto-start-mode-$mode'),
      onPressed: () => onTap(mode),
      style: AppTheme.actionButton().copyWith(
        padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 2)),
        minimumSize: const WidgetStatePropertyAll(Size(48, 48)),
        side: WidgetStatePropertyAll(
            BorderSide(width: active ? 1.4 : 1, color: active ? AppTheme.accent : AppTheme.cardLine)),
        backgroundColor: WidgetStatePropertyAll(active ? AppTheme.accentDim : AppTheme.pane),
      ),
      child: FittedBox(
        fit: BoxFit.scaleDown,
        child: Text(mode.toUpperCase(),
            maxLines: 1,
            style: AppTheme.display(13,
                color: active ? AppTheme.blend(AppTheme.accent, 0.8) : AppTheme.txt,
                weight: FontWeight.w600,
                letterSpacing: 0.04)),
      ),
    );
  }
}

class _StepRowView extends StatelessWidget {
  final AutoStepRow row;

  const _StepRowView(this.row);

  @override
  Widget build(BuildContext context) {
    final (word, color) = switch (row.state) {
      AutoStepState.waiting => ('—', AppTheme.txtFaint),
      AutoStepState.running => ('…', AppTheme.amber),
      AutoStepState.done => ('OK', AppTheme.green),
      AutoStepState.failed => ('FAILED', AppTheme.red),
    };
    return Padding(
      key: ValueKey('auto-start-row-${row.id}'),
      padding: const EdgeInsets.symmetric(vertical: 1),
      child: Row(crossAxisAlignment: CrossAxisAlignment.start, children: [
        SizedBox(
          width: 96,
          child: Text(row.label,
              style: AppTheme.mono(12,
                  color: row.state == AutoStepState.waiting ? AppTheme.txtMute : AppTheme.txt,
                  weight: FontWeight.w600)),
        ),
        SizedBox(
          width: 52,
          child: Text(word,
              key: ValueKey('auto-start-row-${row.id}-state'),
              style: AppTheme.mono(12, color: color, weight: FontWeight.w700)),
        ),
        Expanded(
          child: Text(row.note,
              key: ValueKey('auto-start-row-${row.id}-note'),
              maxLines: 2,
              overflow: TextOverflow.ellipsis,
              style: AppTheme.mono(11, color: row.state == AutoStepState.failed ? AppTheme.red : AppTheme.txtMute)),
        ),
      ]),
    );
  }
}

class _EndSession extends StatelessWidget {
  final AutoStarter st;
  final DateTime now;

  const _EndSession(this.st, this.now);

  @override
  Widget build(BuildContext context) {
    final on = st.endBlock(now) == null;
    final line = st.endLine(now);
    return Column(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
      Text('A session is open. End it, then start again.',
          style: AppTheme.mono(11, color: AppTheme.amber, weight: FontWeight.w600)),
      const SizedBox(height: 2),
      ElevatedButton(
        key: const ValueKey('auto-end-session'),
        onPressed: on ? st.endSession : null,
        style: AppTheme.actionButton(amber: true).copyWith(
          minimumSize: const WidgetStatePropertyAll(Size(48, 48)),
        ),
        child: const Text('END SESSION'),
      ),
      if (line != null)
        Text(line.text,
            key: const ValueKey('auto-end-line'),
            maxLines: 1,
            overflow: TextOverflow.ellipsis,
            style: AppTheme.mono(11, color: line.color, weight: FontWeight.w600)),
    ]);
  }
}

/// The Start panel where the page needs it, else [orElse]: in place of the
/// decode list, which has nothing to show without a session.
class AutoStartSlot extends StatelessWidget {
  final Widget orElse;

  /// The height of the panel in a scroll view. Null fills the parent.
  final double? panelHeight;

  const AutoStartSlot({super.key, required this.orElse, this.panelHeight});

  @override
  Widget build(BuildContext context) {
    final st = context.watch<AutoStarter>();
    return AutoTicker(
      period: const Duration(seconds: 1),
      builder: (context, _) {
        if (!st.showPanel(clock.now())) return orElse;
        final h = panelHeight;
        if (h == null) return const AutoStartPanel();
        return Padding(
          padding: const EdgeInsets.only(bottom: autoStartGap),
          child: SizedBox(height: h, child: const AutoStartPanel()),
        );
      },
    );
  }
}
