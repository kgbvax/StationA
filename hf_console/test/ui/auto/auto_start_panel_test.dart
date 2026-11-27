import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/mqtt/mqtt_service.dart';
import 'package:mqtt_client/mqtt_client.dart';
import 'package:hf_console/store/auto_op.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/auto/auto_start_panel.dart';
import 'package:hf_console/ui/screens/console_screen.dart';
import 'package:provider/provider.dart';
import 'package:shared_preferences/shared_preferences.dart';

import '../../support/auto_op_fixtures.dart';
import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/render_support.dart';
import '../../support/test_harness.dart';

/// A test key. It is not the key of the station.
final _keyHex = 'cd' * 32;
final _key = parseAutoOpKey(_keyHex)!;

const _seqSlot = 'muehle/hf/power-seq';

/// A 75auto view without an open session: the Start panel shows.
Map<String, dynamic> _idleDoc({String state = 'ended', bool latched = false, Map<String, dynamic>? lastCmd}) => {
      ...busyAutoOpDoc(),
      'session': {'state': state, 'rule_set': 'everyday.gess', 'level': 'normal', 'mode': 'confirm', 'source': 'live'},
      'pending': null,
      'latched': latched,
      'station': {...(busyAutoOpDoc()['station'] as Map<String, dynamic>), 'transmitting': false},
      'remote': {'commands': 'operate', 'last_cmd': lastCmd},
    };

int _seq = 9000;

/// A live /state of [doc] and a /beat: the 75auto data is fresh.
void _live(BusStore store, Map<String, dynamic> doc) {
  _seq++;
  store.liveAutoOp({...doc, 'seq': _seq});
  store.applyAutoOpBeat(stateSeq: _seq, nowMs: busyNowMs, slotOffsetMs: 11000);
}

void _chain(BusStore store, String state, String step, {String fault = '', String? requester, int ts = 1}) {
  store.applyState(_seqSlot, {
    'phase': 'running',
    'fault': '',
    'chain': {'state': state, 'step': step, 'fault': fault, 'requester': requester ?? 'other', 'ts': ts},
  });
}

/// A fake MQTT service whose publish to [failTopic] fails (not sent) while
/// [fail] is true. Each publish is recorded, also a failed one.
class _FailingMqtt extends FakeMqttService {
  final String failTopic;
  bool fail = true;

  _FailingMqtt(super.store, this.failTopic);

  @override
  bool publish(String topic, String payload, {required bool retain, MqttQos qos = MqttQos.atLeastOnce}) {
    super.publish(topic, payload, retain: retain, qos: qos);
    return !(fail && topic == failTopic);
  }
}

Future<(BusStore, FakeMqttService)> _pump(WidgetTester tester,
    {bool key = true, Map<String, dynamic>? doc, FakeMqttService Function(BusStore)? mqttOf}) async {
  tester.view.physicalSize = const Size(2400, 1080);
  tester.view.devicePixelRatio = 2.625;
  addTearDown(tester.view.reset);
  SharedPreferences.setMockInitialValues(key ? {'auto_op_key': _keyHex} : {});
  final d = doc ?? _idleDoc();
  final store = BusStore()..seedAutoOp(d);
  store.applyStatus(_seqSlot, 'online');
  store.applyState(_seqSlot, {'phase': 'idle', 'fault': ''});
  final mqtt = mqttOf?.call(store) ?? FakeMqttService(store);
  await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const ConsoleScreen()));
  await tester.pump(const Duration(milliseconds: 100));
  await tester.tap(find.widgetWithText(InkWell, '75AUTO'));
  await tester.pump(const Duration(milliseconds: 100));
  _live(store, d);
  await tester.pump(const Duration(milliseconds: 300));
  await tester.pump(const Duration(seconds: 1));
  return (store, mqtt);
}

AutoStarter _starter(WidgetTester tester) =>
    AutoStarter.of(tester.element(find.byType(AutoStartPanel)).read<BusStore>(),
        tester.element(find.byType(AutoStartPanel)).read<MqttService>());

Finder _k(String key) => find.byKey(ValueKey(key));

bool _on(WidgetTester tester, String key) => tester.widget<ElevatedButton>(_k(key)).onPressed != null;

String _text(WidgetTester tester, String key) => tester.widget<Text>(_k(key)).data!;

Future<void> _tap(WidgetTester tester, String key) async {
  await tester.tap(_k(key), warnIfMissed: false);
  await tester.pump();
  await tester.pump(const Duration(milliseconds: 50));
}

Future<void> _settle(WidgetTester tester) async {
  await tester.pump();
  await tester.pump(const Duration(milliseconds: 50));
  await tester.pump(const Duration(seconds: 1));
}

List<PublishRecord> _on75(FakeMqttService m) => m.publishes.where((p) => p.topic == 'muehle/hf/auto-op/cmd').toList();
List<PublishRecord> _onSeq(FakeMqttService m) => m.publishes.where((p) => p.topic == '$_seqSlot/cmd').toList();

/// Checks the signature of one 75auto command and returns its JSON.
Map<String, dynamic> _signed(PublishRecord p) {
  expect(p.retain, isFalse);
  final m = jsonDecode(p.payload) as Map<String, dynamic>;
  expect(m['sig'],
      signAutoOpCmd(_key, m['action'] as String, jsonEncode(m['value']), m['ts'] as String, m['cmd_id'] as String));
  return m;
}

/// Taps Start and runs the chain to `ready` for this console.
Future<String> _startToReady(WidgetTester tester, BusStore store) async {
  await _tap(tester, 'auto-start');
  final me = _starter(tester).deviceId!;
  _chain(store, 'running', 'station', requester: me, ts: 2);
  await _settle(tester);
  _chain(store, 'ready', '75auto', requester: me, ts: 3);
  await _settle(tester);
  return me;
}

void main() {
  setUpAll(() async {
    TestWidgetsFlutterBinding.ensureInitialized();
    await loadRenderFonts();
  });

  testWidgets('the panel hides during a session and shows without one', (tester) async {
    final (store, _) = await _pump(tester, doc: busyAutoOpDoc());
    expect(find.byType(AutoStartPanel), findsNothing);
    _live(store, _idleDoc());
    await _settle(tester);
    expect(find.byType(AutoStartPanel), findsOneWidget);
  });

  testWidgets('the pickers default to the last choice from /state.start', (tester) async {
    await _pump(tester);
    final dd = tester.widget<DropdownButton<String>>(_k('auto-start-rule-set'));
    expect(dd.value, 'everyday.gess');
    expect(_starter(tester).mode, 'confirm');
  });

  testWidgets('Start publishes auto_start with the device id, not retained', (tester) async {
    final (_, mqtt) = await _pump(tester);
    expect(_on(tester, 'auto-start'), isTrue);
    await _tap(tester, 'auto-start');
    final p = _onSeq(mqtt).single;
    expect(p.retain, isFalse);
    final me = _starter(tester).deviceId!;
    expect(me, startsWith('hf-console-'));
    expect(jsonDecode(p.payload), {
      'action': 'auto_start',
      'value': {'requester': me},
    });
    expect(_on75(mqtt), isEmpty, reason: 'the tap itself sends nothing to 75auto');
  });

  testWidgets('a failed AetherSDR step: earlier steps done, AetherSDR failed with the fault, later not started',
      (tester) async {
    final (store, _) = await _pump(tester);
    await _tap(tester, 'auto-start');
    final me = _starter(tester).deviceId!;
    _chain(store, 'running', 'aethersdr', requester: me, ts: 2);
    await _settle(tester);
    expect(_text(tester, 'auto-start-row-aethersdr-state'), '…');
    _chain(store, 'failed', 'aethersdr', fault: 'rigctl: RPRT -1 (no radio)', requester: me, ts: 3);
    await _settle(tester);
    for (final id in ['station', 'bwpc']) {
      expect(_text(tester, 'auto-start-row-$id-state'), 'OK');
    }
    expect(find.descendant(of: _k('auto-start-row-aethersdr'), matching: find.text('AetherSDR')), findsOneWidget);
    expect(_text(tester, 'auto-start-row-aethersdr-state'), 'FAILED');
    expect(_text(tester, 'auto-start-row-aethersdr-note'), 'rigctl: RPRT -1 (no radio)');
    for (final id in ['dax', 'wsjtx', '75auto', 'session']) {
      expect(_text(tester, 'auto-start-row-$id-state'), '—');
    }
    expect(_on(tester, 'auto-start'), isTrue, reason: 'the operator can start again');
  });

  testWidgets('ready for this device: one signed start_session with the picked rule set and mode', (tester) async {
    final (store, mqtt) = await _pump(tester);
    await tester.tap(_k('auto-start-rule-set'));
    await tester.pumpAndSettle();
    await tester.tap(find.text('hunt.gess').last);
    await tester.pumpAndSettle();
    await _tap(tester, 'auto-start-mode-auto');
    await _startToReady(tester, store);

    final sent = _on75(mqtt);
    expect(sent, hasLength(1));
    final m = _signed(sent.single);
    expect(m['action'], 'start_session');
    expect(m['value'], {'rule_set': 'hunt.gess', 'mode': 'auto'});

    // More views and the same ready record send nothing more.
    _live(store, _idleDoc());
    _chain(store, 'ready', '75auto', requester: _starter(tester).deviceId, ts: 3);
    await _settle(tester);
    expect(_on75(mqtt), hasLength(1));
  });

  testWidgets('ready for another requester: no start_session, the progress shows', (tester) async {
    final (store, mqtt) = await _pump(tester);
    await _tap(tester, 'auto-start');
    _chain(store, 'running', 'dax', requester: 'hf-console-elsewhere', ts: 2);
    await _settle(tester);
    _chain(store, 'ready', '75auto', requester: 'hf-console-elsewhere', ts: 3);
    await _settle(tester);
    expect(_on75(mqtt), isEmpty);
    expect(_text(tester, 'auto-start-row-75auto-state'), 'OK');
    expect(_text(tester, 'auto-start-row-session-note'), 'started by another device');
  });

  testWidgets('a retained ready from before the tap does not send start_session', (tester) async {
    final (store, mqtt) = await _pump(tester);
    final me = _starter(tester).deviceId!;
    _chain(store, 'ready', '75auto', requester: me, ts: 1);
    await _settle(tester);
    await _tap(tester, 'auto-start');
    _live(store, _idleDoc());
    await _settle(tester);
    expect(_on75(mqtt), isEmpty);
  });

  testWidgets('ready 31 min after the tap: no start_session, the start expired', (tester) async {
    final (store, mqtt) = await _pump(tester);
    await _tap(tester, 'auto-start');
    final me = _starter(tester).deviceId!;
    _chain(store, 'running', 'bwpc', requester: me, ts: 2);
    await tester.pump(const Duration(minutes: 31));
    expect(_text(tester, 'auto-start-row-session-note'), 'START EXPIRED');
    _live(store, _idleDoc());
    _chain(store, 'ready', '75auto', requester: me, ts: 3);
    await _settle(tester);
    expect(_on75(mqtt), isEmpty);
    expect(_text(tester, 'auto-start-row-session-note'), 'START EXPIRED');
  });

  testWidgets('after start_session the session row follows 75auto to running', (tester) async {
    final (store, mqtt) = await _pump(tester);
    await _startToReady(tester, store);
    final cmdId = (jsonDecode(_on75(mqtt).single.payload) as Map)['cmd_id'];
    final accepted = {'cmd_id': cmdId, 'action': 'start_session', 'result': 'accepted', 'reason': '', 'at_ms': busyNowMs};
    _live(store, _idleDoc(state: 'connecting', lastCmd: accepted));
    await _settle(tester);
    expect(find.byType(AutoStartPanel), findsOneWidget, reason: 'the start is still on its way');
    expect(_text(tester, 'auto-start-row-session-note'), 'connecting');
    _live(store, _idleDoc(state: 'running', lastCmd: accepted));
    await _settle(tester);
    expect(find.byType(AutoStartPanel), findsNothing, reason: 'the session controls take over');
    final row = AutoStarter.of(store, mqtt).rows(DateTime.now()).last;
    expect([row.state, row.note], [AutoStepState.done, 'running']);
  });

  testWidgets('antenna mode storm: Start is off with the mode as the reason', (tester) async {
    final (store, mqtt) = await _pump(tester);
    store.setAntMode('storm');
    await _settle(tester);
    expect(_on(tester, 'auto-start'), isFalse);
    expect(_text(tester, 'auto-start-line'), 'antenna mode storm');
    await _tap(tester, 'auto-start');
    expect(mqtt.publishes, isEmpty);
  });

  testWidgets('storm after the tap: the page checks again and sends no start_session', (tester) async {
    final (store, mqtt) = await _pump(tester);
    await _tap(tester, 'auto-start');
    final me = _starter(tester).deviceId!;
    _chain(store, 'running', 'wsjtx', requester: me, ts: 2);
    store.setAntMode('maintenance');
    _chain(store, 'ready', '75auto', requester: me, ts: 3);
    await _settle(tester);
    expect(_on75(mqtt), isEmpty);
    expect(_text(tester, 'auto-start-row-session-note'), 'antenna mode maintenance');
  });

  testWidgets('start_session refused with busy: the reason shows, END SESSION ends the session, then Start again',
      (tester) async {
    final (store, mqtt) = await _pump(tester);
    // 75auto opened a session of its own (a latched one) while the chain ran.
    await _tap(tester, 'auto-start');
    final me = _starter(tester).deviceId!;
    _chain(store, 'running', '75auto', requester: me, ts: 2);
    _live(store, _idleDoc(state: 'running', latched: true));
    _chain(store, 'ready', '75auto', requester: me, ts: 3);
    await _settle(tester);
    final start = _signed(_on75(mqtt).single);
    expect(start['action'], 'start_session');
    _live(store, _idleDoc(state: 'running', latched: true, lastCmd: {
      'cmd_id': start['cmd_id'],
      'action': 'start_session',
      'result': 'refused',
      'reason': 'busy',
      'at_ms': busyNowMs,
    }));
    await _settle(tester);
    expect(find.byType(AutoStartPanel), findsOneWidget);
    expect(_text(tester, 'auto-start-row-session-note'), 'REFUSED · busy');
    expect(_on(tester, 'auto-start'), isFalse);

    expect(_on(tester, 'auto-end-session'), isTrue);
    await tester.ensureVisible(_k('auto-end-session'));
    await _tap(tester, 'auto-end-session');
    final end = _signed(_on75(mqtt).last);
    expect(end['action'], 'end_session');
    expect(end['value'], isNull);

    _live(store, _idleDoc(state: 'ended'));
    _chain(store, 'failed', 'station', fault: 'none', requester: me, ts: 4);
    await _settle(tester);
    expect(_on(tester, 'auto-start'), isTrue);
    await _tap(tester, 'auto-start');
    expect(_onSeq(mqtt), hasLength(2));
  });

  testWidgets('without a stored key Start is off with the key hint', (tester) async {
    final (_, mqtt) = await _pump(tester, key: false);
    expect(_on(tester, 'auto-start'), isFalse);
    expect(_text(tester, 'auto-start-line'), 'no key');
    await _tap(tester, 'auto-start');
    expect(mqtt.publishes, isEmpty);
  });

  testWidgets('a refusal by powerseq without a step shows its reason', (tester) async {
    final (store, _) = await _pump(tester);
    await _tap(tester, 'auto-start');
    _chain(store, 'failed', '', fault: 'busy: stopping', requester: _starter(tester).deviceId, ts: 2);
    await _settle(tester);
    expect(_text(tester, 'auto-start-line'), 'REFUSED · busy: stopping');
  });

  testWidgets('two taps before powerseq answers: one auto_start, the line says start sent', (tester) async {
    final (_, mqtt) = await _pump(tester);
    await _tap(tester, 'auto-start');
    await _tap(tester, 'auto-start');
    expect(_onSeq(mqtt), hasLength(1));
    expect(_text(tester, 'auto-start-line'), 'start sent');
    expect(_on(tester, 'auto-start'), isFalse);
  });

  testWidgets('auto_start not sent: NOT SENT · tap again, and Start is on again', (tester) async {
    late _FailingMqtt failing;
    final (_, mqtt) = await _pump(tester, mqttOf: (s) => failing = _FailingMqtt(s, '$_seqSlot/cmd'));
    await _tap(tester, 'auto-start');
    expect(_onSeq(mqtt), hasLength(1));
    expect(_text(tester, 'auto-start-line'), 'NOT SENT · tap again');
    expect(_on(tester, 'auto-start'), isTrue);
    failing.fail = false;
    await _tap(tester, 'auto-start');
    expect(_onSeq(mqtt), hasLength(2));
    expect(_text(tester, 'auto-start-line'), 'start sent');
  });

  testWidgets('ready while the 75auto data is not fresh: wait, then one start_session at the live /state',
      (tester) async {
    final (store, mqtt) = await _pump(tester);
    await _tap(tester, 'auto-start');
    final me = _starter(tester).deviceId!;
    _chain(store, 'running', 'station', requester: me, ts: 2);
    await _settle(tester);
    // A reconnect: the retained /state of 75auto is not fresh, the gate is closed.
    store.markConnected(scheduleGraceNotify: false);
    _chain(store, 'ready', '75auto', requester: me, ts: 3);
    await _settle(tester);
    expect(_on75(mqtt), isEmpty);
    expect(_text(tester, 'auto-start-row-session-note'), 'waiting · DATA STALE');
    _live(store, _idleDoc());
    await _settle(tester);
    final sent = _on75(mqtt);
    expect(sent, hasLength(1));
    expect(_signed(sent.single)['action'], 'start_session');
    _live(store, _idleDoc());
    await _settle(tester);
    expect(_on75(mqtt), hasLength(1));
  });

  testWidgets('a station stop after ready (gate closed): no start_session when 75auto turns live', (tester) async {
    final (store, mqtt) = await _pump(tester);
    await _tap(tester, 'auto-start');
    final me = _starter(tester).deviceId!;
    _chain(store, 'running', 'station', requester: me, ts: 2);
    await _settle(tester);
    store.markConnected(scheduleGraceNotify: false);
    _chain(store, 'ready', '75auto', requester: me, ts: 3);
    await _settle(tester);
    expect(_on75(mqtt), isEmpty);
    _chain(store, 'failed', 'station', fault: 'stopped by the operator', requester: me, ts: 4);
    await _settle(tester);
    _live(store, _idleDoc());
    await _settle(tester);
    expect(_on75(mqtt), isEmpty);
  });

  testWidgets('powerseq offline: Start is off with powerseq offline', (tester) async {
    final (store, mqtt) = await _pump(tester);
    store.applyStatus(_seqSlot, 'offline');
    await _settle(tester);
    expect(_on(tester, 'auto-start'), isFalse);
    expect(_text(tester, 'auto-start-line'), 'powerseq offline');
    await _tap(tester, 'auto-start');
    expect(_onSeq(mqtt), isEmpty);
  });

  testWidgets('link down: Start is off with LINK DOWN', (tester) async {
    final (store, mqtt) = await _pump(tester);
    store.markDisconnected();
    await _settle(tester);
    expect(_on(tester, 'auto-start'), isFalse);
    expect(_text(tester, 'auto-start-line'), 'LINK DOWN');
    await _tap(tester, 'auto-start');
    expect(_onSeq(mqtt), isEmpty);
  });

  testWidgets('in portrait the panel sits in the scroll column without an overflow', (tester) async {
    tester.view.physicalSize = const Size(1170, 2532);
    tester.view.devicePixelRatio = 3.0;
    addTearDown(tester.view.reset);
    SharedPreferences.setMockInitialValues({'auto_op_key': _keyHex});
    final store = BusStore()..seedAutoOp(_idleDoc());
    store.applyStatus(_seqSlot, 'online');
    final mqtt = FakeMqttService(store);
    await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const ConsoleScreen()));
    await tester.pump(const Duration(milliseconds: 100));
    await tester.tap(find.widgetWithText(InkWell, '75AUTO'));
    _live(store, _idleDoc());
    await _settle(tester);
    expect(find.byType(AutoStartPanel), findsOneWidget);
    expect(tester.takeException(), isNull);
  });
}
